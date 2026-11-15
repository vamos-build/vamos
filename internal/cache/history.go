package cache

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"time"

	"github.com/opencontainers/go-digest"
)

// MaxHistorySize bounds a target's history record whole, so that a target with very large sources keeps fewer
// entries rather than a record without bound.
const MaxHistorySize = 16 << 20

// HistoryRetention is how long a history record is kept since it was last used, by a build or a reuse of its target:
// a prune removes the records unused for longer.
const HistoryRetention = 100 * 24 * time.Hour

// History is a target's history: the latest actions that builds of it used, most recent first,
// and what attribution compares them by.
// It names no content blob, so that it survives a prune.
// Its fields are encoded in this order, so that Newest and Used can be read without the rest.
type History struct {
	// Newest is the fingerprint of the entry added last, which is the first entry's,
	// unless that entry was too large to keep.
	Newest digest.Digest `json:"newest,omitempty"`
	Used   time.Time     `json:"used"` // when an add last published the record
	// Entries are the actions, most recent first, each key at most once.
	Entries []HistoryEntry `json:"entries"`
	// Trees holds the files of the entries' sources by their trees' digests, so that entries share them.
	// A tree an entry names that isn't here is kept by its digest alone, as one too large to keep is.
	Trees map[digest.Digest][]HistoryFile `json:"trees,omitempty"`
}

// HistoryHead is what a history record's leading fields say, which Newest reads without the rest.
type HistoryHead struct {
	Newest digest.Digest
	Used   time.Time
}

// HistoryEntry is one action that a build of a target used, reused or made, and what supplied its inputs.
type HistoryEntry struct {
	Key           digest.Digest `json:"key"`
	ActionVersion int           `json:"action_version"` // of what an action covers, as the scheduler encodes it
	Recorded      time.Time     `json:"recorded"`
	// Action is the action, as the scheduler encodes it, which this package doesn't read.
	Action json.RawMessage `json:"action"`
	Base   *Supplier       `json:"base,omitempty"` // what supplied the base, if it isn't scratch
	Adds   []Supplier      `json:"adds,omitempty"` // what supplied each add step's files, in order
}

// Supplier says what supplied one of an action's inputs: a pull, layer or task by its target,
// or a source by its tree.
type Supplier struct {
	Target   string        `json:"target,omitempty"`   // the configured target of a pull, layer or task
	Image    string        `json:"image,omitempty"`    // a pull's image reference
	Manifest digest.Digest `json:"manifest,omitempty"` // the manifest a pull's reference named
	Tree     digest.Digest `json:"tree,omitempty"`     // a source's tree
}

// HistoryFile is one entry of a source's tree, as attribution compares it.
type HistoryFile struct {
	Path   string        `json:"path"`
	Type   string        `json:"type"`
	Mode   uint32        `json:"mode,omitempty"`
	Target string        `json:"target,omitempty"`
	Digest digest.Digest `json:"digest,omitempty"` // of a file's content
}

// HistoryFiles returns tree's entries as a history keeps them.
func HistoryFiles(tree *Tree) []HistoryFile {
	files := make([]HistoryFile, len(tree.Entries))
	for i, e := range tree.Entries {
		files[i] = HistoryFile{Path: e.Path, Type: e.Type, Mode: e.Mode, Target: e.Target}
		if e.Blob != nil {
			files[i].Digest = e.Blob.Digest
		}
	}
	return files
}

// Fingerprint identifies the entry by its key and what supplied its inputs, but its sources' trees,
// which the key covers already, so that a reuse of the same result through another supplier is told apart.
func (e HistoryEntry) Fingerprint() digest.Digest {
	identity := func(s Supplier) Supplier {
		return Supplier{Target: s.Target, Image: s.Image, Manifest: s.Manifest}
	}
	f := struct {
		Key  digest.Digest `json:"key"`
		Base *Supplier     `json:"base,omitempty"`
		Adds []Supplier    `json:"adds,omitempty"`
	}{Key: e.Key}
	if e.Base != nil {
		base := identity(*e.Base)
		f.Base = &base
	}
	for _, a := range e.Adds {
		f.Adds = append(f.Adds, identity(a))
	}
	data, _ := json.Marshal(f) // of strings alone, which always encode
	return digest.FromBytes(data)
}

// trees returns the digests of the trees the entry's sources name.
func (e HistoryEntry) trees() []digest.Digest {
	var trees []digest.Digest
	for _, a := range e.Adds {
		if a.Tree != "" {
			trees = append(trees, a.Tree)
		}
	}
	return trees
}

