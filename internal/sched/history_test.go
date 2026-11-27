package sched

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/graph"

	"github.com/opencontainers/go-digest"
)

// version is a history entry's version of what an action covers.
type version int

// tree returns the digest of a tree named name.
func tree(name string) digest.Digest {
	return digest.FromString("tree " + name)
}

// entryOf returns the history entry of a, keyed by name, whose base and adds are supplied as given, at the
// current ActionVersion unless v says otherwise.
func entryOf(t *testing.T, name string, a *Action, base *cache.Supplier, adds []cache.Supplier,
	v ...version) cache.HistoryEntry {
	t.Helper()
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	e := cache.HistoryEntry{Key: digest.FromString(name), ActionVersion: ActionVersion, Action: data, Base: base,
		Adds: adds, Recorded: time.Unix(1000, 0)}
	if len(v) > 0 {
		e.ActionVersion = int(v[0])
	}
	return e
}

// layer returns a layer's action on base, an image ID, adding a source's files and a task's, then running cmd.
func layer(base string, source *cache.Blob, task *cache.Blob, taskEpoch int64, cmd ...string) *Action {
	a := &Action{Version: ActionVersion, Kind: KindLayer, Base: base, Platform: "linux/amd64"}
	if source != nil {
		a.Steps = append(a.Steps, Step{Op: graph.OpAdd, Files: source, Dest: "/src"})
	}
	if task != nil {
		a.Steps = append(a.Steps, Step{Op: graph.OpAdd, Files: task, Epoch: taskEpoch, Dest: "/gen"})
	}
	if len(cmd) > 0 {
		a.Steps = append(a.Steps, Step{Op: graph.OpRun, Argv: cmd})
	}
	return a
}

// twoAdds returns a layer's action on base adding the files of two tasks, both files, then running make.
func twoAdds(base string, files *cache.Blob) *Action {
	a := layer(base, nil, files, 5)
	a.Steps = append(a.Steps, Step{Op: graph.OpAdd, Files: files, Epoch: 5, Dest: "/gen2"},
		Step{Op: graph.OpRun, Argv: []string{"make"}})
	return a
}

// withSteps returns a with a run step more for each of cmds.
func withSteps(a *Action, cmds ...string) *Action {
	for _, c := range cmds {
		a.Steps = append(a.Steps, Step{Op: graph.OpRun, Argv: []string{c}})
	}
	return a
}

// mounting returns a with the tool caches caches, each a path and a label, owned by root.
func mounting(a *Action, caches ...string) *Action {
	for i := 0; i < len(caches); i += 2 {
		a.ToolCaches = append(a.ToolCaches, graph.ToolCache{Path: caches[i], Label: caches[i+1]})
	}
	return a
}

// files returns a source's files, each regular with mode 0644 and content named by itself, but as changes says.
func files(paths ...string) []cache.HistoryFile {
	var out []cache.HistoryFile
	for _, p := range paths {
		out = append(out, cache.HistoryFile{Path: p, Type: "file", Mode: 0o644, Digest: digest.FromString(p)})
	}
	return out
}

