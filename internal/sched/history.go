package sched

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/graph"
	"github.com/vamos-build/vamos/internal/tracing"

	"github.com/opencontainers/go-digest"
)

// HistoryKeep is how many distinct actions a target's history keeps.
const HistoryKeep = 8

// historyRefresh is how old a history's Used may be before a hit adds its entry again, though it is the newest,
// so that a target reused every day is kept by a prune; HistoryRetention less this is the shortest it survives.
const historyRefresh = 7 * 24 * time.Hour

// maxReportedFiles is how many paths a miss's span names of each of the files added, removed and changed.
const maxReportedFiles = 100

// The causes of a miss.
const (
	CauseForced     = "forced"     // --rebuild named the target
	CauseNew        = "new"        // no earlier build of the target to compare with
	CauseLost       = "lost"       // the same action as before, whose result is gone or can't be used
	CauseOwnInputs  = "own-inputs" // a step, command or source of its own changed, or what supplies an input
	CauseUpstream   = "upstream"   // only the image its base pull's reference names moved
	CauseDependency = "dependency" // only what a target it depends on supplies changed
)

// HistoryName returns the name of the history of the configured target target in the workspace named workspace.
func HistoryName(workspace, target string) digest.Digest {
	return digest.FromString(workspace + "\n" + target)
}

// historyEntry returns the entry of the target n, whose action is a and key key, as its history keeps it: the
// action, and what supplied its base and each add step's files, by their targets, a pull's image reference and the
// manifest it named, and a source's tree; with the blobs of the sources' trees, by digest.
// baseIndex is the manifest that the base names, if it is a pull, which the walk captured before dispatching the
// target, so that this reads none of the Scheduler's maps, which the walk writes meanwhile.
func (s *Scheduler) historyEntry(n *graph.Node, a *Action, key digest.Digest, baseIndex digest.Digest) (
	cache.HistoryEntry, map[digest.Digest]cache.Blob, error) {
	data, err := json.Marshal(a)
	if err != nil {
		return cache.HistoryEntry{}, nil, err
	}
	e := cache.HistoryEntry{Key: key, ActionVersion: ActionVersion, Recorded: time.Now().UTC(), Action: data}
	if base := n.Base(); base != graph.Scratch {
		bn := s.graph.Node(base)
		e.Base = &cache.Supplier{Target: bn.Target}
		if bn.Pull != nil {
			e.Base.Image, e.Base.Manifest = bn.Pull.Image, baseIndex
		}
	}
	trees := map[digest.Digest]cache.Blob{}
	for _, in := range n.Steps() {
		if in.From == nil {
			continue
		}
		from := s.graph.Node(*in.From)
		if src := from.Source; src != nil {
			e.Adds = append(e.Adds, cache.Supplier{Tree: src.Tree.Digest})
			trees[src.Tree.Digest] = src.Tree
		} else {
			e.Adds = append(e.Adds, cache.Supplier{Target: from.Target})
		}
	}
	return e, trees, nil
}

// attribution is why a target missed, as attribute finds it, and what its span says of it.
type attribution struct {
	cause string
	// added, removed and changed are the paths of a source's files that own-inputs found, each sorted.
	added, removed, changed []string
	// sourceChanged says a source changed that history kept by its digest alone, whose files it can't name.
	sourceChanged bool
	pull          string        // upstream: the pull that moved
	before, after digest.Digest // upstream: the manifests its reference named
	dependencies  []string      // dependency: the suppliers that changed, by their descriptions
}

// args returns what the target's span says of the attribution, with each list of files capped at
// maxReportedFiles and its count of all.
func (at attribution) args() []any {
	args := []any{"miss", at.cause}
	for _, files := range []struct {
		name  string
		paths []string
	}{{"added", at.added}, {"removed", at.removed}, {"changed", at.changed}} {
		if len(files.paths) > 0 {
			// A copy, so that the span doesn't keep every path alive while its recorder holds it.
			args = append(args, files.name, slices.Clone(files.paths[:min(len(files.paths), maxReportedFiles)]),
				files.name+" count", len(files.paths))
		}
	}
	if at.sourceChanged {
		args = append(args, "source changed", true)
	}
	if at.pull != "" {
		args = append(args, "pull", at.pull, "before", at.before.String(), "after", at.after.String())
	}
	if len(at.dependencies) > 0 {
		args = append(args, "dependencies", at.dependencies)
	}
	return args
}

