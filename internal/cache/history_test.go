package cache

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
)

// entry returns a history entry of the key named name whose sources' trees are trees.
func entry(name string, trees ...digest.Digest) HistoryEntry {
	e := HistoryEntry{Key: digest.FromString(name), ActionVersion: 1, Action: json.RawMessage(`{}`)}
	for _, t := range trees {
		e.Adds = append(e.Adds, Supplier{Tree: t})
	}
	return e
}

// files returns n files whose paths take about size bytes each.
func files(n, size int) []HistoryFile {
	out := make([]HistoryFile, n)
	for i := range out {
		out[i] = HistoryFile{Path: fmt.Sprintf("%d/%s", i, strings.Repeat("p", size)), Type: "file"}
	}
	return out
}

// keys returns the names of h's entries' keys, as entry gave them, most recent first.
func keys(h *History, names ...string) []string {
	byKey := map[digest.Digest]string{}
	for _, n := range names {
		byKey[digest.FromString(n)] = n
	}
	var out []string
	for _, e := range h.Entries {
		out = append(out, byKey[e.Key])
	}
	return out
}

// TestHistoryAdd checks what an add keeps: the newest first, each key once, at most keep entries,
// and the trees that some entry names.
func TestHistoryAdd(t *testing.T) {
	tree1, tree2 := digest.FromString("tree1"), digest.FromString("tree2")
	h := &History{}
	now := time.Now()
	h.add(entry("a", tree1), map[digest.Digest][]HistoryFile{tree1: files(1, 1)}, 3, MaxHistorySize, now)
	h.add(entry("b", tree1), nil, 3, MaxHistorySize, now) // shares tree1, which the record holds
	h.add(entry("c", tree2), map[digest.Digest][]HistoryFile{tree2: files(1, 1)}, 3, MaxHistorySize, now)
	h.add(entry("a", tree1), nil, 3, MaxHistorySize, now) // again, so it moves to the front
	if got := keys(h, "a", "b", "c"); !slices.Equal(got, []string{"a", "c", "b"}) {
		t.Errorf("entries %v; want [a c b]", got)
	}
	if len(h.Trees) != 2 {
		t.Errorf("%d trees; want tree1 and tree2, once each", len(h.Trees))
	}
	h.add(entry("d"), nil, 2, MaxHistorySize, now)
	if got := keys(h, "a", "b", "c", "d"); !slices.Equal(got, []string{"d", "a"}) {
		t.Errorf("entries kept 2 = %v; want [d a]", got)
	}
	if _, ok := h.Trees[tree2]; ok || len(h.Trees) != 1 {
		t.Errorf("trees %v; want only tree1, which a remains to name", slices.Collect(maps.Keys(h.Trees)))
	}
	if h.Newest != entry("d").Fingerprint() || !h.Used.Equal(now.UTC()) {
		t.Errorf("newest %s at %v; want d's fingerprint at %v", h.Newest, h.Used, now)
	}
}

