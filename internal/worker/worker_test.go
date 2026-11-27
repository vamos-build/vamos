package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/api/apitest"
	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/coord"
	"github.com/vamos-build/vamos/internal/graph"
	"github.com/vamos-build/vamos/internal/remote"
	"github.com/vamos-build/vamos/internal/run"
	"github.com/vamos-build/vamos/internal/testenv"
	"github.com/vamos-build/vamos/internal/tracing"
	"github.com/vamos-build/vamos/internal/transfer"

	"github.com/containerd/platforms"
	"github.com/google/go-containerregistry/pkg/registry"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

func TestMain(m *testing.M) {
	testenv.Main(m, func() bool {
		if path := os.Getenv(slowRemovalEnv); path != "" && strings.HasPrefix(os.Getenv(ChildEnv), "{") {
			slowRemovals(path) // in an attempt's own process alone, as ChildFromEnv runs one
		}
		return ChildFromEnv()
	})
}

// slowRemovals makes the attempt that this process may run remove its temporary tool caches slowly, naming each
// directory it removed in the file path, a line each, with how long the removal took, in nanoseconds, before it.
func slowRemovals(path string) {
	run.SlowToolCacheRemoval(slowRemoval, func(dir string, took time.Duration) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			_, err = fmt.Fprintf(f, "%d %s\n", took, dir)
			err = errors.Join(err, f.Close())
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "recording the removal of %s: %v\n", dir, err)
		}
	})
}

// slowRemovalEnv, in an attempt's environment, makes the attempt remove its temporary tool caches slowly, taking
// slowRemoval, three of the fixture's leases, and name each directory it removed in the file it names.
const slowRemovalEnv = "VAMOS_TEST_SLOW_TOOL_CACHE_REMOVAL"

// slowRemoval is how long an attempt given slowRemovalEnv waits before it removes its temporary tool caches.
const slowRemoval = 3 * testLease

// fixture is a coordinator with stores in memory, served over HTTP, with a cache registry of its own.
type fixture struct {
	c        *coord.Coordinator
	m        *cache.Memory
	server   *httptest.Server
	registry string // the cache repository
}

// testLease is the lease of the fixture's coordinator: short, so that tests of its expiry are quick.
const testLease = 2 * time.Second

// newFixture returns a coordinator with short leases, whose workers push layers to an in-process registry.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixtureWithLease(t, testLease)
}

// newFixtureWithLease returns a coordinator whose attempts' leases last lease, with heartbeats at a sixth of it,
// configured further by configure.
func newFixtureWithLease(t *testing.T, lease time.Duration, configure ...func(*coord.Config)) *fixture {
	t.Helper()
	testenv.RequireBuildah(t)
	reg := httptest.NewServer(untagging(registry.New(registry.Logger(log.New(io.Discard, "", 0)))))
	t.Cleanup(reg.Close)
	host := strings.TrimPrefix(reg.URL, "http://")
	testenv.InsecureRegistry(t, host)
	sys, err := transfer.SystemContext()
	testenv.Must(t, err)
	m := cache.NewMemory()
	cfg := coord.Config{
		Lease:           lease,
		Heartbeat:       lease / 6,
		CacheRepository: host + "/vamos-cache",
		Images:          &coord.Registry{Sys: sys},
	}
	for _, c := range configure {
		c(&cfg)
	}
	c, err := coord.New(m, m, cfg)
	testenv.Must(t, err)
	t.Cleanup(func() { c.Close() })
	server := httptest.NewServer((&remote.Server{Executor: c, Content: m, Workers: c.Workers()}).Handler())
	t.Cleanup(server.Close)
	return &fixture{c: c, m: m, server: server, registry: host + "/vamos-cache"}
}

// startWorker runs a worker of the fixture's coordinator, in a store of its own, until the test ends;
// each of configure changes its configuration first.
func startWorker(t *testing.T, f *fixture, configure ...func(*Config)) (*Worker, *testenv.Stores) {
	t.Helper()
	stores := testenv.PrivateStores(t)
	cfg := Config{
		Coordinator: f.server.URL,
		Store:       stores.Options,
		Capacity:    2,
		Child:       TestChild(stores.Options),
		CgroupRoot:  testenv.CgroupRoot(t),
		Log:         testWriter{t},
	}
	for _, c := range configure {
		c(&cfg)
	}
	w, err := New(cfg)
	testenv.Must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Run(ctx) // it ends when the test cancels ctx
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	// The worker registers in its own time; the coordinator offers its platform once it has.
	for start := time.Now(); ; {
		caps, err := f.c.Capabilities(context.Background())
		testenv.Must(t, err)
		if caps.Offers(run.HostPlatform) {
			break
		}
		if time.Since(start) > 10*time.Second {
			t.Fatal("the worker never registered")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return w, stores
}

// untagging makes the in-process registry h delete a manifest's tags along with the manifest when it is deleted by
// digest, as Distribution does and as the coordinator's prune relies on; the in-process registry alone keeps the tag.
func untagging(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo, ref, isManifest := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v2/"), "/manifests/")
		if r.Method == http.MethodDelete && isManifest && strings.HasPrefix(ref, "sha256:") {
			for _, tag := range tagsOf(h, repo) {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/v2/"+repo+"/manifests/"+tag, nil))
				if rec.Header().Get("Docker-Content-Digest") == ref {
					h.ServeHTTP(httptest.NewRecorder(),
						httptest.NewRequest(http.MethodDelete, "/v2/"+repo+"/manifests/"+tag, nil))
				}
			}
		}
		h.ServeHTTP(w, r)
	})
}