// missFacts is what attribute knows of a target that missed, besides its history.
type missFacts struct {
	forced bool // --rebuild named it
	// lost says a result was found for it but couldn't be used, at lookup or when a target that adds its files found
	// them unusable and had it run again.
	lost  bool
	named bool // the workspace is named and the target has a Target, so that it keeps history
	entry cache.HistoryEntry
	// trees are the files of the current entry's sources that history doesn't hold, by tree digest.
	trees map[digest.Digest][]cache.HistoryFile
	// baseDesc and addDescs describe the suppliers of the base and each add step, as the current graph does.
	baseDesc string
	addDescs []string
}

// attribute returns why the target that facts describes missed, given its history, which may be nil:
// forced, if --rebuild named it; lost, if a result was found but couldn't be used, or history holds its action;
// new, if it keeps no history, or history holds no entry of the current ActionVersion;
// and otherwise what differs from the nearest such entry, the most recent among the nearest: own-inputs, if its own
// kind, steps, command, entrypoint, output, a source, or which target supplies an input does; upstream, if only its
// base pull's manifest does; and dependency, naming each supplier whose image, files or epoch does.
func attribute(facts missFacts, h *cache.History) attribution {
	switch {
	case facts.forced:
		return attribution{cause: CauseForced}
	case facts.lost:
		return attribution{cause: CauseLost}
	}
	if h != nil && slices.ContainsFunc(h.Entries, func(e cache.HistoryEntry) bool { return e.Key == facts.entry.Key }) {
		return attribution{cause: CauseLost}
	}
	if !facts.named || h == nil {
		return attribution{cause: CauseNew}
	}
	var current Action
	if err := json.Unmarshal(facts.entry.Action, &current); err != nil {
		return attribution{cause: CauseNew}
	}
	var nearest *actionDiff
	for _, e := range h.Entries { // most recent first, so that ties go to the most recent
		if e.ActionVersion != facts.entry.ActionVersion {
			continue
		}
		var earlier Action
		if json.Unmarshal(e.Action, &earlier) != nil {
			continue
		}
		d := diffActions(&current, &earlier, facts, e, h)
		if nearest == nil || d.distance() < nearest.distance() {
			nearest = &d
		}
	}
	if nearest == nil {
		return attribution{cause: CauseNew}
	}
	return nearest.attribution()
}

// actionDiff is how an action differs from an earlier one of the same target, keeping the target's own
// configuration apart from what its suppliers gave it, which distance, cause and reported paths all derive from.
type actionDiff struct {
	own int // the kind, steps by position, command, entrypoint, output source and selection that differ
	// added, removed and changed are the paths of sources' files that differ; sourceChanged counts sources that
	// history kept by digest alone, and so can't list.
	added, removed, changed []string
	sourceChanged           int
	supplierChanged         int  // the base and adds that a different target supplies
	baseMoved               bool // the base pull is the same, of the same reference, but named another manifest
	pull                    string
	before, after           digest.Digest
	baseChanged             bool     // the base's image differs otherwise: a layer's, or a pull's edited reference
	tasks                   int      // the added tasks whose files or epoch differ
	dependencies            []string // the suppliers whose image, files or epoch differ, by description
}

// distance counts the differences, one for each of the target's own that differs, for each path of a source added,
// removed or changed, for each source kept by digest that changed, for a supplier that differs, for the base, and for
// each added task whose files or epoch differ.
func (d actionDiff) distance() int {
	n := d.own + len(d.added) + len(d.removed) + len(d.changed) + d.sourceChanged + d.tasks + d.supplierChanged
	if d.baseMoved || d.baseChanged {
		n++
	}
	return n
}

