package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/remote"
	"github.com/vamos-build/vamos/internal/run"
)

// TestCommandsRefuseAToolCache checks that every command that takes a target refuses a tool cache's label, saying
// that a tool cache is mounted with mount(): build, which builds a publication's images rather than refusing one,
// among them, and publish, which takes a publication.
func TestCommandsRefuseAToolCache(t *testing.T) {
	const want = `"//tools:go-build" is a tool_cache(), which is no target: ` +
		"a tool cache is mounted with mount(), in a task's or layer's tool_caches"
	fsys := project(t, map[string]string{
		"WORKSPACE":   `workspace(name = "com.acme.shop")`,
		"tools/BUILD": `tool_cache(name = "go-build", path = "/cache/go-build")`,
	})
	dir := t.TempDir()
	for _, args := range [][]string{
		{"build", "//tools:go-build"},
		{"save", "//tools:go-build", dir + "/app.tar"},
		{"write", "//tools:go-build", dir + "/app.tar"},
		{"export", "//tools:go-build", dir + "/out"},
		{"push", "//tools:go-build", "registry.example/app:v1"},
		{"load", "//tools:go-build", "app:v1"},
		{"graph", "//tools:go-build"},
		{"publish", "//tools:go-build", "--tag", "v1"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			r := vamos(t, fsys, append(args, "-C", projectDir)...)
			if r.err == nil || !strings.Contains(r.err.Error(), want) {
				t.Errorf("vamos %s = %v; want an error with %q", strings.Join(args, " "), r.err, want)
			}
		})
	}
}

// TestClientRefusesGraphVersion4 checks that a client refuses an executor that runs only graph version 4, which would
// decode a graph's tool caches and drop them, building every target without them.
func TestClientRefusesGraphVersion4(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/capabilities" {
			http.NotFound(rw, r)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"graph_versions": [4]}`))
	}))
	t.Cleanup(server.Close)
	fsys := project(t, map[string]string{
		"WORKSPACE": `workspace(name = "com.acme.shop")`,
		"BUILD": `
tool_cache(name = "tool", path = "/cache")
task(name = "app", base = "scratch", tool_caches = [mount("tool")], steps = [run(["true"])])
`,
	})
	r := vamos(t, fsys, "build", "app", "--coordinator", server.URL, "-C", projectDir)
	want := "the coordinator at " + server.URL + " runs graph versions [4], not this vamos's, 5"
	if r.err == nil || !strings.Contains(r.err.Error(), want) {
		t.Errorf("vamos build = %v; want an error with %q", r.err, want)
	}
}

// TestLocalCoordinatorGivesCachesEmpty checks that vamos coordinator --local serves a local executor in coordinator
// mode, which gives every attempt empty tool caches, since its clients' workspaces are their own.
func TestLocalCoordinatorGivesCachesEmpty(t *testing.T) {
	local, err := newLocalCoordinator(stores(t).Options)
	must(t, err)
	defer local.Close()
	if !local.ServesAsCoordinator() {
		t.Error("vamos coordinator --local's executor doesn't serve as a coordinator")
	}
}

// refusingExecutor is a local executor that records each graph request it is given, and refuses it.
type refusingExecutor struct {
	*run.Local
	mu       sync.Mutex
	requests []api.GraphRequest
}

func (r *refusingExecutor) Submit(_ context.Context, req api.Request) (api.JobID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.Graph != nil {
		r.requests = append(r.requests, *req.Graph)
	}
	return "", errors.New("refused, once recorded")
}

// TestNoToolCachesFlag checks that vamos build --no-tool-caches asks the executor for empty tool caches in its graph
// request, with the workspace's name, and that a build without it doesn't.
func TestNoToolCachesFlag(t *testing.T) {
	local, err := newLocalCoordinator(stores(t).Options)
	must(t, err)
	defer local.Close()
	content, err := local.ContentStore()
	must(t, err)
	ex := &refusingExecutor{Local: local}
	server := httptest.NewServer((&remote.Server{Executor: ex, Content: content}).Handler())
	t.Cleanup(server.Close)
	fsys := project(t, map[string]string{
		"WORKSPACE": `workspace(name = "com.acme.shop")`,
		"BUILD": `
tool_cache(name = "tool", path = "/cache")
task(name = "app", base = "scratch", tool_caches = [mount("tool")], steps = [run(["true"])])
`,
	})
	for _, flags := range [][]string{{"--no-tool-caches"}, nil} {
		args := append([]string{"build", "app", "--coordinator", server.URL, "-C", projectDir}, flags...)
		if r := vamos(t, fsys, args...); r.err == nil {
			t.Fatalf("vamos %s succeeded, though the executor refused it", strings.Join(args, " "))
		}
	}
	if len(ex.requests) != 2 {
		t.Fatalf("the executor was given %d graph requests, want 2", len(ex.requests))
	}
	for i, want := range []bool{true, false} {
		if got := ex.requests[i]; got.NoToolCaches != want || got.Workspace != "com.acme.shop" {
			t.Errorf("request %d asks for no tool caches %v in workspace %q; want %v in com.acme.shop",
				i, got.NoToolCaches, got.Workspace, want)
		}
	}
}
