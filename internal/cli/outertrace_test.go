package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

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