// Validate checks that the entry's digests are well formed, its action is JSON,
// and when it was recorded can be encoded, so that a record of valid entries always encodes.
func (e HistoryEntry) Validate() error {
	if err := validateKey(e.Key); err != nil {
		return fmt.Errorf("history entry: %w", err)
	}
	if !json.Valid(e.Action) {
		return fmt.Errorf("history entry %s: its action isn't JSON", e.Key)
	}
	if _, err := e.Recorded.MarshalJSON(); err != nil {
		return fmt.Errorf("history entry %s: when it was recorded: %w", e.Key, err)
	}
	suppliers := e.Adds
	if e.Base != nil {
		suppliers = append([]Supplier{*e.Base}, suppliers...)
	}
	for _, s := range suppliers {
		for _, d := range []digest.Digest{s.Manifest, s.Tree} {
			if d == "" {
				continue
			}
			if err := d.Validate(); err != nil {
				return fmt.Errorf("history entry %s: %q: %w", e.Key, d, err)
			}
		}
	}
	return nil
}

// Validate checks that what the record names is well formed.
func (h *History) Validate() error {
	if h == nil {
		return errors.New("no history")
	}
	if h.Newest != "" {
		if err := h.Newest.Validate(); err != nil {
			return fmt.Errorf("history's newest %q: %w", h.Newest, err)
		}
	}
	for _, e := range h.Entries {
		if err := e.Validate(); err != nil {
			return err
		}
	}
	return validateHistoryTrees(h.Trees)
}

// validateHistoryTrees checks that trees' digests and their files are well formed.
func validateHistoryTrees(trees map[digest.Digest][]HistoryFile) error {
	for d, files := range trees {
		if err := d.Validate(); err != nil {
			return fmt.Errorf("history's tree %q: %w", d, err)
		}
		for _, f := range files {
			if f.Path == "" {
				return fmt.Errorf("history's tree %s: an entry without a path", d)
			}
			if f.Digest != "" {
				if err := f.Digest.Validate(); err != nil {
					return fmt.Errorf("history's tree %s: %s: %w", d, f.Path, err)
				}
			}
		}
	}
	return nil
}

// validateAdd checks what AddHistory is given, so that no add can publish a record that a read then refuses.
func validateAdd(name digest.Digest, entry HistoryEntry, trees map[digest.Digest][]HistoryFile) error {
	if err := validateKey(name); err != nil {
		return err
	}
	if err := entry.Validate(); err != nil {
		return err
	}
	return validateHistoryTrees(trees)
}

// historySize returns the size of h encoded.
func historySize(h *History) int {
	data, err := json.Marshal(h)
	if err != nil {
		// Validate refuses what can't be encoded, so this can't happen;
		// were it to, the add would go on, and encodeHistory refuse the record, publishing nothing.
		return 0
	}
	return len(data)
}