// TestAttribute checks the cause attribute finds for each kind of miss, against the nearest entry of history.
func TestAttribute(t *testing.T) {
	srcOne, srcTwo := &cache.Blob{Digest: tree("one")}, &cache.Blob{Digest: tree("two")}
	gen1, gen2 := &cache.Blob{Digest: tree("gen1")}, &cache.Blob{Digest: tree("gen2")}
	pull := func(image string, manifest string) *cache.Supplier {
		return &cache.Supplier{Target: "//:base platform=linux/amd64", Image: image,
			Manifest: digest.FromString(manifest)}
	}
	other := &cache.Supplier{Target: "//:other platform=linux/amd64", Image: "busybox:1.36",
		Manifest: digest.FromString("m1")}
	runtime := &cache.Supplier{Target: "//:runtime platform=linux/amd64"}
	src := func(d digest.Digest) cache.Supplier { return cache.Supplier{Tree: d} }
	gen := cache.Supplier{Target: "//:gen platform=linux/amd64"}
	trees := map[digest.Digest][]cache.HistoryFile{
		tree("one"): files("a.txt", "b.txt", "c.txt"),
		tree("two"): append(files("a.txt", "c.txt", "d.txt"), cache.HistoryFile{}),
	}
	// tree two: a.txt changed, b.txt removed, d.txt added
	trees[tree("two")] = []cache.HistoryFile{
		{Path: "a.txt", Type: "file", Mode: 0o644, Digest: digest.FromString("a.txt, edited")},
		{Path: "c.txt", Type: "file", Mode: 0o644, Digest: digest.FromString("c.txt")},
		{Path: "d.txt", Type: "file", Mode: 0o644, Digest: digest.FromString("d.txt")},
	}
	modeOnly := []cache.HistoryFile{{Path: "run.sh", Type: "file", Mode: 0o644, Digest: digest.FromString("run.sh")}}
	modeChanged := []cache.HistoryFile{{Path: "run.sh", Type: "file", Mode: 0o755, Digest: digest.FromString("run.sh")}}

	base := layer("img", srcOne, nil, 0, "make")
	for _, tc := range []struct {
		name    string
		facts   missFacts
		history []cache.HistoryEntry
		trees   map[digest.Digest][]cache.HistoryFile // the history's
		want    attribution
	}{
		{
			name:  "forced, whatever changed",
			facts: missFacts{forced: true, lost: true, named: true, entry: entryOf(t, "x", base, nil, nil)},
			want:  attribution{cause: CauseForced},
		},
		{
			name:  "a result that can't be used is lost",
			facts: missFacts{lost: true, named: true, entry: entryOf(t, "x", base, nil, nil)},
			want:  attribution{cause: CauseLost},
		},
		{
			name: "an action history holds is lost",
			facts: missFacts{named: true, entry: entryOf(t, "same", base, pull("b:1", "m1"),
				[]cache.Supplier{src(tree("one"))})},
			history: []cache.HistoryEntry{entryOf(t, "same", base, pull("b:1", "m1"),
				[]cache.Supplier{src(tree("one"))})},
			want: attribution{cause: CauseLost},
		},
		{
			name:    "an unnamed workspace's miss is new",
			facts:   missFacts{entry: entryOf(t, "x", base, nil, nil)},
			history: []cache.HistoryEntry{entryOf(t, "y", layer("img", srcOne, nil, 0, "make", "-j"), nil, nil)},
			want:    attribution{cause: CauseNew},
		},
		{
			name:  "a first build is new",
			facts: missFacts{named: true, entry: entryOf(t, "x", base, nil, nil)},
			want:  attribution{cause: CauseNew},
		},
		{
			name:    "entries of another version are no comparison",
			facts:   missFacts{named: true, entry: entryOf(t, "x", base, nil, nil)},
			history: []cache.HistoryEntry{entryOf(t, "y", layer("img", srcOne, nil, 0, "other"), nil, nil, version(1))},
			want:    attribution{cause: CauseNew},
		},
		{
			name:  "a changed step is own-inputs, naming no file",
			facts: missFacts{named: true, entry: entryOf(t, "x", base, nil, []cache.Supplier{src(tree("one"))})},
			history: []cache.HistoryEntry{entryOf(t, "y", layer("img", srcOne, nil, 0, "make", "all"), nil,
				[]cache.Supplier{src(tree("one"))})},
			want: attribution{cause: CauseOwnInputs},
		},
		{
			name:  "steps that differ in count are own-inputs",
			facts: missFacts{named: true, entry: entryOf(t, "x", base, nil, []cache.Supplier{src(tree("one"))})},
			history: []cache.HistoryEntry{entryOf(t, "y", layer("img", srcOne, nil, 0), nil,
				[]cache.Supplier{src(tree("one"))})},
			want: attribution{cause: CauseOwnInputs},
		},
		{
			name: "a changed output directory is own-inputs",
			facts: missFacts{named: true, entry: entryOf(t, "x",
				&Action{Version: ActionVersion, Kind: KindTask, Base: "img", Source: "/two"}, nil, nil)},
			history: []cache.HistoryEntry{entryOf(t, "y",
				&Action{Version: ActionVersion, Kind: KindTask, Base: "img", Source: "/one"}, nil, nil)},
			want: attribution{cause: CauseOwnInputs},
		},
		{
			name: "a layer that became a task is own-inputs",
			facts: missFacts{named: true, entry: entryOf(t, "x",
				&Action{Version: ActionVersion, Kind: KindTask, Base: "img", Steps: base.Steps}, nil, nil)},
			history: []cache.HistoryEntry{entryOf(t, "y",
				&Action{Version: ActionVersion, Kind: KindLayer, Base: "img", Steps: base.Steps}, nil, nil)},
			want: attribution{cause: CauseOwnInputs},
		},
		{
			name: "files added, removed and changed are named",
			facts: missFacts{named: true, trees: trees,
				entry: entryOf(t, "x", layer("img", srcTwo, nil, 0, "make"), nil, []cache.Supplier{src(tree("two"))})},
			history: []cache.HistoryEntry{entryOf(t, "y", base, nil, []cache.Supplier{src(tree("one"))})},
			trees:   trees,
			want: attribution{cause: CauseOwnInputs, added: []string{"d.txt"}, removed: []string{"b.txt"},
				changed: []string{"a.txt"}},
		},
		{
			name: "a change of mode alone is a change",
			facts: missFacts{named: true, trees: map[digest.Digest][]cache.HistoryFile{tree("two"): modeChanged},
				entry: entryOf(t, "x", layer("img", srcTwo, nil, 0), nil, []cache.Supplier{src(tree("two"))})},
			history: []cache.HistoryEntry{entryOf(t, "y", layer("img", srcOne, nil, 0), nil,
				[]cache.Supplier{src(tree("one"))})},
			trees: map[digest.Digest][]cache.HistoryFile{tree("one"): modeOnly},
			want:  attribution{cause: CauseOwnInputs, changed: []string{"run.sh"}},
		},
		{
			name: "a source kept by its digest alone changed",
			facts: missFacts{named: true, trees: trees,
				entry: entryOf(t, "x", layer("img", srcTwo, nil, 0, "make"), nil, []cache.Supplier{src(tree("two"))})},
			history: []cache.HistoryEntry{entryOf(t, "y", base, nil, []cache.Supplier{src(tree("one"))})},
			want:    attribution{cause: CauseOwnInputs, sourceChanged: true},
		},
		{
			name: "the nearest entry is compared, not the latest",
			facts: missFacts{named: true, trees: trees,
				entry: entryOf(t, "x", layer("img", srcTwo, nil, 0, "make"), nil, []cache.Supplier{src(tree("two"))})},
			history: []cache.HistoryEntry{
				entryOf(t, "latest", withSteps(layer("img", srcTwo, nil, 0, "make"), "lint", "test"), nil,
					[]cache.Supplier{src(tree("two"))}),
				entryOf(t, "nearest", layer("img", srcTwo, nil, 0, "make"), nil, []cache.Supplier{src(tree("one"))}),
			},
			trees: map[digest.Digest][]cache.HistoryFile{tree("one"): trees[tree("two")][:2]},
			// against "latest", two steps; against "nearest", only d.txt added
			want: attribution{cause: CauseOwnInputs, added: []string{"d.txt"}},
		},
		{
			name:  "of entries that differ equally, the most recent is compared",
			facts: missFacts{named: true, entry: entryOf(t, "x", layer("img", nil, nil, 0, "make"), nil, nil)},
			history: []cache.HistoryEntry{
				entryOf(t, "recent", layer("img", nil, nil, 0, "make", "-k"), nil, nil),
				entryOf(t, "older", layer("other", nil, nil, 0, "make"), runtime, nil),
			},
			want: attribution{cause: CauseOwnInputs},
		},
		{
			name: "a moved tag is upstream, naming both manifests",
			facts: missFacts{named: true, baseDesc: `pull "base"`,
				entry: entryOf(t, "x", layer("img2", nil, nil, 0, "make"), pull("b:1", "m2"), nil)},
			history: []cache.HistoryEntry{entryOf(t, "y", layer("img1", nil, nil, 0, "make"), pull("b:1", "m1"), nil)},
			want: attribution{cause: CauseUpstream, pull: `pull "base"`, before: digest.FromString("m1"),
				after: digest.FromString("m2")},
		},
		{
			name: "an edited tag is dependency",
			facts: missFacts{named: true, baseDesc: `pull "base"`,
				entry: entryOf(t, "x", layer("img2", nil, nil, 0, "make"), pull("b:2", "m2"), nil)},
			history: []cache.HistoryEntry{entryOf(t, "y", layer("img1", nil, nil, 0, "make"), pull("b:1", "m1"), nil)},
			want:    attribution{cause: CauseDependency, dependencies: []string{`pull "base"`}},
		},
		{
			name: "a moved pull and a changed task together are dependency, naming both",
			facts: missFacts{named: true, baseDesc: `pull "base"`, addDescs: []string{`task "gen"`},
				entry: entryOf(t, "x", layer("img2", nil, gen2, 5, "make"), pull("b:1", "m2"), []cache.Supplier{gen})},
			history: []cache.HistoryEntry{entryOf(t, "y", layer("img1", nil, gen1, 5, "make"), pull("b:1", "m1"),
				[]cache.Supplier{gen})},
			want: attribution{cause: CauseDependency, dependencies: []string{`pull "base"`, `task "gen"`}},
		},
		{
			name: "a rebuilt task's epoch alone is dependency",
			facts: missFacts{named: true, addDescs: []string{`task "gen"`},
				entry: entryOf(t, "x", layer("img", nil, gen1, 9, "make"), nil, []cache.Supplier{gen})},
			history: []cache.HistoryEntry{entryOf(t, "y", layer("img", nil, gen1, 5, "make"), nil,
				[]cache.Supplier{gen})},
			want: attribution{cause: CauseDependency, dependencies: []string{`task "gen"`}},
		},
		{
			name: "a base layer that changed is dependency",
			facts: missFacts{named: true, baseDesc: `layer "runtime"`,
				entry: entryOf(t, "x", layer("img2", nil, nil, 0, "make"), runtime, nil)},
			history: []cache.HistoryEntry{entryOf(t, "y", layer("img1", nil, nil, 0, "make"), runtime, nil)},
			want:    attribution{cause: CauseDependency, dependencies: []string{`layer "runtime"`}},
		},
		{
			name: "each supplier that changed counts, so the nearest entry is the one that differs least",
			facts: missFacts{named: true, baseDesc: `pull "base"`, addDescs: []string{`task "gen"`, `task "gen"`},
				entry: entryOf(t, "x", twoAdds("img2", gen1), pull("b:1", "m2"),
					[]cache.Supplier{gen, gen})},
			history: []cache.HistoryEntry{
				// the latest: two adds from other targets
				entryOf(t, "latest", twoAdds("img2", gen2), pull("b:1", "m2"),
					[]cache.Supplier{{Target: "//:a"}, {Target: "//:b"}}),
				// the nearest: only the base pull moved
				entryOf(t, "nearest", twoAdds("img1", gen1), pull("b:1", "m1"),
					[]cache.Supplier{gen, gen}),
			},
			want: attribution{cause: CauseUpstream, pull: `pull "base"`, before: digest.FromString("m1"),
				after: digest.FromString("m2")},
		},
		{
			name: "a moved cache mount is own-inputs, naming no file",
			facts: missFacts{named: true,
				entry: entryOf(t, "x", mounting(layer("img", nil, nil, 0, "make"), "/other", "//:tool"), nil, nil)},
			history: []cache.HistoryEntry{
				entryOf(t, "y", mounting(layer("img", nil, nil, 0, "make"), "/cache", "//:tool"), nil, nil),
			},
			want: attribution{cause: CauseOwnInputs},
		},
		{
			name: "another cache mounted is own-inputs",
			facts: missFacts{named: true,
				entry: entryOf(t, "x", mounting(layer("img", nil, nil, 0, "make"), "/cache", "//:npm"), nil, nil)},
			history: []cache.HistoryEntry{
				entryOf(t, "y", mounting(layer("img", nil, nil, 0, "make"), "/cache", "//:go"), nil, nil),
			},
			want: attribution{cause: CauseOwnInputs},
		},
		{
			name: "a cache's other owner and group is own-inputs",
			facts: missFacts{named: true, entry: entryOf(t, "x", func() *Action {
				a := mounting(layer("img", nil, nil, 0, "make"), "/cache", "//:tool")
				a.ToolCaches[0].Owner, a.ToolCaches[0].Group = 1000, 1000
				return a
			}(), nil, nil)},
			history: []cache.HistoryEntry{
				entryOf(t, "y", mounting(layer("img", nil, nil, 0, "make"), "/cache", "//:tool"), nil, nil),
			},
			want: attribution{cause: CauseOwnInputs},
		},
		{
			// Two mounts moved are one difference, nearer than two files changed in the more recent entry, which two
			// differences of mounts would tie with, and so be compared.
			name: "the caches a target mounts count as one difference",
			facts: missFacts{named: true, entry: entryOf(t, "x",
				mounting(layer("img", srcOne, nil, 0, "make"), "/a", "//:a", "/b", "//:b"), nil,
				[]cache.Supplier{src(tree("one"))})},
			history: []cache.HistoryEntry{ // most recent first
				entryOf(t, "edited", mounting(layer("img", srcTwo, nil, 0, "make"), "/a", "//:a", "/b", "//:b"), nil,
					[]cache.Supplier{src(tree("ab"))}),
				entryOf(t, "moved", mounting(layer("img", srcOne, nil, 0, "make"), "/a2", "//:a", "/b2", "//:b"), nil,
					[]cache.Supplier{src(tree("one"))}),
			},
			trees: map[digest.Digest][]cache.HistoryFile{
				tree("one"): files("a.txt", "b.txt"),
				tree("ab"): {
					{Path: "a.txt", Type: "file", Mode: 0o644, Digest: digest.FromString("a.txt, edited")},
					{Path: "b.txt", Type: "file", Mode: 0o644, Digest: digest.FromString("b.txt, edited")},
				},
			},
			want: attribution{cause: CauseOwnInputs},
		},
		{
			name: "a base switched to another pull is own-inputs",
			facts: missFacts{named: true,
				entry: entryOf(t, "x", layer("img2", nil, nil, 0, "make"), other, nil)},
			history: []cache.HistoryEntry{entryOf(t, "y", layer("img1", nil, nil, 0, "make"), pull("b:1", "m1"), nil)},
			want:    attribution{cause: CauseOwnInputs},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var h *cache.History
			if tc.history != nil {
				h = &cache.History{Entries: tc.history, Trees: tc.trees}
			}
			got := attribute(tc.facts, h)
			if got.cause != tc.want.cause || !slices.Equal(got.added, tc.want.added) ||
				!slices.Equal(got.removed, tc.want.removed) || !slices.Equal(got.changed, tc.want.changed) ||
				got.sourceChanged != tc.want.sourceChanged || got.pull != tc.want.pull ||
				got.before != tc.want.before || got.after != tc.want.after ||
				!slices.Equal(got.dependencies, tc.want.dependencies) {
				t.Errorf("attribute = %+v; want %+v", got, tc.want)
			}
		})
	}
}

// TestAttributionArgs checks that a miss's span names at most maxReportedFiles of each list of files, with the count
// of all.
func TestAttributionArgs(t *testing.T) {
	var many []string
	for i := range maxReportedFiles + 5 {
		many = append(many, string(rune('a'+i%26))+string(rune('0'+i/26)))
	}
	args := attribution{cause: CauseOwnInputs, added: many}.args()
	got := map[string]any{}
	for i := 0; i+1 < len(args); i += 2 {
		got[args[i].(string)] = args[i+1]
	}
	if names, _ := got["added"].([]string); len(names) != maxReportedFiles || got["added count"] != len(many) {
		t.Errorf("args %v; want %d of the files added and their count, %d", got, maxReportedFiles, len(many))
	}
}