// TestHistoryCap checks that an add drops the oldest entries until the record fits, keeps the newest entry's
// largest trees by digest alone if it doesn't fit with them, and adds no entry that doesn't fit even so.
func TestHistoryCap(t *testing.T) {
	const limit = 64 << 10
	now := time.Now()
	t.Run("the oldest entries go", func(t *testing.T) {
		h := &History{}
		for i := range 5 {
			tree := digest.FromString(fmt.Sprint("tree", i))
			h.add(entry(fmt.Sprint(i), tree), map[digest.Digest][]HistoryFile{tree: files(100, 200)}, 8, limit, now)
		}
		if got := keys(h, "0", "1", "2", "3", "4"); !slices.Equal(got, []string{"4", "3"}) {
			t.Errorf("entries %v; want the newest two, [4 3]", got)
		}
		if size := historySize(h); size > limit {
			t.Errorf("the record takes %d bytes; want at most %d", size, limit)
		}
	})
	t.Run("the last tree's field counts towards the fit", func(t *testing.T) {
		// Dropping the old entry and its tree, the record's only one, leaves the new entry alone at the cap exactly.
		tree := digest.FromString("tree")
		later := now.Add(time.Hour)
		exact := &History{Newest: entry("new").Fingerprint(), Used: later.UTC(), Entries: []HistoryEntry{entry("new")}}
		h := &History{}
		h.add(entry("old", tree), map[digest.Digest][]HistoryFile{tree: files(1, 1)}, 8, MaxHistorySize, now)
		h.add(entry("new"), nil, 8, historySize(exact), later)
		if got := keys(h, "old", "new"); !slices.Equal(got, []string{"new"}) {
			t.Errorf("entries %v; want [new], which fits the cap exactly", got)
		}
	})
	t.Run("a stripped last tree's field counts towards the fit", func(t *testing.T) {
		// Stripping the new entry's only tree leaves it at the cap exactly, so it is added, by the tree's digest.
		tree := digest.FromString("tree")
		e := entry("new", tree)
		exact := &History{Newest: e.Fingerprint(), Used: now.UTC(), Entries: []HistoryEntry{e}}
		h := &History{}
		h.add(e, map[digest.Digest][]HistoryFile{tree: files(10, 10)}, 8, historySize(exact), now)
		if got := keys(h, "new"); !slices.Equal(got, []string{"new"}) || h.Trees != nil {
			t.Errorf("entries %v with %d trees; want [new] with its tree by digest alone", got, len(h.Trees))
		}
	})
	t.Run("an oversized tree is kept by digest", func(t *testing.T) {
		small, large, larger := digest.FromString("small"), digest.FromString("large"), digest.FromString("larger")
		h := &History{}
		h.add(entry("old", small), map[digest.Digest][]HistoryFile{small: files(1, 1)}, 8, limit, now)
		h.add(entry("new", small, large, larger), map[digest.Digest][]HistoryFile{
			large:  files(100, 300),
			larger: files(100, 400),
		}, 8, limit, now)
		if got := keys(h, "old", "new"); !slices.Equal(got, []string{"new", "old"}) {
			t.Errorf("entries %v; want [new old]", got)
		}
		if _, ok := h.Trees[larger]; ok {
			t.Error("the larger tree, too large to keep beside the other, is kept whole")
		}
		if _, ok := h.Trees[large]; !ok {
			t.Error("the large tree, which fits once the larger goes, isn't kept")
		}
		if _, ok := h.Trees[small]; !ok {
			t.Error("the small tree isn't kept")
		}
	})
	t.Run("an entry too large even so isn't added", func(t *testing.T) {
		shared := digest.FromString("shared")
		h := &History{}
		h.add(entry("old", shared), map[digest.Digest][]HistoryFile{shared: files(1, 1)}, 8, limit, now)
		big := entry("big", shared)
		big.Action = json.RawMessage(`"` + strings.Repeat("x", limit) + `"`)
		later := now.Add(time.Hour)
		h.add(big, nil, 8, limit, later)
		if got := keys(h, "old", "big"); !slices.Equal(got, []string{"old"}) {
			t.Errorf("entries %v; want [old]", got)
		}
		if h.Newest != big.Fingerprint() || !h.Used.Equal(later.UTC()) {
			t.Error("an entry too large to keep isn't the newest, or didn't move Used")
		}
		if _, ok := h.Trees[shared]; !ok {
			t.Error("the old entry lost the files of a tree it shares with the entry too large to keep")
		}
	})
}

// TestHistoryHead checks that a record's leading fields are read without its entries, which may not even parse.
func TestHistoryHead(t *testing.T) {
	h := &History{}
	used := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	h.add(entry("a"), nil, 8, MaxHistorySize, used)
	data, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	cut := data[:bytes.Index(data, []byte(`"entries"`))+len(`"entries":[{`)] // the entries cut short
	head, ok := decodeHistoryHead(bytes.NewReader(cut))
	if !ok || head.Newest != h.Newest || !head.Used.Equal(used) {
		t.Errorf("head = %+v, %v; want %s at %v", head, ok, h.Newest, used)
	}
	if _, ok := decodeHistoryHead(strings.NewReader("not JSON")); ok {
		t.Error("a head was read from what isn't JSON")
	}
}

// TestFingerprint checks that an entry's fingerprint changes with its key and what supplied it,
// but not with its sources' trees, which the key covers, nor with when it was recorded or its action.
func TestFingerprint(t *testing.T) {
	base := entry("a", digest.FromString("tree"))
	base.Base = &Supplier{Target: "//:base", Image: "busybox:1.36", Manifest: digest.FromString("m1")}
	base.Adds = append(base.Adds, Supplier{Target: "//:gen"})
	with := func(change func(e *HistoryEntry)) HistoryEntry {
		e := base
		e.Base = new(*base.Base)
		e.Adds = slices.Clone(base.Adds)
		change(&e)
		return e
	}
	for name, e := range map[string]HistoryEntry{
		"another tree":           with(func(e *HistoryEntry) { e.Adds[0].Tree = digest.FromString("other") }),
		"recorded later":         with(func(e *HistoryEntry) { e.Recorded = time.Now() }),
		"another action":         with(func(e *HistoryEntry) { e.Action = json.RawMessage(`{"x":1}`) }),
		"another action version": with(func(e *HistoryEntry) { e.ActionVersion++ }),
	} {
		if e.Fingerprint() != base.Fingerprint() {
			t.Errorf("an entry with %s has another fingerprint", name)
		}
	}
	for name, e := range map[string]HistoryEntry{
		"another key":           with(func(e *HistoryEntry) { e.Key = digest.FromString("b") }),
		"another base target":   with(func(e *HistoryEntry) { e.Base.Target = "//:other" }),
		"another base image":    with(func(e *HistoryEntry) { e.Base.Image = "busybox:1.37" }),
		"a moved base manifest": with(func(e *HistoryEntry) { e.Base.Manifest = digest.FromString("m2") }),
		"another add's target":  with(func(e *HistoryEntry) { e.Adds[1].Target = "//:other" }),
		"no base":               with(func(e *HistoryEntry) { e.Base = nil }),
	} {
		if e.Fingerprint() == base.Fingerprint() {
			t.Errorf("an entry with %s has the same fingerprint", name)
		}
	}
}