// attribution returns the cause the difference makes, with what the span says of it.
func (d actionDiff) attribution() attribution {
	if d.own > 0 || d.supplierChanged > 0 || len(d.added)+len(d.removed)+len(d.changed) > 0 || d.sourceChanged > 0 {
		return attribution{cause: CauseOwnInputs, added: d.added, removed: d.removed, changed: d.changed,
			sourceChanged: d.sourceChanged > 0}
	}
	if d.baseMoved && len(d.dependencies) == 1 {
		return attribution{cause: CauseUpstream, pull: d.pull, before: d.before, after: d.after}
	}
	if len(d.dependencies) > 0 {
		return attribution{cause: CauseDependency, dependencies: d.dependencies}
	}
	// The same action and suppliers, as history kept them, yet another key: what history can't see, as a pull's
	// image of the same manifest made again, so the closest it can say is that a supplier changed.
	return attribution{cause: CauseDependency}
}

// diffActions returns how current, the action the facts describe, differs from earlier, of the entry e of h.
func diffActions(current, earlier *Action, facts missFacts, e cache.HistoryEntry, h *cache.History) actionDiff {
	var d actionDiff
	for _, same := range []bool{
		current.Kind == earlier.Kind,
		slices.Equal(current.Command, earlier.Command),
		equalEntrypoints(current.Entrypoint, earlier.Entrypoint),
		current.Source == earlier.Source,
		equalJSON(current.Output, earlier.Output),
		// The tool caches it mounts, where, and with which owner and group, counted as one difference.
		slices.Equal(current.ToolCaches, earlier.ToolCaches),
	} {
		if !same {
			d.own++
		}
	}
	for i := range max(len(current.Steps), len(earlier.Steps)) {
		if i >= len(current.Steps) || i >= len(earlier.Steps) || !sameStep(current.Steps[i], earlier.Steps[i]) {
			d.own++
		}
	}

	// The base.
	cur, old := facts.entry.Base, e.Base
	switch {
	case (cur == nil) != (old == nil) || cur != nil && cur.Target != old.Target:
		d.supplierChanged++
	case cur == nil:
	case cur.Image != old.Image:
		d.baseChanged = true // a pull whose reference was edited: its declaration changed
		d.dependencies = append(d.dependencies, facts.baseDesc)
	case cur.Manifest != old.Manifest:
		d.baseMoved, d.pull, d.before, d.after = true, facts.baseDesc, old.Manifest, cur.Manifest
		d.dependencies = append(d.dependencies, facts.baseDesc)
	case current.Base != earlier.Base:
		d.baseChanged = true // a layer, or a pull of the same manifest, that made another image
		d.dependencies = append(d.dependencies, facts.baseDesc)
	}

	// The adds, by position among add steps.
	curAdds, oldAdds := addSteps(current), addSteps(earlier)
	for i := range max(len(facts.entry.Adds), len(e.Adds)) {
		if i >= len(facts.entry.Adds) || i >= len(e.Adds) {
			continue // a step count that differs is counted among the steps
		}
		c, o := facts.entry.Adds[i], e.Adds[i]
		switch {
		case (c.Tree != "") != (o.Tree != "") || c.Target != o.Target:
			d.supplierChanged++
		case c.Tree != "":
			if c.Tree == o.Tree {
				continue
			}
			now, nowOK := facts.trees[c.Tree]
			if !nowOK {
				now, nowOK = h.Trees[c.Tree] // the history holds the current tree, which wasn't read again
			}
			then, thenOK := h.Trees[o.Tree]
			if !nowOK || !thenOK {
				d.sourceChanged++
				continue
			}
			added, removed, changed := diffFiles(now, then)
			d.added, d.removed, d.changed = append(d.added, added...), append(d.removed, removed...),
				append(d.changed, changed...)
		case i < len(curAdds) && i < len(oldAdds) &&
			(!sameBlob(curAdds[i].Files, oldAdds[i].Files) || curAdds[i].Epoch != oldAdds[i].Epoch):
			desc := c.Target
			if i < len(facts.addDescs) {
				desc = facts.addDescs[i]
			}
			d.dependencies = append(d.dependencies, desc)
			d.tasks++
		}
	}
	slices.Sort(d.added)
	slices.Sort(d.removed)
	slices.Sort(d.changed)
	return d
}

