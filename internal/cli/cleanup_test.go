package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/afero"
	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/remote"
	"github.com/vamos-build/vamos/internal/testenv"
	"github.com/vamos-build/vamos/internal/tracing"
	"github.com/vamos-build/vamos/internal/worker"
)

// heldCoordinator is a coordinator, `vamos coordinator` with a `vamos worker` registered, both in this process,
// behind a front that leaves unanswered every request that one of its holds matches, until the test lets the hold's
// requests through or their client gives up on them.
type heldCoordinator struct {
	url    string         // the front's
	client *remote.Client // the coordinator, as another client reaches it through the front

	mu    sync.Mutex
	holds []*heldRequests

	commandTraced atomic.Bool // whether a command trace has been put, held or not
}

// heldRequests are the requests that one hold of a heldCoordinator matches.
type heldRequests struct {
	match   func(r *http.Request, body []byte) bool
	arrived chan struct{} // closed once the first request it matches arrives
	at      time.Time     // when that was, set before arrived is closed
	gaveUp  chan struct{} // closed once the client of a request it holds gives up on it
	gaveAt  time.Time     // when that was, set before gaveUp is closed
	giving  sync.Once
	once    sync.Once
	through chan struct{} // closed once the test lets its requests through
}

// newHeldCoordinator starts a heldCoordinator, until the test ends, holding nothing yet: the coordinator keeps the
// layers its worker builds in a registry of the test's own, and each builds in stores of its own.
func newHeldCoordinator(t *testing.T) *heldCoordinator {
	t.Helper()
	t.Setenv(worker.ChildEnv, "1") // the worker runs this test binary as its attempts' process
	_, registry := insecureRegistry(t, nil)
	// Before the commands' cleanup is registered, so that what they use is removed only once they have ended.
	coordinatorStores, cacheDir, workerStores := testenv.PrivateStores(t), t.TempDir(), testenv.PrivateStores(t)
	cgroups := testenv.CgroupRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	var coordinator, registered *servingCommand
	t.Cleanup(func() { // registered first, so that it runs last, once the front has closed
		cancel()
		for _, s := range []*servingCommand{registered, coordinator} {
			if s != nil {
				s.wait(t)
			}
		}
	})
	coordinator = serveCommand(ctx, cgroups, coordinatorStores, "coordinator", "--listen", "127.0.0.1:0",
		"--cache-dir", cacheDir, "--cache-repository", registry+"/vamos-cache", "--lease", "2s")
	address := "http://" + coordinator.waitFor(t, "Coordinating builds at ")
	registered = serveCommand(ctx, cgroups, workerStores, "worker", "--coordinator", address, "--capacity", "2")
	registered.waitFor(t, "worker: registered as ")

	backend, err := url.Parse(address)
	must(t, err)
	c := &heldCoordinator{}
	proxy := &httputil.ReverseProxy{FlushInterval: -1, // so that events stream
		Rewrite: func(pr *httputil.ProxyRequest) { pr.SetURL(backend) }}
	front := httptest.NewServer(c.middleware(proxy))
	t.Cleanup(front.Close)
	// Before the front closes, since a request left unanswered would keep Close waiting.
	t.Cleanup(c.letAllThrough)
	c.url = front.URL
	c.client, err = remote.NewClient(front.URL)
	must(t, err)
	return c
}

// servingCommand is a vamos command that serves, run in this process until its context ends.
type servingCommand struct {
	name   string
	output syncBuffer
	ended  chan struct{} // closed once it has returned err
	err    error
}

// serveCommand runs vamos with args, a command that serves, on store, until ctx ends.
func serveCommand(ctx context.Context, cgroups string, store *testenv.Stores, args ...string) *servingCommand {
	s := &servingCommand{name: "vamos " + args[0], ended: make(chan struct{})}
	cmd := NewRootCommand(Config{Fs: afero.NewMemMapFs(), Store: &store.Options, // which reads no project
		CgroupRoot: cgroups})
	cmd.SetOut(&s.output)
	cmd.SetErr(&s.output)
	cmd.SetArgs(args)
	go func() {
		defer close(s.ended)
		s.err = cmd.ExecuteContext(ctx)
	}()
	return s
}