// add adds e to h as AddHistory does, with trees, the files of the trees e names that h may lack,
// keeping at most keep entries and h within limit bytes, and published at now.
// The newest entry is measured with its trees on every add; each tree and older entry is measured only when
// something must go, and the record's whole again to confirm a fit.
func (h *History) add(e HistoryEntry, trees map[digest.Digest][]HistoryFile, keep, limit int, now time.Time) {
	h.Newest, h.Used = e.Fingerprint(), now.UTC()
	if h.Trees == nil {
		h.Trees = map[digest.Digest][]HistoryFile{}
	}

	// The newest entry alone, with every tree it names that h holds or trees gives: does it fit,
	// once its largest trees are kept by digest alone, if need be?
	alone := &History{Newest: h.Newest, Used: h.Used, Entries: []HistoryEntry{e}}
	alone.Trees = map[digest.Digest][]HistoryFile{}
	for _, d := range e.trees() {
		if files, ok := trees[d]; ok {
			alone.Trees[d] = files
		} else if files, ok := h.Trees[d]; ok {
			alone.Trees[d] = files
		}
	}
	size := historySize(alone)
	var stripped []digest.Digest
	if size > limit {
		type candidate struct {
			d    digest.Digest
			cost int
		}
		var largestFirst []candidate
		for d, files := range alone.Trees {
			largestFirst = append(largestFirst, candidate{d, treeCost(d, files)})
		}
		slices.SortFunc(largestFirst, func(a, b candidate) int {
			return cmp.Or(cmp.Compare(b.cost, a.cost), cmp.Compare(a.d, b.d))
		})
		for _, c := range largestFirst {
			if size <= limit {
				break
			}
			size -= c.cost + lastTreeCost(len(alone.Trees))
			delete(alone.Trees, c.d)
			stripped = append(stripped, c.d)
		}
		size = historySize(alone) // which the estimate should equal: confirm it
	}
	fits := size <= limit

	var entries []HistoryEntry
	if fits {
		entries = append(entries, e)
		maps.Copy(h.Trees, alone.Trees)
		for _, d := range stripped {
			delete(h.Trees, d) // kept by digest alone by every entry that names it, since it doesn't fit beside e
		}
	}
	for _, old := range h.Entries {
		if !fits || old.Key != e.Key {
			entries = append(entries, old)
		}
	}
	h.Entries = entries[:min(len(entries), max(keep, 0))]
	h.dropUnusedTrees()

	// Drop the oldest entries, and the trees no other entry names, until the record fits.
	// What dropping an entry saves is its size and comma, which counts a byte too many for the last entry,
	// so that the estimate never errs towards dropping one more: the record's size confirms each fit.
	uses := map[digest.Digest]int{}
	for _, entry := range h.Entries {
		for _, d := range entry.trees() {
			uses[d]++
		}
	}
	size = historySize(h)
	for size > limit && len(h.Entries) > 0 {
		oldest := h.Entries[len(h.Entries)-1]
		h.Entries = h.Entries[:len(h.Entries)-1]
		data, _ := json.Marshal(oldest) // which encodes, as Validate checked
		size -= len(data) + len(",")
		for _, d := range oldest.trees() {
			uses[d]--
			if files, ok := h.Trees[d]; ok && uses[d] == 0 {
				size -= treeCost(d, files) + lastTreeCost(len(h.Trees))
				delete(h.Trees, d)
			}
		}
		if size <= limit {
			size = historySize(h)
		}
	}
	if len(h.Trees) == 0 {
		h.Trees = nil
	}
}

// treeCost returns what the tree d of files adds to a record's encoding: its key, its files and a comma.
func treeCost(d digest.Digest, files []HistoryFile) int {
	data, _ := json.Marshal(files) // of strings and numbers alone, which always encode
	return len(`"":,`) + len(d) + len(data)
}

// lastTreeCost returns what removing one of n trees saves beyond its treeCost:
// for the last, the field that holds them, which an empty map leaves out, less the comma it lacked.
func lastTreeCost(n int) int {
	if n == 1 {
		return len(`,"trees":{}`) - len(",")
	}
	return 0
}

// decodeHistory decodes a history record, reporting false if it can't, which is as good as none.
func decodeHistory(data []byte) (*History, bool) {
	var h History
	if data == nil || len(data) > MaxHistorySize || json.Unmarshal(data, &h) != nil || h.Validate() != nil {
		return nil, false
	}
	return &h, true
}

// encodeHistory returns h encoded, failing if it wouldn't decode again,
// as an action nested too deeply to fit inside a record wouldn't, so that no add publishes a record a read refuses.
func encodeHistory(h *History) ([]byte, error) {
	data, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	if _, ok := decodeHistory(data); !ok {
		return nil, errors.New("the target history would not read back")
	}
	return data, nil
}

// dropUnusedTrees removes the trees that no entry names.
func (h *History) dropUnusedTrees() {
	used := map[digest.Digest]bool{}
	for _, e := range h.Entries {
		for _, d := range e.trees() {
			used[d] = true
		}
	}
	maps.DeleteFunc(h.Trees, func(d digest.Digest, _ []HistoryFile) bool { return !used[d] })
	if len(h.Trees) == 0 {
		h.Trees = nil
	}
}

// decodeHistoryHead reads a history record's leading fields from r, stopping before its entries,
// and reports false if they can't be read, which is as good as no record.
func decodeHistoryHead(r io.Reader) (HistoryHead, bool) {
	dec := json.NewDecoder(r)
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return HistoryHead{}, false
	}
	var head HistoryHead
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return HistoryHead{}, false
		}
		switch t {
		case "newest":
			if dec.Decode(&head.Newest) != nil || head.Newest.Validate() != nil {
				return HistoryHead{}, false
			}
		case "used":
			if dec.Decode(&head.Used) != nil {
				return HistoryHead{}, false
			}
		default:
			return head, true // the entries, which the head doesn't need
		}
	}
	return head, true
}