// sameStep reports whether two steps are the same but for an add's files and epoch, which the sources and the
// tasks it adds count apart.
func sameStep(a, b Step) bool {
	a.Files, b.Files, a.Epoch, b.Epoch = nil, nil, 0, 0
	return equalJSON(a, b)
}

// addSteps returns a's add steps that add files from another node, in order.
func addSteps(a *Action) []Step {
	var adds []Step
	for _, in := range a.Steps {
		if in.Op == graph.OpAdd && in.Files != nil {
			adds = append(adds, in)
		}
	}
	return adds
}

// diffFiles returns the paths of now's files that then lacks, then's that now lacks, and those in both that differ
// in type, mode, link target or content, each unsorted.
func diffFiles(now, then []cache.HistoryFile) (added, removed, changed []string) {
	before := make(map[string]cache.HistoryFile, len(then))
	for _, f := range then {
		before[f.Path] = f
	}
	for _, f := range now {
		old, ok := before[f.Path]
		switch {
		case !ok:
			added = append(added, f.Path)
		case old != f:
			changed = append(changed, f.Path)
		}
		delete(before, f.Path)
	}
	for path := range before {
		removed = append(removed, path)
	}
	return added, removed, changed // which the caller sorts
}

// sameBlob reports whether a and b name the same blob, or neither names one.
func sameBlob(a, b *cache.Blob) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// equalEntrypoints reports whether two layers' entrypoints are the same, nil being unlike an empty one.
func equalEntrypoints(a, b *[]string) bool {
	return a == nil && b == nil || a != nil && b != nil && slices.Equal(*a, *b)
}

// equalJSON reports whether a and b encode alike.
func equalJSON(a, b any) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(x) == string(y)
}

// historyFailure returns err, a failure to read or add a target's history, as a span's error says it.
func historyFailure(err error) error {
	return fmt.Errorf("target history: %w", err)
}

// pendingAdd is a target's entry to add to its history once it succeeds, or nothing for one that keeps none.
type pendingAdd struct {
	s     *Scheduler
	cause string
	name  digest.Digest
	entry *cache.HistoryEntry
	trees map[digest.Digest][]cache.HistoryFile // the files of the entry's sources that its history lacks
}

// add adds the entry to the target's history, reporting a failure on span, which fails nothing.
func (p pendingAdd) add(ctx context.Context, span *tracing.Active) {
	if p.entry == nil {
		return
	}
	if err := p.s.records.AddHistory(ctx, p.name, *p.entry, p.trees, HistoryKeep); err != nil {
		span.Set("history error", historyFailure(err).Error())
	}
}

// keepsHistory returns the name of the target n's history, and whether it keeps one: in a named workspace, of a
// node that names its Target, as an older client's doesn't.
func (s *Scheduler) keepsHistory(n *graph.Node) (digest.Digest, bool) {
	if s.opts.Workspace == "" || n.Target == "" {
		return "", false
	}
	return HistoryName(s.opts.Workspace, n.Target), true
}