// waitFor waits until the command has printed a line starting with prefix, and returns the rest of it,
// failing the test if it ends first, or prints none within 20 seconds.
func (s *servingCommand) waitFor(t *testing.T, prefix string) string {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		select {
		case <-s.ended:
			t.Fatalf("%s ended with %v before it printed %q:\n%s", s.name, s.err, prefix, s.output.String())
		default:
		}
		for line := range strings.Lines(s.output.String()) {
			if rest, ok := strings.CutPrefix(line, prefix); ok && strings.HasSuffix(rest, "\n") {
				return strings.TrimSuffix(rest, "\n")
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s printed no %q:\n%s", s.name, prefix, s.output.String())
		}
	}
}

// wait waits for the command to end, logging what it printed if it ended with an error.
func (s *servingCommand) wait(t *testing.T) {
	select {
	case <-s.ended:
		if s.err != nil {
			t.Logf("%s ended with %v:\n%s", s.name, s.err, s.output.String())
		}
	case <-time.After(time.Minute):
		t.Errorf("%s didn't end within a minute of being interrupted", s.name)
	}
}

// syncBuffer is a buffer that a command writes to while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// hold makes the coordinator leave unanswered the requests that match says, given each request and its body,
// from now until the hold's requests are let through.
func (c *heldCoordinator) hold(match func(r *http.Request, body []byte) bool) *heldRequests {
	h := &heldRequests{match: match, arrived: make(chan struct{}), gaveUp: make(chan struct{}),
		through: make(chan struct{})}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.holds = append(c.holds, h)
	return h
}

// letAllThrough lets every hold's requests through.
func (c *heldCoordinator) letAllThrough() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, h := range c.holds {
		h.letThrough()
	}
}

// middleware holds each request that a hold matches until the hold lets it through or its client gives up.
func (c *heldCoordinator) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if isCommandTrace(r, body) {
			c.commandTraced.Store(true)
		}
		c.mu.Lock()
		holds := append([]*heldRequests(nil), c.holds...)
		c.mu.Unlock()
		for _, h := range holds {
			if !h.match(r, body) {
				continue
			}
			h.once.Do(func() {
				h.at = time.Now()
				close(h.arrived)
			})
			select {
			case <-h.through:
			case <-r.Context().Done():
				h.giving.Do(func() {
					h.gaveAt = time.Now()
					close(h.gaveUp)
				})
				return // the client gave up, and hears nothing
			}
		}
		next.ServeHTTP(rw, r)
	})
}

// build runs `vamos build app` of a project whose BUILD file is nonceLayer's on the coordinator, writing --trace's
// file to out, until it ends, which the returned channel says once its result is set. The test waits for it as it
// ends, after letting every hold's requests through, so that the command doesn't outlive what it uses.
func (c *heldCoordinator) build(t *testing.T, out string, r *result) <-chan struct{} {
	t.Helper()
	dir := project(t, map[string]string{"BUILD": nonceLayer()})
	done := make(chan struct{})
	go func() {
		defer close(done)
		*r = vamos(t, dir, "build", "app", "-C", projectDir, "--coordinator", c.url, "--trace", out)
	}()
	t.Cleanup(func() {
		c.letAllThrough()
		select {
		case <-done:
		case <-time.After(time.Minute):
			t.Error("the build didn't end within a minute of the test")
		}
	})
	return done
}

// letThrough answers the hold's requests, those waiting and those still to come.
func (h *heldRequests) letThrough() {
	select {
	case <-h.through:
	default:
		close(h.through)
	}
}

// waitUntilArrived waits until a request that the hold matches has arrived, and returns when it did, failing the
// test if none has by the time done is closed or a minute has passed.
func (h *heldRequests) waitUntilArrived(t *testing.T, done <-chan struct{}) time.Time {
	t.Helper()
	select {
	case <-h.arrived:
	case <-done:
		select {
		case <-h.arrived: // and the command then ended, at its bound, before this goroutine saw it
		default:
			t.Fatal("the command ended before the coordinator was sent the request that it holds")
		}
	case <-time.After(time.Minute):
		t.Fatal("the coordinator wasn't sent the request that it holds")
	}
	return h.at
}

// isCommandTrace reports whether r puts a command's trace into the coordinator's content store.
func isCommandTrace(r *http.Request, body []byte) bool {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/blobs" { // remote's pathBlobs
		return false
	}
	h, err := tracing.ReadHeader(body)
	return err == nil && h.Kind == api.KindCommand
}

