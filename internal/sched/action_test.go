package sched

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/fileset"
	"github.com/vamos-build/vamos/internal/graph"
	"github.com/vamos-build/vamos/internal/testenv"

	"github.com/opencontainers/go-digest"
)

// testSelection returns a compiled selection of the files include and exclude select.
func testSelection(t *testing.T, include, exclude []string) *fileset.Selection {
	t.Helper()
	sel := fileset.DefaultSelection()
	sel.Include, sel.Exclude = include, exclude
	testenv.Must(t, sel.Compile(testenv.ProjectDir))
	return sel
}

// TestActionKeyStable pins how an Action is encoded, and so every target's key:
// a change that alters it would make every cached result unusable, so it must be deliberate, and raise ActionVersion.
// An action without tool caches encodes as it did before they were, but for its version.
func TestActionKeyStable(t *testing.T) {
	files := cache.Blob{Digest: digest.FromString("files"), Size: 5}
	a := &Action{
		Version:  ActionVersion,
		Kind:     "layer",
		Base:     "0123abcd",
		Platform: "linux/arm64",
		Steps: []Step{
			{Op: "add", Files: &files, Epoch: 7, Dest: "/app", Owner: "app", Group: "staff"},
			{Op: "run", Argv: []string{"make", "all"}, Secrets: []graph.Secret{
				{ID: "token", Env: "TOKEN"}, {ID: "key", File: "/run/secrets/key"},
			}},
			{Op: "env", Vars: []string{"A=1", "B=2"}},
			{Op: "workdir", Dir: "/app"},
		},
		Command:    []string{"/app/server"},
		Entrypoint: &[]string{"java", "-jar"},
		Source:     "/out",
		Output:     testSelection(t, []string{"dist"}, []string{"dist/*.map"}),
	}
	want := `{"version":12,"kind":"layer","base":"0123abcd","platform":"linux/arm64","steps":[` +
		`{"op":"add","files":{"digest":"` + files.Digest.String() + `","size":5},"epoch":7,"dest":"/app",` +
		`"owner":"app","group":"staff"},{"op":"run","argv":["make","all"],"secrets":[{"id":"token","env":"TOKEN"},` +
		`{"id":"key","file":"/run/secrets/key"}]},{"op":"env","vars":["A=1","B=2"]},` +
		`{"op":"workdir","dir":"/app"}],"command":["/app/server"],"entrypoint":["java","-jar"],"source":"/out",` +
		`"output":{"include":["dist"],` +
		`"exclude":["dist/*.map"],"mode_mask":493,"max_file_count":10000,"max_file_size":104857600,` +
		`"max_total_size":1073741824}}`
	if got := a.Key(); got != digest.FromString(want) {
		data, _ := json.Marshal(a)
		t.Errorf("Action encodes as\n%s\nwant\n%s", data, want)
	}
}

// TestActionKeys checks that an Action's key changes with its steps and their order.
func TestActionKeys(t *testing.T) {
	a := &Action{
		Version: ActionVersion,
		Kind:    "task",
		Base:    "Scratch",
		Steps: []Step{
			{Op: "env", Vars: []string{"LANG=C"}},
			{Op: "run", Argv: []string{"true"}},
		},
	}
	key := a.Key()
	b := *a
	b.Steps = []Step{a.Steps[0], {Op: "run", Argv: []string{"false"}}}
	if b.Key() == key {
		t.Errorf("actions with different commands have the same key")
	}
	b.Steps = []Step{a.Steps[1], a.Steps[0]}
	if b.Key() == key {
		t.Errorf("actions with steps in a different order have the same key")
	}
	b.Steps = []Step{a.Steps[0], {Op: "run", Argv: []string{"true"}, Secrets: []graph.Secret{{ID: "t", Env: "T"}}}}
	if b.Key() == key {
		t.Errorf("actions whose steps are given different secrets have the same key")
	}
}

// TestToolCacheKeys checks that an Action's key changes with where each of its tool caches is mounted, which one it is,
// and its owner and group, and that an action that mounts caches encodes them.
func TestToolCacheKeys(t *testing.T) {
	a := &Action{
		Version:    ActionVersion,
		Kind:       "task",
		Base:       "Scratch",
		Steps:      []Step{{Op: "run", Argv: []string{"go", "build"}}},
		ToolCaches: []graph.ToolCache{{Path: "/cache", Label: "//tools:go-build", Owner: 1000, Group: 1000}},
	}
	keys := map[digest.Digest]string{a.Key(): "as declared"}
	for name, change := range map[string]func(*graph.ToolCache){
		"another path":  func(c *graph.ToolCache) { c.Path = "/other" },
		"another label": func(c *graph.ToolCache) { c.Label = "//tools:go-mod" },
		"another owner": func(c *graph.ToolCache) { c.Owner = 0 },
		"another group": func(c *graph.ToolCache) { c.Group = 0 },
	} {
		b := *a
		b.ToolCaches = []graph.ToolCache{a.ToolCaches[0]}
		change(&b.ToolCaches[0])
		if other, ok := keys[b.Key()]; ok {
			t.Errorf("a tool cache with %s has the key of one %s", name, other)
		}
		keys[b.Key()] = name
	}
	b := *a
	b.ToolCaches = nil
	if _, ok := keys[b.Key()]; ok {
		t.Errorf("an action without its tool cache has the key of one with it")
	}
	data, _ := json.Marshal(a)
	want := `"tool_caches":[{"path":"/cache","label":"//tools:go-build","owner":1000,"group":1000}]`
	if !strings.Contains(string(data), want) {
		t.Errorf("an action that mounts a cache encodes as %s, without %s", data, want)
	}
}

// TestEntrypointKeys checks that a layer's key tells an entrypoint left out, which keeps the base's, from an empty one,
// which removes it, and from a set one; and that an action without one has the key it had before entrypoints were.
func TestEntrypointKeys(t *testing.T) {
	a := &Action{Version: ActionVersion, Kind: "layer", Base: "Scratch", Platform: "linux/amd64"}
	keys := map[digest.Digest]string{a.Key(): "left out"}
	for name, entrypoint := range map[string][]string{"empty": {}, "set": {"java"}} {
		b := *a
		b.Entrypoint = &entrypoint
		if other, ok := keys[b.Key()]; ok {
			t.Errorf("an entrypoint %s has the key of one %s", name, other)
		}
		keys[b.Key()] = name
	}
	if data, _ := json.Marshal(a); strings.Contains(string(data), "entrypoint") {
		t.Errorf("an action without an entrypoint encodes as %s, which names one", data)
	}
}

// TestRequestSecretsNotEncoded checks that a request's secret values are never encoded, as for a worker's attempt.
func TestRequestSecretsNotEncoded(t *testing.T) {
	req := Request{Action: &Action{Version: ActionVersion}, Secrets: map[string][]byte{"token": []byte("hunter2")}}
	data, err := json.Marshal(req)
	testenv.Must(t, err)
	if strings.Contains(string(data), "hunter2") || strings.Contains(string(data), "token") {
		t.Errorf("a request encodes as %s, which holds its secrets", data)
	}
}