// attributeMiss finds why the target n, of the action a and key key, missed, reading its history once, sets the
// cause and what the span says of it on span, and returns the entry to add once it succeeds.
// regenerated says a result was found that couldn't be used, at lookup or by a target that adds its files.
// A history that can't be read is reported on span, and attributed as none.
func (s *Scheduler) attributeMiss(ctx context.Context, span *tracing.Active, n *graph.Node, a *Action,
	key, baseIndex digest.Digest, forced, regenerated bool) pendingAdd {
	name, named := s.keepsHistory(n)
	facts := missFacts{forced: forced, lost: regenerated, named: named}
	var h *cache.History
	var p pendingAdd
	if named {
		entry, blobs, err := s.historyEntry(n, a, key, baseIndex)
		if err == nil {
			facts.entry = entry
			if h, err = s.records.History(ctx, name); err == nil {
				p.trees, err = s.missingTrees(ctx, h, blobs)
			}
		}
		if err == nil {
			p.name, p.entry = name, &entry
		} else {
			// Nothing is added, rather than an entry without the files of its sources, which the next miss couldn't
			// name; the next build adds it.
			span.Set("history error", historyFailure(err).Error())
		}
		facts.trees = p.trees // and those the history holds, which attribute looks up there
		facts.baseDesc, facts.addDescs = s.supplierDescs(n)
	}
	if !named {
		facts.entry = cache.HistoryEntry{Key: key, ActionVersion: ActionVersion}
	}
	at := attribute(facts, h)
	span.Set(at.args()...)
	p.s, p.cause = s, at.cause
	return p
}

// recordHit adds the entry of the target n, a hit, to its history if its fingerprint isn't the newest, or the
// history wasn't used for historyRefresh, reading only the history's head otherwise.
// A failure is reported on span, and fails nothing.
func (s *Scheduler) recordHit(ctx context.Context, span *tracing.Active, n *graph.Node, a *Action,
	key, baseIndex digest.Digest) {
	name, named := s.keepsHistory(n)
	if !named {
		return
	}
	err := func() error {
		entry, blobs, err := s.historyEntry(n, a, key, baseIndex)
		if err != nil {
			return err
		}
		head, err := s.records.Newest(ctx, name)
		if err != nil {
			return err
		}
		var trees map[digest.Digest][]cache.HistoryFile
		switch fresh := time.Since(head.Used) <= historyRefresh; {
		case head.Newest == entry.Fingerprint() && fresh:
			return nil
		case head.Newest != entry.Fingerprint():
			// Another action, or the same through other suppliers: its sources' trees may be new to the history.
			h, err := s.records.History(ctx, name)
			if err != nil {
				return err
			}
			if trees, err = s.missingTrees(ctx, h, blobs); err != nil {
				return err
			}
		}
		return s.records.AddHistory(ctx, name, entry, trees, HistoryKeep)
	}()
	if err != nil {
		span.Set("history error", historyFailure(err).Error())
	}
}

// missingTrees returns the files of the trees blobs that h, which may be nil, doesn't hold, read from the content
// store, which ReadTree bounds.
func (s *Scheduler) missingTrees(ctx context.Context, h *cache.History, blobs map[digest.Digest]cache.Blob) (
	map[digest.Digest][]cache.HistoryFile, error) {
	trees := map[digest.Digest][]cache.HistoryFile{}
	for d, b := range blobs {
		if h != nil {
			if _, ok := h.Trees[d]; ok {
				continue
			}
		}
		tree, err := cache.ReadTree(ctx, s.content, b)
		if err != nil {
			return nil, err
		}
		trees[d] = cache.HistoryFiles(tree)
	}
	return trees, nil
}

// supplierDescs returns the descriptions of what supplies the target n's base, or "" for scratch, and each add
// step's files, in order.
func (s *Scheduler) supplierDescs(n *graph.Node) (string, []string) {
	var base string
	if b := n.Base(); b != graph.Scratch {
		base = s.graph.Node(b).Desc
	}
	var adds []string
	for _, in := range n.Steps() {
		if in.From != nil {
			adds = append(adds, s.graph.Node(*in.From).Desc)
		}
	}
	return base, adds
}

// PruneHistory removes the target histories in records unused for cache.HistoryRetention, counting them in pruned;
// a failure, which leaves the histories it couldn't remove for the next prune, is a warning in pruned, and fails
// nothing.
func PruneHistory(ctx context.Context, records cache.RecordStore, pruned *api.Pruned) {
	removed, err := records.PruneHistory(ctx, time.Now().Add(-cache.HistoryRetention))
	pruned.Histories = removed
	if err != nil {
		pruned.Warnings = append(pruned.Warnings, fmt.Sprintf("pruning target histories: %v", err))
	}
}
