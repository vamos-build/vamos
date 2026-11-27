package run

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/graph"
	"github.com/vamos-build/vamos/internal/remote"
	"github.com/vamos-build/vamos/internal/sched"
	"github.com/vamos-build/vamos/internal/testenv"

	"github.com/containerd/platforms"
	"go.podman.io/storage"
)

// TestToolCacheRequestsCross checks that an action's request keeps its tool caches, its workspace's name and whether
// it asks for empty tool caches as a coordinator assigns it to a worker, and as the worker gives it to the subprocess
// that runs it.
func TestToolCacheRequestsCross(t *testing.T) {
	caches := []graph.ToolCache{{Path: "/cache", Label: "//tools:go-build", Owner: 1000, Group: 1000}}
	req := &sched.Request{Action: &sched.Action{Kind: sched.KindTask, ToolCaches: caches}, Workspace: "com.acme.shop",
		NoToolCaches: true}
	roundTrip := func(in, out any) []byte {
		t.Helper()
		data, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatal(err)
		}
		return data
	}
	var assignment remote.Assignment
	data := roundTrip(remote.Assignment{Attempt: "a-1", Request: req}, &assignment)
	if r := assignment.Request; r == nil || r.Action == nil || r.Workspace != "com.acme.shop" || !r.NoToolCaches ||
		!slices.Equal(r.Action.ToolCaches, caches) {
		t.Errorf("an assignment %s carries %+v; want its mounts, its workspace and that it is clean", data, r)
	}
	var input AttemptInput
	data = roundTrip(AttemptInput{Attempt: "a-1", Request: req}, &input)
	if r := input.Request; r == nil || r.Action == nil || r.Workspace != "com.acme.shop" || !r.NoToolCaches ||
		!slices.Equal(r.Action.ToolCaches, caches) {
		t.Errorf("an attempt's input %s carries %+v; want its mounts, its workspace and that it is clean", data, r)
	}
}

// cacheGraph returns a graph of a task on busybox, pinned, that mounts a tool cache and runs argv, stored in l's
// content store, and the request to run it.
func cacheGraph(t *testing.T, l *Local, argv ...string) api.GraphRequest {
	t.Helper()
	exec := platforms.Format(platforms.Normalize(platforms.DefaultSpec()))
	g := &graph.Graph{Version: graph.Version, ExecPlatform: exec, Roots: []graph.Ref{1}, Nodes: []graph.Node{
		{Label: "busybox", Desc: `pull "busybox"`, Pull: &graph.Pull{
			Image: "docker.io/library/busybox:1.36", Platform: exec, Digest: testenv.BusyboxIndex,
		}},
		{Label: "build", Desc: `task "build"`, Task: &graph.Task{
			Base:       0,
			Steps:      []graph.Step{{Op: graph.OpRun, Display: "run", Argv: argv}},
			ToolCaches: []graph.ToolCache{{Path: "/cache", Label: "//:tool"}},
		}},
	}}
	testenv.Must(t, g.Validate())
	content, err := l.ContentStore()
	testenv.Must(t, err)
	blob, err := content.PutBytes(t.Context(), graph.Encode(g))
	testenv.Must(t, err)
	platform, err := graph.ParsePlatform(exec)
	testenv.Must(t, err)
	return api.GraphRequest{Graph: blob, Version: graph.Version, Platform: platform}
}

// TestWorkspaceNameRefused checks that a standalone build of a graph that mounts caches fails before anything runs,
// without opening the container store, when its request's workspace name breaks the rules of a name, clean or not,
// or is empty without NoToolCaches; and that an empty name with NoToolCaches, or any name in coordinator mode, gets
// past the check, to the container store, which here can't be opened.
func TestWorkspaceNameRefused(t *testing.T) {
	for _, tc := range []struct {
		name         string
		workspace    string
		noToolCaches bool
		coordinator  bool
		want         string // in the failure
	}{
		{
			name: "no name",
			want: "the build mounts tool caches, so the workspace needs a name in WORKSPACE",
		},
		{
			name:      "a malformed name",
			workspace: "Shop",
			want:      `the request's workspace name "Shop" must start with a lowercase letter`,
		},
		{
			name:         "a malformed name, clean",
			workspace:    "../shop",
			noToolCaches: true,
			want:         "the request's workspace name",
		},
		{
			name:         "no name, clean",
			noToolCaches: true,
			want:         "no-such-driver",
		},
		{
			name:        "no name, for a coordinator",
			coordinator: true,
			want:        "no-such-driver",
		},
		{
			name:        "a malformed name, for a coordinator",
			workspace:   "../shop",
			coordinator: true,
			want:        "no-such-driver",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			l, err := NewLocal(storage.StoreOptions{
				GraphRoot:       filepath.Join(dir, "storage"),
				RunRoot:         filepath.Join(dir, "run"),
				GraphDriverName: "no-such-driver", // which would fail to open
			})
			testenv.Must(t, err)
			defer l.Close()
			if tc.coordinator {
				l.ServeAsCoordinator()
			}
			req := cacheGraph(t, l, "true")
			req.Workspace, req.NoToolCaches = tc.workspace, tc.noToolCaches
			outcome := awaitJob(t, l, api.Request{Graph: &req})
			if outcome.Failure == nil || !strings.Contains(outcome.Failure.Message, tc.want) {
				t.Errorf("the job's outcome = %+v; want it to fail with %q", outcome.Failure, tc.want)
			}
		})
	}
}

// TestWorkspaceNameAccepted checks that a build of a graph that mounts caches succeeds without a workspace name when
// it asks for empty caches, and in coordinator mode without one or with a malformed one.
func TestWorkspaceNameAccepted(t *testing.T) {
	testenv.RequireBuildah(t)
	for _, tc := range []struct {
		name         string
		workspace    string
		noToolCaches bool
		coordinator  bool
	}{
		{name: "no name, clean", noToolCaches: true},
		{name: "no name, for a coordinator", coordinator: true},
		{name: "a malformed name, for a coordinator", workspace: "../shop", coordinator: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, err := NewLocal(testenv.PrivateStores(t).Options)
			testenv.Must(t, err)
			defer l.Close()
			if tc.coordinator {
				l.ServeAsCoordinator()
			}
			req := cacheGraph(t, l, "echo", testenv.NewNonce())
			req.Workspace, req.NoToolCaches = tc.workspace, tc.noToolCaches
			if outcome := awaitJob(t, l, api.Request{Graph: &req}); outcome.Failure != nil {
				t.Errorf("the job failed: %s", outcome.Failure.Message)
			}
		})
	}
}