// storingTheCommandTrace matches the request that stores a command's trace.
func storingTheCommandTrace(r *http.Request, body []byte) bool {
	return isCommandTrace(r, body)
}

// readingTracesOnceStored matches, once c has been sent a command's trace, a batch read of blobs, which only
// assembling --trace's file then sends.
func (c *heldCoordinator) readingTracesOnceStored(r *http.Request, _ []byte) bool {
	return c.commandTraced.Load() && r.Method == http.MethodPost && r.URL.Path == "/v1/blobs/read" // pathReadMany
}

// releasingAJob matches a client's release of a job.
func releasingAJob(r *http.Request, _ []byte) bool {
	return r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/jobs/") &&
		strings.HasSuffix(r.URL.Path, "/release") // remote's pathRelease
}

// shorten shortens bound, one of the bounds on a command's cleanup, to d, for the rest of the test, leaving the
// others as they are, so that only the step held ends at its bound.
func shorten(t *testing.T, bound *time.Duration, d time.Duration) {
	t.Helper()
	was := *bound
	*bound = d
	t.Cleanup(func() { *bound = was })
}

// nonceLayer is a BUILD file whose layer app must run, being new to the run, and needs no image pulled.
func nonceLayer() string {
	return `layer(name="app", base="scratch", steps=[env(NONCE="` + newNonce() + `")])` + "\n"
}

// TestCleanupStepsAreBounded checks that each step of a command's cleanup once its jobs end, storing its trace,
// assembling --trace's file and releasing its jobs, ends at its bound against a coordinator that stops answering it,
// warning or noting in the trace what it couldn't do, that the command's outcome is unchanged, and that a trace's
// step ending at its bound still leaves the jobs released.
func TestCleanupStepsAreBounded(t *testing.T) {
	requireBuildah(t)
	const bound = 500 * time.Millisecond
	for _, tc := range []struct {
		name    string
		hold    func(c *heldCoordinator) func(*http.Request, []byte) bool
		bound   *time.Duration // the held step's, which the test shortens
		printed bool           // whether it prints its trace's digest
		warning string         // on stderr
		trace   string         // the reason a "trace incomplete" instant in --trace's file gives, or "" for none
		// Whether the command released its jobs, after its trace's step ended at its bound, rather than being the
		// step held.
		released bool
	}{
		{
			name:     "storing the command trace",
			hold:     func(*heldCoordinator) func(*http.Request, []byte) bool { return storingTheCommandTrace },
			bound:    &traceStoreTimeout,
			warning:  "vamos: warning: the command's trace couldn't be stored: ",
			released: true,
		},
		{
			name:     "assembling --trace's file",
			hold:     func(c *heldCoordinator) func(*http.Request, []byte) bool { return c.readingTracesOnceStored },
			bound:    &traceAssembleTimeout,
			printed:  true,
			trace:    "a job's trace can't be read",
			released: true,
		},
		{
			name:    "releasing the jobs",
			hold:    func(*heldCoordinator) func(*http.Request, []byte) bool { return releasingAJob },
			bound:   &releaseTimeout,
			printed: true,
			warning: "vamos: warning: releasing the build on the executor: ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shorten(t, tc.bound, bound)
			c := newHeldCoordinator(t)
			held := c.hold(tc.hold(c))
			out := filepath.Join(t.TempDir(), "trace.json")
			var r result
			done := c.build(t, out, &r)
			arrived := held.waitUntilArrived(t, done)
			select {
			case <-done:
			case <-time.After(traceStoreTimeout + traceAssembleTimeout + releaseTimeout + time.Minute): // every bound
				t.Fatalf("the command didn't end once %s stopped being answered", tc.name)
			}
			// The command gave up on the held step within 5 seconds, far sooner than its real bound, which only the
			// shortened one explains; the steps after it keep theirs, and aren't timed.
			select {
			case <-held.gaveUp:
				if took := held.gaveAt.Sub(arrived); took > 5*time.Second {
					t.Errorf("the command gave up on %s %v after it was held; want it bounded", tc.name, took)
				}
			case <-time.After(10 * time.Second): // the front hears it as the connection ends, maybe after the command
				t.Errorf("the command ended without giving up on %s", tc.name)
			}
			r.ran(t)
			if printed := strings.Contains(r.stderr, "vamos: trace sha256:"); printed != tc.printed {
				t.Errorf("printed its trace's digest = %v, want %v; stderr:\n%s", printed, tc.printed, r.stderr)
			}
			if tc.warning != "" && !strings.Contains(r.stderr, tc.warning) {
				t.Errorf("stderr = %q, want the warning %q", r.stderr, tc.warning)
			}
			data, err := os.ReadFile(out)
			must(t, err)
			var file struct {
				TraceEvents []struct {
					Name string         `json:"name"`
					Args map[string]any `json:"args"`
				} `json:"traceEvents"`
			}
			must(t, json.Unmarshal(data, &file))
			var reasons []string
			names := map[string]bool{}
			for _, e := range file.TraceEvents {
				names[e.Name] = true
				if e.Name == "trace incomplete" {
					reasons = append(reasons, fmt.Sprint(e.Args["reason"]))
				}
			}
			if !names[`layer "app"`] && tc.trace == "" {
				t.Errorf("--trace's file names %v; want the executor's layer \"app\"", names)
			}
			if tc.trace != "" && !slices.Contains(reasons, tc.trace) {
				t.Errorf("--trace's file says it is incomplete because %q; want %q", reasons, tc.trace)
			}
			if tc.released {
				c.prune(t, pruneSettled) // refused while its jobs were held
			}
		})
	}
}

