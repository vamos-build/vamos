package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/afero"
	"github.com/vamos-build/vamos/internal/api"
)

// validTraceparent is a W3C traceparent, sampled.
const validTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

// TestReadOuterTrace checks which TRACEPARENT a command building on a coordinator sends: a valid one, with its
// TRACESTATE; none for an empty one, without a word; none for a malformed one, or one with an all-zero trace ID,
// and no TRACESTATE that isn't one, warning once, naming the variable and its value; and nothing, without a word,
// without a coordinator.
func TestReadOuterTrace(t *testing.T) {
	for _, c := range []struct {
		parent, state         string
		wantParent, wantState string
		warning               string
	}{
		{parent: validTraceparent, state: "ci=pipeline", wantParent: validTraceparent, wantState: "ci=pipeline"},
		{parent: ""},
		{parent: "00-not-a-trace",
			warning: "vamos: warning: TRACEPARENT \"00-not-a-trace\" is not a W3C traceparent and is " +
				"ignored\n"},
		{parent: "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
			warning: "vamos: warning: TRACEPARENT " +
				"\"00-00000000000000000000000000000000-00f067aa0ba902b7-01\" is not a W3C traceparent and " +
				"is ignored\n"},
		{parent: validTraceparent, state: "not a state", wantParent: validTraceparent,
			warning: "vamos: warning: TRACESTATE \"not a state\" is not a W3C tracestate and is ignored\n"},
	} {
		t.Setenv("TRACEPARENT", c.parent)
		t.Setenv("TRACESTATE", c.state)
		var stderr bytes.Buffer
		opts := defaultOptions()
		opts.coordinator = "http://coordinator"
		inv := &invocation{ctx: t.Context(), stderr: &stderr, opts: opts}
		inv.outerTrace()
		inv.outerTrace() // as a command that sends a second job does
		if inv.traceParent != c.wantParent || inv.traceState != c.wantState || stderr.String() != c.warning {
			t.Errorf("TRACEPARENT %q: sends %q, %q, warning %q; want %q, %q, warning %q", c.parent, inv.traceParent,
				inv.traceState, stderr.String(), c.wantParent, c.wantState, c.warning)
		}
	}

	t.Setenv("TRACEPARENT", "00-not-a-trace")
	var stderr bytes.Buffer
	opts := defaultOptions()
	opts.coordinator = "" // whatever VAMOS_COORDINATOR says
	local := &invocation{ctx: t.Context(), stderr: &stderr, opts: opts}
	if parent, _ := local.outerTrace(); parent != "" || stderr.Len() != 0 {
		t.Errorf("without a coordinator, sends %q, warning %q; want nothing", parent, stderr.String())
	}
}

// TestOuterTraceSentOnEveryJob checks that a push on a coordinator sends TRACEPARENT and TRACESTATE with both its
// jobs, the build and the push.
func TestOuterTraceSentOnEveryJob(t *testing.T) {
	requireBuildah(t)
	c := newHeldCoordinator(t)
	_, host := insecureRegistry(t, nil)
	var mu sync.Mutex
	var sent []string // each job's kind, traceparent and tracestate
	c.hold(func(r *http.Request, body []byte) bool {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/jobs" {
			return false
		}
		var req api.Request
		if json.Unmarshal(body, &req) == nil {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case req.Graph != nil:
				sent = append(sent, "graph "+req.Graph.Parent+" "+req.Graph.State)
			case req.Push != nil:
				sent = append(sent, "push "+req.Push.Parent+" "+req.Push.State)
			}
		}
		return false // nothing is held
	})
	t.Setenv("TRACEPARENT", validTraceparent)
	t.Setenv("TRACESTATE", "ci=pipeline")
	r := vamos(t, project(t, map[string]string{"BUILD": nonceLayer()}), "push", "app", host+"/team/app:v1",
		"--tls-verify=false", "-C", projectDir, "--coordinator", c.url).succeeds(t)
	if strings.Contains(r.stderr, "TRACEPARENT") {
		t.Errorf("stderr = %q; want no word of a valid TRACEPARENT", r.stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"graph " + validTraceparent + " ci=pipeline", "push " + validTraceparent + " ci=pipeline"}
	if !slices.Equal(sent, want) {
		t.Errorf("the jobs sent %q; want %q", sent, want)
	}
}

// TestCoordinatorRefusesTelemetryMistake checks that a coordinator whose telemetry settings Setup can't take fails to
// start, naming the variable, rather than with a message about its build cache.
func TestCoordinatorRefusesTelemetryMistake(t *testing.T) {
	cmd := NewRootCommand(Config{Fs: afero.NewMemMapFs(), Getenv: func(name string) string {
		return map[string]string{"OTEL_TRACES_EXPORTER": "zipkin"}[name]
	}})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"coordinator", "--listen", "127.0.0.1:0", "--cache-dir", t.TempDir()})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second) // so that one that starts after all ends
	defer cancel()
	err := cmd.ExecuteContext(ctx)
	if err == nil || !strings.HasPrefix(err.Error(), "OTEL_TRACES_EXPORTER=zipkin") {
		t.Errorf("the coordinator started with %v; want it refused, naming OTEL_TRACES_EXPORTER", err)
	}
}

// blockingWriter is a writer whose writes say they have begun, on entered, and wait until release is closed.
type blockingWriter struct{ entered, release chan struct{} }

func (b blockingWriter) Write(p []byte) (int, error) {
	close(b.entered)
	<-b.release
	return len(p), nil
}

// TestLockStreams checks that a write to stdout that blocks holds up no write to a stderr of its own, and that one
// writer given as both is locked once, for both.
func TestLockStreams(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	var stderr bytes.Buffer
	stdout, errs := lockStreams(blockingWriter{entered, release}, &stderr)
	go fmt.Fprint(stdout, "blocked")
	<-entered // the write to stdout holds its lock
	done := make(chan struct{})
	go func() {
		fmt.Fprint(errs, "warning")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a write to stderr waited for a blocked write to stdout")
	}

	var both bytes.Buffer
	out, err := lockStreams(&both, &both)
	if out.(*lockedWriter).mu != err.(*lockedWriter).mu {
		t.Error("one writer given as both streams has two locks")
	}
}