// tagsOf lists the tags of repo at the registry h.
func tagsOf(h http.Handler, repo string) []string {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/"+repo+"/tags/list", nil))
	var list struct {
		Tags []string `json:"tags"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list) // an error, for a repository it doesn't know, lists no tags
	return list.Tags
}

// testWriter logs to the test.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// layerGraph stores a graph whose root is a layer on scratch adding a file with content, and returns its request.
func layerGraph(t *testing.T, f *fixture, content string, steps ...graph.Step) api.GraphRequest {
	t.Helper()
	ctx := t.Context()
	file, err := f.m.PutBytes(ctx, []byte(content))
	testenv.Must(t, err)
	tree, err := f.m.PutBytes(ctx, (&cache.Tree{Entries: []cache.TreeEntry{
		{Path: "a.txt", Type: cache.EntryFile, Mode: 0o644, Blob: &file},
	}}).Encode())
	testenv.Must(t, err)
	from := graph.Ref(0)
	platform := platforms.Format(run.HostPlatform)
	g := &graph.Graph{Version: graph.Version, ExecPlatform: platform, Roots: []graph.Ref{1}, Nodes: []graph.Node{
		{Label: "src", Desc: `glob "src"`, Source: &graph.Source{Tree: tree}},
		{Label: "app", Desc: `layer "app"`, Layer: &graph.Layer{Base: graph.Scratch, Platform: platform,
			Steps: append([]graph.Step{{Op: graph.OpAdd, Display: `add("src", "/data")`, From: &from, Dest: "/data"}},
				steps...)}},
	}}
	testenv.Must(t, g.Validate())
	stored, err := f.m.PutBytes(ctx, graph.Encode(g))
	testenv.Must(t, err)
	return api.GraphRequest{Graph: stored, Version: graph.Version, Platform: run.HostPlatform}
}

// runJob submits req and watches it to its end, returning its outcome and the statuses reported.
func runJob(t *testing.T, c *coord.Coordinator, req api.GraphRequest) (api.Outcome, []string) {
	t.Helper()
	ctx := t.Context()
	id, err := c.Submit(ctx, api.Request{Graph: &req})
	testenv.Must(t, err)
	var statuses []string
	testenv.Must(t, c.Watch(ctx, id, 0, func(e api.Event) error {
		if e.Kind == api.EventStatus {
			statuses = append(statuses, e.Status)
		}
		return nil
	}))
	outcome, err := c.Result(ctx, id)
	testenv.Must(t, err)
	testenv.Must(t, c.Release(ctx, id)) // as a client does once it has delivered: a held build keeps a prune out
	return outcome, statuses
}

// TestWorkerBuilds checks that a worker builds a layer the coordinator assigns, in a subprocess, pushes it to the
// cache repository before reporting, so that the result's location is there, and that the next build is a hit that
// the coordinator checks against the registry without asking the worker.
func TestWorkerBuilds(t *testing.T) {
	f := newFixture(t)
	startWorker(t, f)
	req := layerGraph(t, f, "built on a worker")
	outcome, statuses := runJob(t, f.c, req)
	if outcome.Failure != nil {
		t.Fatalf("outcome = %+v", outcome.Failure)
	}
	res := outcome.Results[0]
	var registry []cache.Location
	for _, l := range res.Locations {
		if l.Kind == cache.LocationRegistry {
			registry = append(registry, l)
		}
	}
	if len(registry) != 1 || registry[0].Reference != f.registry+":"+res.Image {
		t.Errorf("locations = %+v, want the image at %s:%s", res.Locations, f.registry, res.Image)
	}
	if !strings.Contains(strings.Join(statuses, ","), "building") {
		t.Errorf("statuses = %q, want the layer building", statuses)
	}
	// The attempt's subprocess used CPU in its cgroup, which its try's span says, in seconds.
	if ts := tries(t, f, outcome); len(ts) != 1 || ts[0].Args["cpu"] == nil {
		t.Errorf("the build's tries = %+v; want one, saying the CPU it used", ts)
	} else if cpu, ok := ts[0].Args["cpu"].(float64); !ok || cpu <= 0 {
		t.Errorf("the try used %v seconds of CPU; want some", ts[0].Args["cpu"])
	}
	checker := &coord.Registry{Sys: mustSys(t)}
	if problem, err := checker.CheckImage(t.Context(), res.Image, registry[0]); problem != nil || err != nil {
		t.Errorf("the image at its location = %v, %v; want it there", problem, err)
	}

	outcome, statuses = runJob(t, f.c, req)
	if outcome.Failure != nil || !strings.Contains(strings.Join(statuses, ","), "cached") {
		t.Errorf("the second build = %+v with statuses %q; want the layer cached", outcome.Failure, statuses)
	}
}

// mustSys returns the registry settings of this process's environment.
func mustSys(t *testing.T) *types.SystemContext {
	t.Helper()
	sys, err := transfer.SystemContext()
	testenv.Must(t, err)
	return sys
}

// TestWorkerFailureCrosses checks that a step that fails on the worker reaches the client as the same failure a
// local build reports: the step, its message and its log.
func TestWorkerFailureCrosses(t *testing.T) {
	f := newFixture(t)
	startWorker(t, f)
	req := layerGraph(t, f, "failing", graph.Step{Op: graph.OpRun, Display: `run(["/nope"])`, Argv: []string{"/nope"}})
	outcome, _ := runJob(t, f.c, req)
	if outcome.Failure == nil || outcome.Failure.Step == nil || *outcome.Failure.Step != 1 ||
		len(outcome.Failure.Nodes) != 1 || outcome.Failure.Nodes[0] != 1 {
		t.Fatalf("outcome = %+v, want the layer's second step failed", outcome.Failure)
	}
	if outcome.Failure.Log == nil {
		t.Error("the failure carries no log")
	}
}

// TestWorkerKillsOnCancel checks that cancelling a build kills the attempt's subprocess and everything it started,
// within seconds, and removes the attempt's container from the worker's store.
func TestWorkerKillsOnCancel(t *testing.T) {
	f := newFixture(t)
	w, stores := startWorker(t, f)
	ctx := t.Context()
	// The step needs a shell to sleep in, so the base is busybox from Docker Hub, through testenv.Main's mirror;
	// one sleep leaves the process group with setsid, as a daemon would, and must die all the same.
	platform := platforms.Format(run.HostPlatform)
	g := &graph.Graph{Version: graph.Version, ExecPlatform: platform, Roots: []graph.Ref{1}, Nodes: []graph.Node{
		{Label: "busybox", Desc: `pull "busybox"`, Pull: &graph.Pull{Image: "docker.io/library/busybox:1.36",
			Platform: platform, Digest: testenv.BusyboxIndex}},
		{Label: "app", Desc: `layer "app"`, Layer: &graph.Layer{Base: 0, Platform: platform, Steps: []graph.Step{
			{Op: graph.OpRun, Display: `run(["sh", "-c", "..."])`, Argv: []string{"sh", "-c",
				"setsid sleep 300 & sleep 300"}},
		}, ToolCaches: []graph.ToolCache{{Path: "/cache", Label: "//:tool"}}}}, // whose empty directory goes too
	}}
	testenv.Must(t, g.Validate())
	stored, err := f.m.PutBytes(ctx, graph.Encode(g))
	testenv.Must(t, err)
	id, err := f.c.Submit(ctx, api.Request{Graph: &api.GraphRequest{Graph: stored, Version: graph.Version,
		Platform: run.HostPlatform}})
	testenv.Must(t, err)
	// Wait for the layer to run: its attempt's cgroup holds processes. A job that ends first, as when its pull
	// fails, fails the test with the job's own failure, rather than leaving it to wait for the deadline.
	ended := make(chan error, 1)
	go func() { ended <- f.c.Watch(ctx, id, 0, func(api.Event) error { return nil }) }()
	var attempt string
	deadline := time.Now().Add(3 * time.Minute)
	for attempt == "" {
		if time.Now().After(deadline) {
			t.Fatal("the layer never started running")
		}
		select {
		case err := <-ended:
			outcome, rerr := f.c.Result(ctx, id)
			t.Fatalf("watching the job ended, with %v, before its layer ran; its result is %v, with failure %+v", err,
				rerr, outcome.Failure)
		case <-time.After(100 * time.Millisecond):
		}
		ids, _ := w.contain.leftovers()
		for _, id := range ids {
			procs, _ := os.ReadFile(filepath.Join(w.contain.path(id), "cgroup.procs"))
			if strings.Count(string(procs), "\n") >= 3 { // the attempt, sh and the sleeps
				attempt = id
			}
		}
	}
	start := time.Now()
	testenv.Must(t, f.c.Cancel(ctx, id))
	testenv.Must(t, <-ended)
	if outcome, err := f.c.Result(ctx, id); err != nil || !outcome.Cancelled {
		t.Errorf("outcome = %+v, %v; want it cancelled", outcome, err)
	}
	for {
		if _, err := os.Stat(w.contain.path(attempt)); errors.Is(err, os.ErrNotExist) {
			break
		}
		if time.Since(start) > 30*time.Second {
			procs, _ := os.ReadFile(filepath.Join(w.contain.path(attempt), "cgroup.procs"))
			t.Fatalf("the attempt's cgroup still holds processes %s after %s", procs, time.Since(start))
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("the attempt's processes were gone %s after the cancel", time.Since(start))
	// The worker removes the attempt's container once its processes are gone and their cgroup removed, so the
	// container may outlast the cgroup for a moment.
	for {
		remaining := attemptContainers(t, stores)
		if len(remaining) == 0 {
			break
		}
		if time.Since(start) > 30*time.Second {
			t.Fatalf("containers %q remain %s after the kill", remaining, time.Since(start))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// attemptContainers returns the names of the attempts' containers in stores.
func attemptContainers(t *testing.T, stores *testenv.Stores) []string {
	t.Helper()
	var names []string
	testenv.Must(t, stores.WithStore(func(store storage.Store) error {
		containers, err := store.Containers()
		for _, c := range containers {
			for _, name := range c.Names {
				if strings.HasPrefix(name, attemptPrefix) {
					names = append(names, name)
				}
			}
		}
		return err
	}))
	return names
}

// attemptPrefix is how the run package names attempts' containers.
const attemptPrefix = "vamos-attempt-"

// TestContract runs the executor contract on a coordinator with one worker, through remote.Client, as a client
// sees it: the coordinator exports nothing, so the contract checks locations and the refusal instead.
func TestContract(t *testing.T) {
	testenv.RequireBuildah(t)
	contents := map[api.Executor]*cache.Memory{}
	var mu sync.Mutex
	apitest.Contract(t, apitest.Fixture{
		New: func(t *testing.T) api.Executor {
			t.Helper()
			// A coordinator refuses a prune for a lease after it starts, which the contract's prune doesn't expect,
			// so the lease is short and waited out.
			const lease = 600 * time.Millisecond
			f := newFixtureWithLease(t, lease)
			f.c.RetainEvents(8)
			startWorker(t, f)
			time.Sleep(lease)
			client, err := remote.NewClient(f.server.URL)
			testenv.Must(t, err)
			t.Cleanup(func() { client.Close() })
			mu.Lock()
			contents[client] = f.m
			mu.Unlock()
			return client
		},
		Put: func(t *testing.T, ex api.Executor, data []byte) cache.Blob {
			t.Helper()
			mu.Lock()
			m := contents[ex]
			mu.Unlock()
			b, err := m.PutBytes(t.Context(), data)
			testenv.Must(t, err)
			return b
		},
		Missing: func(t *testing.T, ex api.Executor, blobs []cache.Blob) []cache.Blob {
			t.Helper()
			mu.Lock()
			m := contents[ex]
			mu.Unlock()
			missing, err := m.Missing(t.Context(), blobs)
			testenv.Must(t, err)
			return missing
		},
		Read: func(t *testing.T, ex api.Executor, b cache.Blob) []byte {
			t.Helper()
			mu.Lock()
			m := contents[ex]
			mu.Unlock()
			data, err := m.Read(t.Context(), b, tracing.MaxTraceSize)
			testenv.Must(t, err)
			return data
		},
		Platform:      run.HostPlatform,
		Registry:      registry.New(registry.Logger(log.New(io.Discard, "", 0))),
		Insecure:      testenv.InsecureRegistry,
		Retention:     8,
		Buffered:      true,
		RequireBuilds: testenv.RequireBuildah,
		NoExport:      true,
	})
}

// TestWorkerFencesItself checks that an attempt whose heartbeats get no answer is killed before its lease would
// expire at the coordinator, on the worker's own clock, so that the coordinator may retry its work without the two
// running at once: a proxy between the worker and the coordinator hangs every heartbeat once the attempt runs.
func TestWorkerFencesItself(t *testing.T) {
	f := newFixture(t)
	var hanging atomic.Bool
	upstream := httputil.NewSingleHostReverseProxy(mustURL(t, f.server.URL))
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hanging.Load() && strings.HasSuffix(r.URL.Path, "/heartbeat") {
			// The body is read first: the server notices the client leaving only once it has read the request.
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			return
		}
		upstream.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	f.server = proxy
	reader := sdkmetric.NewManualReader()
	w, stores := startWorker(t, f, metered(t, reader))
	ctx := t.Context()
	platform := platforms.Format(run.HostPlatform)
	g := &graph.Graph{Version: graph.Version, ExecPlatform: platform, Roots: []graph.Ref{1}, Nodes: []graph.Node{
		{Label: "busybox", Desc: `pull "busybox"`, Pull: &graph.Pull{Image: "docker.io/library/busybox:1.36",
			Platform: platform, Digest: testenv.BusyboxIndex}},
		{Label: "app", Desc: `layer "app"`, Layer: &graph.Layer{Base: 0, Platform: platform, Steps: []graph.Step{
			{Op: graph.OpRun, Display: `run(["sleep", "300"])`, Argv: []string{"sleep", "300"}},
		}}},
	}}
	testenv.Must(t, g.Validate())
	stored, err := f.m.PutBytes(ctx, graph.Encode(g))
	testenv.Must(t, err)
	id, err := f.c.Submit(ctx, api.Request{Graph: &api.GraphRequest{Graph: stored, Version: graph.Version,
		Platform: run.HostPlatform}})
	testenv.Must(t, err)
	// The layer's attempt is the one to fence: the pull's has ended once the layer is building.
	wctx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	building := make(chan struct{})
	go func() {
		// It ends with fn's context.Canceled once the layer is building.
		_ = f.c.Watch(wctx, id, 0, func(e api.Event) error {
			if e.Kind == api.EventStatus && e.Status == "building" && len(e.Nodes) == 1 && e.Nodes[0] == 1 {
				close(building)
				return context.Canceled
			}
			return nil
		})
	}()
	select {
	case <-building:
	case <-time.After(2 * time.Minute):
		t.Fatal("the layer never started building")
	}
	attempt, done := waitForContainedAttempt(t, w, stores)
	hanging.Store(true)
	fenced := time.Now()
	// The fence is at 80% of the lease; the kill and cleanup follow. The coordinator's view of the lease,
	// a lease from its last renewal, is what the attempt must not outlive.
	for time.Since(fenced) < testLease {
		if _, err := os.Stat(w.contain.path(attempt)); errors.Is(err, os.ErrNotExist) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(w.contain.path(attempt)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("attempt %s's cgroup still exists %s after its heartbeats stopped answering; want it killed within "+
			"the lease", attempt, time.Since(fenced).Round(time.Millisecond))
	}
	// The worker removes the attempt's container once it has killed the attempt, and only then forgets it.
	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		t.Fatalf("the worker still hasn't finished with attempt %s, killing it, removing its container and "+
			"reporting it, two minutes after its kill", attempt)
	}
	leftover, err := run.LeftoverAttempts(stores.Options)
	testenv.Must(t, err)
	if slices.Contains(leftover, attempt) {
		t.Errorf("attempt %s's container is still in the store after the worker finished with it", attempt)
	}
	if n := counted(t, reader, "vamos.worker.fenced"); n != 1 {
		t.Errorf("the worker counted %d fenced attempts; want the one", n)
	}
	testenv.Must(t, f.c.Cancel(ctx, id))
}

// TestWorkerRenewsBeforeStarting checks that an assignment delivered after its lease has passed at the coordinator,
// which started the lease before sending it, never starts its process: the worker renews before starting it, and the
// coordinator, which has tried the work again, refuses the renewal.
func TestWorkerRenewsBeforeStarting(t *testing.T) {
	f := newFixture(t)
	upstream := httputil.NewSingleHostReverseProxy(mustURL(t, f.server.URL))
	var delayed atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/assignments") {
			upstream.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		upstream.ServeHTTP(rec, r)
		if rec.Code == http.StatusOK && delayed.CompareAndSwap(false, true) {
			time.Sleep(testLease + testLease/4) // the first assignment arrives once its lease has passed
		}
		maps.Copy(w.Header(), rec.Header())
		w.WriteHeader(rec.Code)
		w.Write(rec.Body.Bytes())
	}))
	t.Cleanup(proxy.Close)
	f.server = proxy
	var started atomic.Int32
	startWorker(t, f, func(cfg *Config) {
		child := cfg.Child
		cfg.Child = func(ctx context.Context) *exec.Cmd {
			started.Add(1)
			return child(ctx)
		}
	})
	outcome, _ := runJob(t, f.c, layerGraph(t, f, "delivered late"))
	if outcome.Failure != nil {
		t.Fatalf("outcome = %+v, want the retry to build it", outcome.Failure)
	}
	if !delayed.Load() {
		t.Fatal("no assignment was delayed")
	}
	if got := started.Load(); got != 1 {
		t.Errorf("the worker started %d attempts' processes, want only the retry's", got)
	}
}

// TestWorkerFailsUnreadableOutput checks that an attempt whose subprocess prints a line too long to read fails, its
// process killed, rather than leaving it blocked writing the rest while the worker renews its lease.
func TestWorkerFailsUnreadableOutput(t *testing.T) {
	f := newFixture(t)
	startWorker(t, f, func(cfg *Config) {
		cfg.Child = func(ctx context.Context) *exec.Cmd {
			script := fmt.Sprintf(`head -c %d /dev/zero | tr '\0' a; echo; sleep 300`, run.MaxLineSize+(1<<20))
			return exec.CommandContext(ctx, "sh", "-c", script)
		}
	})
	req := layerGraph(t, f, "too long a line")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	id, err := f.c.Submit(ctx, api.Request{Graph: &req})
	testenv.Must(t, err)
	if err := f.c.Watch(ctx, id, 0, func(api.Event) error { return nil }); err != nil {
		t.Fatalf("the build never ended: %v", err)
	}
	outcome, err := f.c.Result(ctx, id)
	testenv.Must(t, err)
	testenv.Must(t, f.c.Release(t.Context(), id))
	if outcome.Failure == nil || !strings.Contains(outcome.Failure.Message, "reading the attempt's output") {
		t.Fatalf("outcome = %+v, want a failure to read the attempt's output", outcome.Failure)
	}
	// What it used is read before it is killed, which removes its cgroup.
	if ts := tries(t, f, outcome); len(ts) != 1 || ts[0].Args["cpu"] == nil {
		t.Errorf("the failed attempt's tries = %+v; want one, saying the CPU it used", ts)
	}
}

// waitForContainedAttempt returns the ID of an attempt that w runs whose container is in its store,
// and a channel closed once w has finished with it.
func waitForContainedAttempt(t *testing.T, w *Worker, stores *testenv.Stores) (string, <-chan struct{}) {
	t.Helper()
	for start := time.Now(); ; {
		leftover, err := run.LeftoverAttempts(stores.Options)
		testenv.Must(t, err)
		w.mu.Lock()
		for _, id := range leftover {
			if r, ok := w.attempts[id]; ok {
				w.mu.Unlock()
				return id, r.done
			}
		}
		w.mu.Unlock()
		if time.Since(start) > 30*time.Second {
			t.Fatal("the worker never ran an attempt with a container")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// mustURL parses raw.
func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	testenv.Must(t, err)
	return u
}

// TestPruneClearsRepository checks that a coordinator's prune, once a lease has passed since it started, removes the
// cache repository's tags along with the records and blobs, so that the image a result located there is gone.
func TestPruneClearsRepository(t *testing.T) {
	f := newFixture(t)
	startWorker(t, f)
	ctx := t.Context()
	outcome, _ := runJob(t, f.c, layerGraph(t, f, "pruned away"))
	if outcome.Failure != nil {
		t.Fatalf("outcome = %+v", outcome.Failure)
	}
	res := outcome.Results[0]
	var location cache.Location
	for _, l := range res.Locations {
		if l.Kind == cache.LocationRegistry {
			location = l
		}
	}
	checker := &coord.Registry{Sys: mustSys(t)}
	if problem, err := checker.CheckImage(ctx, res.Image, location); problem != nil || err != nil {
		t.Fatalf("the image at its location before the prune = %v, %v; want it there", problem, err)
	}
	time.Sleep(testLease) // a prune is refused for a lease after the coordinator started
	id, err := f.c.Prune(ctx)
	testenv.Must(t, err)
	testenv.Must(t, f.c.Watch(ctx, id, 0, func(api.Event) error { return nil }))
	pruned, err := f.c.Result(ctx, id)
	testenv.Must(t, err)
	if pruned.Failure != nil || pruned.Pruned == nil || pruned.Pruned.Tags != 1 || pruned.Pruned.Results == 0 ||
		pruned.Pruned.Blobs == 0 {
		t.Fatalf("the prune's outcome = %+v, failure %+v; want 1 tag and the results removed", pruned, pruned.Failure)
	}
	if problem, err := checker.CheckImage(ctx, res.Image, location); problem == nil || err != nil {
		t.Errorf("the image at its location after the prune = %v, %v; want it gone", problem, err)
	}
	// A fresh store's generation is 1, so one prune makes it 2.
	if caps, err := f.c.Capabilities(ctx); err != nil || caps.Generation != 2 {
		t.Errorf("capabilities after the prune = %+v, %v; want generation 2", caps, err)
	}
	// The next build is cold: the layer is built again, and pushed again.
	outcome, statuses := runJob(t, f.c, layerGraph(t, f, "pruned away"))
	if outcome.Failure != nil || !strings.Contains(strings.Join(statuses, ","), "building") {
		t.Errorf("the build after the prune = %+v with statuses %q; want the layer built again", outcome.Failure,
			statuses)
	}
	if problem, err := checker.CheckImage(ctx, res.Image, location); problem != nil || err != nil {
		t.Errorf("the image after the rebuild = %v, %v; want it back at its location", problem, err)
	}
}

// TestUsageOfABusyCgroup checks that an attempt's cgroup tells the CPU time its processes used, here two seconds of a
// busy loop, of which a tenth of a second must have run, however loaded the host, and their peak memory if it counts
// memory, or leaves it out rather than reporting none.
func TestUsageOfABusyCgroup(t *testing.T) {
	c, err := newContainment(testenv.CgroupRoot(t))
	if err != nil {
		testenv.Unavailable(t, "a cgroup of the test's own", err)
	}
	cgroup, err := c.create("busy")
	testenv.Must(t, err)
	defer cgroup.Close()
	defer func() { _ = c.kill("busy", killWait) }()
	cmd := exec.Command("timeout", "2", "sh", "-c", "while :; do :; done")
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cgroup.Fd())}
	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 124 { // as timeout ends the loop
		t.Fatalf("the busy loop ended with %v; want timeout's exit status 124", err)
	}
	u := c.usage("busy")
	if u == nil || u.CPUMicros == nil || *u.CPUMicros < 100_000 {
		t.Fatalf("the cgroup's usage = %+v; want at least a tenth of a second of CPU", u)
	}
	_, err = os.Stat(filepath.Join(c.path("busy"), "memory.peak"))
	switch counts := err == nil; {
	case counts && (u.MemoryPeak == nil || *u.MemoryPeak <= 0):
		t.Errorf("a cgroup that counts memory reported a peak of %v; want some", u.MemoryPeak)
	case !counts && u.MemoryPeak != nil:
		t.Errorf("a cgroup without memory.peak reported a peak of %d", *u.MemoryPeak)
	}
}

// tries returns the try spans of the attempts that the job of outcome stored, from f's store.
func tries(t *testing.T, f *fixture, outcome api.Outcome) []tracing.Span {
	t.Helper()
	if outcome.Trace == nil {
		t.Fatalf("the outcome %+v names no trace", outcome)
	}
	data, err := f.m.Read(t.Context(), *outcome.Trace, tracing.MaxTraceSize)
	testenv.Must(t, err)
	manifest, err := api.DecodeJobTrace(data)
	testenv.Must(t, err)
	var found []tracing.Span
	for _, e := range manifest.Entries {
		if e.Trace == nil {
			continue
		}
		data, err := f.m.Read(t.Context(), *e.Trace, tracing.MaxTraceSize)
		testenv.Must(t, err)
		a, err := tracing.DecodeAttempt(data)
		testenv.Must(t, err)
		for _, s := range a.Spans {
			if s.Args[tracing.Role] == tracing.RoleTry {
				found = append(found, s)
			}
		}
	}
	return found
}

// metered configures a worker to count into reader.
func metered(t *testing.T, reader *sdkmetric.ManualReader) func(*Config) {
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	return func(cfg *Config) { cfg.Meter = provider }
}

// counted returns the value of the counter name in what reader collects now, failing t if it isn't exported.
func counted(t *testing.T, reader *sdkmetric.ManualReader, name string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	testenv.Must(t, reader.Collect(t.Context(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok && m.Name == name && len(sum.DataPoints) == 1 {
				return sum.DataPoints[0].Value
			}
		}
	}
	t.Fatalf("the worker exports no %s", name)
	return 0
}

// TestWorkerCountsDroppedEvents checks that a worker counts the batches of an attempt's events the coordinator
// didn't take, which it doesn't send again, and that its counters are exported from zero.
func TestWorkerCountsDroppedEvents(t *testing.T) {
	f := newFixture(t)
	upstream := httputil.NewSingleHostReverseProxy(mustURL(t, f.server.URL))
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/attempts/") &&
			strings.HasSuffix(r.URL.Path, "/events") {
			http.Error(w, "refused", http.StatusInternalServerError) // as a coordinator that fails to take them
			return
		}
		upstream.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	f.server = proxy
	reader := sdkmetric.NewManualReader()
	startWorker(t, f, metered(t, reader))
	for _, name := range []string{"vamos.worker.fenced", "vamos.worker.events.dropped", "vamos.worker.cgroups.stuck"} {
		if n := counted(t, reader, name); n != 0 {
			t.Errorf("before any attempt, %s = %d; want 0", name, n)
		}
	}
	outcome, _ := runJob(t, f.c, layerGraph(t, f, "events dropped"))
	if outcome.Failure != nil {
		t.Fatalf("outcome = %+v; want the build to succeed without its events", outcome.Failure)
	}
	if n := counted(t, reader, "vamos.worker.events.dropped"); n < 1 {
		t.Errorf("the worker counted %d dropped batches; want the attempt's", n)
	}
}

// TestKillOfACgroupThatWontEmpty checks that a kill of a cgroup whose processes don't exit fails as errStuck, which
// the worker counts once for the attempt, though both the cancellation's kill and the cleanup's find it so.
func TestKillOfACgroupThatWontEmpty(t *testing.T) {
	c := &containment{root: t.TempDir()} // a directory that acts as a cgroup that never empties
	dir := c.path("stuck")
	testenv.Must(t, os.MkdirAll(dir, 0o755))
	testenv.Must(t, os.WriteFile(filepath.Join(dir, "cgroup.kill"), nil, 0o644))
	testenv.Must(t, os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte("123\n"), 0o644))
	reader := sdkmetric.NewManualReader()
	cfg := Config{}
	metered(t, reader)(&cfg)
	m, err := newMetrics(cfg.Meter)
	testenv.Must(t, err)
	w := &Worker{contain: c, metrics: m}
	kill := w.killer("stuck", 50*time.Millisecond)
	for _, remove := range []bool{false, true} { // the cancellation's, and the cleanup's
		if err := kill(remove); !errors.Is(err, errStuck) {
			t.Errorf("the kill = %v; want %v", err, errStuck)
		}
	}
	if n := counted(t, reader, "vamos.worker.cgroups.stuck"); n != 1 {
		t.Errorf("the worker counted %d stuck cgroups; want the attempt's one", n)
	}
}

// TestWorkerKillsAChildThatClosedItsOutput checks that an attempt whose subprocess closed its output without a report
// and runs on is still killed as its job is cancelled, as the end of its output doesn't end the attempt.
func TestWorkerKillsAChildThatClosedItsOutput(t *testing.T) {
	f := newFixture(t)
	w, _ := startWorker(t, f, func(cfg *Config) {
		cfg.Child = func(ctx context.Context) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c", "exec >&-; sleep 300")
		}
	})
	req := layerGraph(t, f, "closed output")
	ctx := t.Context()
	id, err := f.c.Submit(ctx, api.Request{Graph: &req})
	testenv.Must(t, err)
	var attempt string
	for deadline := time.Now().Add(time.Minute); attempt == ""; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the attempt never started")
		}
		if ids, _ := w.contain.leftovers(); len(ids) == 1 {
			attempt = ids[0]
		}
	}
	time.Sleep(200 * time.Millisecond) // so that its output has ended
	testenv.Must(t, f.c.Cancel(ctx, id))
	for start := time.Now(); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(w.contain.path(attempt)); errors.Is(err, os.ErrNotExist) {
			return
		}
		if time.Since(start) > 30*time.Second {
			t.Fatalf("attempt %s still runs %s after its job was cancelled", attempt, time.Since(start))
		}
	}
}

// TestWorkerReportsNothingOfAFencedChildWithoutAReport checks that a fence that kills a subprocess that closed its
// output without a report sends no report, of a failure the kill made, so that the coordinator, once the lease
// expires, tries the attempt again rather than taking the failure as the attempt's; and that the fence is counted.
func TestWorkerReportsNothingOfAFencedChildWithoutAReport(t *testing.T) {
	f := newFixture(t)
	var hanging atomic.Bool
	var reports atomic.Int64
	upstream := httputil.NewSingleHostReverseProxy(mustURL(t, f.server.URL))
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/report"):
			reports.Add(1)
		case hanging.Load() && strings.HasSuffix(r.URL.Path, "/heartbeat"):
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			return
		}
		upstream.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	f.server = proxy
	reader := sdkmetric.NewManualReader()
	w, _ := startWorker(t, f, metered(t, reader), func(cfg *Config) {
		cfg.Child = func(ctx context.Context) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c", "exec >&-; sleep 300")
		}
	})
	req := layerGraph(t, f, "fenced without a report")
	id, err := f.c.Submit(t.Context(), api.Request{Graph: &req})
	testenv.Must(t, err)
	t.Cleanup(func() { _ = f.c.Cancel(context.Background(), id) })
	// Its lease is renewed, and its process runs, once its cgroup holds it, which closed its output at once.
	var done <-chan struct{}
	for start := time.Now(); done == nil; time.Sleep(20 * time.Millisecond) {
		if time.Since(start) > time.Minute {
			t.Fatal("the attempt's process never started")
		}
		w.mu.Lock()
		for id, r := range w.attempts {
			if w.contain.running(id) {
				done = r.done
			}
		}
		w.mu.Unlock()
	}
	hanging.Store(true)
	select {
	case <-done: // fenced, killed, and finished with, its report sent if it was to be
	case <-time.After(2 * time.Minute):
		t.Fatal("the worker never finished with the attempt")
	}
	if n := counted(t, reader, "vamos.worker.fenced"); n != 1 {
		t.Errorf("the worker counted %d fences; want the one", n)
	}
	if n := reports.Load(); n != 0 {
		t.Errorf("the worker sent %d reports of an attempt it fenced, which reported nothing; want none", n)
	}
}

// running reports whether the attempt's cgroup holds processes.
func (c *containment) running(id string) bool {
	procs, err := os.ReadFile(filepath.Join(c.path(id), "cgroup.procs"))
	return err == nil && strings.TrimSpace(string(procs)) != ""
}

// TestWorkerRemovesACrashedAttemptsToolCaches checks that a worker that starts to find the container of an attempt
// that a crashed run left removes it, and the temporary tool caches in its directory with it.
func TestWorkerRemovesACrashedAttemptsToolCaches(t *testing.T) {
	f := newFixture(t)
	stores := testenv.PrivateStores(t)
	var caches string
	testenv.Must(t, stores.WithStore(func(store storage.Store) error {
		container, err := store.CreateContainer("", []string{attemptPrefix + "crashed"}, "", "", "", nil)
		if err != nil {
			return err
		}
		dir, err := store.ContainerDirectory(container.ID)
		if err != nil {
			return err
		}
		caches = filepath.Join(dir, "tool-caches")
		if err := os.MkdirAll(filepath.Join(caches, "0"), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(caches, "0", "written"), []byte("left"), 0o644)
	}))
	startWorker(t, f, func(cfg *Config) {
		cfg.Store, cfg.Child = stores.Options, TestChild(stores.Options)
	})
	for start := time.Now(); ; time.Sleep(50 * time.Millisecond) {
		leftover, err := run.LeftoverAttempts(stores.Options)
		testenv.Must(t, err)
		_, serr := os.Stat(caches)
		if !slices.Contains(leftover, "crashed") && errors.Is(serr, os.ErrNotExist) {
			break
		}
		if time.Since(start) > time.Minute {
			t.Fatalf("a minute after the worker started, the crashed attempt's container %v and caches %v remain",
				leftover, serr)
		}
	}
}

// TestWorkerRenewsTheLeaseWhileAnAttemptRemovesItsToolCaches checks that the runner deletes an attempt's temporary
// tool caches before its subprocess ends, while the worker renews its lease: a deletion that takes three leases leaves
// the attempt neither lost, which with no retries would fail the build, nor tried again, and its caches gone, once, by
// the time the build ends.
func TestWorkerRenewsTheLeaseWhileAnAttemptRemovesItsToolCaches(t *testing.T) {
	f := newFixtureWithLease(t, testLease, func(cfg *coord.Config) { cfg.Retries = coord.NoRetries })
	removed := filepath.Join(t.TempDir(), "removed")
	_, stores := startWorker(t, f, func(cfg *Config) {
		child := cfg.Child
		cfg.Child = func(ctx context.Context) *exec.Cmd {
			cmd := child(ctx)
			cmd.Env = append(cmd.Env, slowRemovalEnv+"="+removed) // TestChild sets Env, to its environment and more
			return cmd
		}
	})
	platform := platforms.Format(run.HostPlatform)
	g := &graph.Graph{Version: graph.Version, ExecPlatform: platform, Roots: []graph.Ref{1}, Nodes: []graph.Node{
		{Label: "busybox", Desc: `pull "busybox"`, Pull: &graph.Pull{Image: "docker.io/library/busybox:1.36",
			Platform: platform, Digest: testenv.BusyboxIndex}},
		{Label: "app", Desc: `layer "app"`, Layer: &graph.Layer{Base: 0, Platform: platform, Steps: []graph.Step{
			{Op: graph.OpRun, Display: `run(["sh", "-c", "..."])`, Argv: []string{"sh", "-c",
				"echo " + testenv.NewNonce() + " > /cache/written"}},
		}, ToolCaches: []graph.ToolCache{{Path: "/cache", Label: "//:tool"}}}},
	}}
	testenv.Must(t, g.Validate())
	stored, err := f.m.PutBytes(t.Context(), graph.Encode(g))
	testenv.Must(t, err)
	outcome, _ := runJob(t, f.c, api.GraphRequest{Graph: stored, Version: graph.Version, Platform: run.HostPlatform})
	if outcome.Failure != nil {
		t.Fatalf("the build failed: %s", outcome.Failure.Message)
	}
	data, err := os.ReadFile(removed)
	testenv.Must(t, err)
	var dirs []string
	for line := range strings.Lines(string(data)) {
		took, dir, _ := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
		if ns, err := strconv.ParseInt(took, 10, 64); err != nil || time.Duration(ns) < slowRemoval {
			t.Errorf("the removal of %s took %s ns; want at least %v, three leases", dir, took, slowRemoval)
		}
		dirs = append(dirs, dir)
	}
	if len(dirs) != 1 || filepath.Base(dirs[0]) != "tool-caches" {
		t.Fatalf("the attempts removed the temporary caches %q; want one attempt's, once, not tried again", dirs)
	}
	if _, err := os.Stat(dirs[0]); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the attempt's caches at %s remain: %v", dirs[0], err)
	}
	for start := time.Now(); ; time.Sleep(50 * time.Millisecond) {
		leftover, err := run.LeftoverAttempts(stores.Options)
		testenv.Must(t, err)
		if len(leftover) == 0 {
			break
		}
		if time.Since(start) > time.Minute {
			t.Fatalf("a minute after the build, the attempts' containers %v remain", leftover)
		}
	}
}