// TestPruneWaitsForTheCommandTrace checks that a coordinator refuses a prune while a command stores its trace and
// assembles --trace's file, since its jobs are held until then, and runs one once the command has released them.
func TestPruneWaitsForTheCommandTrace(t *testing.T) {
	requireBuildah(t)
	c := newHeldCoordinator(t)
	// A coordinator refuses a prune as settling until a lease has passed since it started; prune its empty cache
	// first, so that the prunes below are refused, or run, for the command's jobs alone.
	c.prune(t, pruneSettled)
	storing := c.hold(storingTheCommandTrace)
	assembling := c.hold(c.readingTracesOnceStored)
	var r result
	done := c.build(t, filepath.Join(t.TempDir(), "trace.json"), &r)
	refused := func(while string) {
		t.Helper()
		id, err := c.client.Prune(t.Context())
		if err == nil || !strings.Contains(err.Error(), "other jobs are running") {
			t.Errorf("a prune while the command %s = %q, %v; want it refused while its jobs are held", while, id, err)
		}
	}
	held := func(h *heldRequests, while string) {
		t.Helper()
		h.waitUntilArrived(t, done)
		select {
		case <-done:
			t.Fatalf("the command ended while it %s, before its request was let through", while)
		default:
		}
		refused(while)
	}
	held(storing, "stores its trace")
	storing.letThrough()
	held(assembling, "assembles --trace's file")
	assembling.letThrough()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("the command didn't end once its requests were let through")
	}
	r.ran(t)
	if !strings.Contains(r.stderr, "vamos: trace sha256:") {
		t.Errorf("stderr = %q, want its trace's digest", r.stderr)
	}
	c.prune(t, func(err error) bool { return false })
}

// pruneSettled retries a prune that a coordinator refuses as settling.
func pruneSettled(err error) bool { return errors.Is(err, api.ErrSettling) }

// prune prunes the coordinator, retrying for up to 10 seconds while retry says of its refusal, and waits until the
// prune has ended, failing the test unless it is accepted and reports what it pruned.
func (c *heldCoordinator) prune(t *testing.T, retry func(error) bool) {
	t.Helper()
	var id api.JobID
	var err error
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if id, err = c.client.Prune(t.Context()); err == nil || !retry(err) || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatalf("a prune = %v; want it run", err)
	}
	var last api.Event
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute) // a prune of a test's cache is quick
	defer cancel()
	must(t, c.client.Watch(ctx, id, 0, func(e api.Event) error {
		last = e
		return nil
	}))
	if last.Kind != api.EventDone || last.Outcome == nil || last.Outcome.Pruned == nil {
		t.Fatalf("the prune ended with %+v; want what it pruned", last)
	}
	must(t, c.client.Release(t.Context(), id))
}
