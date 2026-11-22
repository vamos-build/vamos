package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/finish"
	"github.com/vamos-build/vamos/internal/remote"
	"github.com/vamos-build/vamos/internal/run"
	"github.com/vamos-build/vamos/internal/tracing"
)

// clientProcess names the client's spans' process in a trace.
const clientProcess = "vamos"

// The bounds on a command's cleanup once its jobs end, each under the command's context without its cancellation,
// so that an interrupted command still stores and writes its trace, and no step can take the others' time;
// tests shorten them.
var (
	traceStoreTimeout    = 10 * time.Second // storing the command trace, of at most tracing.MaxTraceSize
	traceAssembleTimeout = 60 * time.Second // reading the manifests and attempt traces for --trace's file
	releaseTimeout       = 10 * time.Second // releasing the jobs, which lets a prune run
)

// clockTimeout bounds each sample of the executor's clock, not the measurement whole, so that a slow sample costs
// none that came before it; tests shorten it.
var clockTimeout = 5 * time.Second

// traceSession is a command's trace: the client's spans, which it always records, the jobs it ran, each by its
// manifest, and how far their executor's clock is ahead of the client's.
// Once the command's work returns, finishTrace stores it as an api.CommandTrace in the executor's content store,
// prints its digest, writes --trace's file, if asked, and only then releases the executors' jobs, so that no prune
// takes a manifest the command trace names before it is stored and read.
// Its methods may be called from several goroutines.
type traceSession struct {
	path     string // --trace's, absolute, or "" if it isn't given
	recorder *tracing.Recorder

	mu            sync.Mutex
	jobs          []api.CommandJob
	offset        tracing.Offset // how far the executor's clock is ahead of the client's
	offsetUnknown bool
	executors     []executor // whose jobs it holds, released and closed once the trace is done, in order
}

// newTraceSession returns a session whose trace goes to path, if it isn't "", checking that a file can be written
// there, and that it isn't the command's own output: a file, or else a folder, if outputDir, which the trace mustn't
// be in. output is "" if the command writes no file.
func newTraceSession(path, output string, outputDir bool) (*traceSession, error) {
	if path == "" {
		return &traceSession{recorder: tracing.NewRecorder(clientProcess, nil)}, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("--trace %s: %w", path, err)
	}
	if output != "" {
		if outputDir {
			// Export replaces the folder at its path without following links in it,
			// so a trace is in it if its path is, even through a link out of it,
			// as well as if it is once links are resolved.
			if within(output, abs) || within(resolve(output), resolve(abs)) {
				return nil, fmt.Errorf("--trace %s: it is within the folder that export replaces", path)
			}
		} else if sameFile(abs, output) {
			return nil, fmt.Errorf("--trace %s: it is also the file that save writes", path)
		}
	}
	if err := finish.CheckSaveDest(abs); err != nil {
		return nil, fmt.Errorf("--trace %s: %w", path, err)
	}
	return &traceSession{path: abs, recorder: tracing.NewRecorder(clientProcess, nil)}, nil
}

// within reports whether the absolute path p is dir or beneath it.
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && filepath.IsLocal(rel) // Rel gives "." for dir itself, which IsLocal accepts
}

// sameFile reports whether the absolute paths a and b name the same file, even through links,
// and even if neither exists yet.
func sameFile(a, b string) bool {
	if resolve(a) == resolve(b) {
		return true
	}
	ai, aErr := os.Stat(a)
	bi, bErr := os.Stat(b)
	return aErr == nil && bErr == nil && os.SameFile(ai, bi)
}

// resolve returns the absolute path p with the links in its folder resolved, as far as they exist.
func resolve(p string) string {
	if dir, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(dir, filepath.Base(p))
	}
	return p
}

// addJob records that the job id ended with outcome; one whose outcome names no manifest, as an older executor's, is
// warned about, and the trace says so.
func (t *traceSession) addJob(id api.JobID, outcome api.Outcome, warn func(error)) {
	t.mu.Lock()
	t.jobs = append(t.jobs, api.CommandJob{ID: id, Manifest: outcome.Trace})
	t.mu.Unlock()
	if outcome.Trace == nil {
		warn(fmt.Errorf("the executor stored no trace of the job %s, so the command's trace lacks it", id))
	}
}

// measure measures ex's clock against the client's, for a command trace that can be stored,
// with one sample, or three if the command writes the trace, whose spans are placed by it.
// It runs once the command's jobs have ended and are held, so that no prune can run while it waits.
// Each sample is bounded, so that an executor that stops answering can't hold the command, and makes none of the
// coordinator client's retries, since a sample retried after a wait is skewed, and a coordinator that has gone, as an
// interrupted one has, would otherwise hold the command for the whole bound; a measurement that fails before that
// bound, as on a reused connection that was reset, is taken once more, timed afresh, so that its failure doesn't skew
// the offset, within what is left of that bound. None is ended by an interrupt, which the jobs' end may meet.
// An offset not measured is warned about, and noted in the trace.
// The local executor runs in this process, on its clock, so its offset is none, exactly: measuring it would place the
// spans a sample's round trip apart at worst, which, under load, can put a job's span before the client's that
// started it.
func (t *traceSession) measure(ctx context.Context, ex api.Executor, warn func(error)) {
	if _, local := ex.(*run.Local); local {
		t.mu.Lock()
		t.offset = tracing.Offset{}
		t.mu.Unlock()
		return
	}
	samples := 1
	if t.path != "" {
		samples = 3
	}
	sample := func(ctx context.Context) (time.Time, error) {
		ctx, cancel := context.WithTimeout(remote.Once(ctx), clockTimeout)
		defer cancel()
		caps, err := ex.Capabilities(ctx)
		return caps.Clock, err
	}
	ctx = context.WithoutCancel(ctx)
	start := time.Now()
	offset, err := tracing.EstimateOffset(ctx, samples, time.Now, sample)
	if err != nil && time.Since(start) < clockTimeout { // a failure before the bound, as of a reset, not a stall
		// Timed afresh, within what is left of the first measurement's bound, which bounds both.
		again, cancel := context.WithDeadline(ctx, start.Add(clockTimeout))
		first := err
		if offset, err = tracing.EstimateOffset(again, samples, time.Now, sample); err != nil &&
			err.Error() != first.Error() { // both, on a line, since the first's cause may be the one to know
			err = fmt.Errorf("%w, and before, %w", err, first)
		}
		cancel()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		t.offsetUnknown = true
		warn(fmt.Errorf("measuring the executor's clock: %w; the trace's spans of this process may be misplaced", err))
		return
	}
	t.offset = offset
}

// hold keeps ex, whose jobs the command ran, to release and close once the trace is done.
func (t *traceSession) hold(ex executor) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.executors = append(t.executors, ex)
}

// document returns the command's trace as it stands, and what the client's recorder refused.
func (t *traceSession) document() (api.CommandTrace, tracing.Lost) {
	spans, lost := t.recorder.Snapshot()
	t.mu.Lock()
	defer t.mu.Unlock()
	return api.CommandTrace{Spans: spans, Offset: t.offset, OffsetUnknown: t.offsetUnknown,
		Jobs: slices.Clone(t.jobs)}, lost
}

// finishTrace ends the command's trace, which returned err, as traceSession says: it stores the command trace, if
// the command ran a job, and prints its digest on stderr, warning instead if it can't; writes --trace's file, if
// given; and releases and closes the executors it holds, each step bounded, under ctx without its cancellation.
// It returns the command's error: err, if it failed, with a failure to write the file only warned about,
// or else that failure.
func (inv *invocation) finishTrace(err error) error {
	t := inv.session
	ctx := context.WithoutCancel(inv.ctx)
	defer func() {
		for _, ex := range t.executors {
			inv.release(ex)
			if err := ex.Close(); err != nil {
				inv.warn(fmt.Errorf("shutting the executor down: %w", err))
			}
		}
	}()
	var content cache.ContentStore
	doc, lost := t.document()
	if t.ranJobs() && len(t.executors) > 0 {
		ex := t.executors[len(t.executors)-1]
		var cerr error
		if content, cerr = ex.ContentStore(); cerr != nil {
			inv.warn(fmt.Errorf("the command's trace can't be stored: %w", cerr))
			content = nil
			doc.Spans = append(doc.Spans, incomplete(clientProcess, clientProcess, time.Time{},
				"the executor's build cache can't be read, so its jobs' spans are missing"))
		} else {
			t.measure(ctx, ex, inv.warn) // only for a trace that can be stored
			doc, lost = t.document()     // with the offset
			if stored, ok := inv.storeTrace(ctx, content, doc, lost); ok {
				// --trace writes what was stored, capped as it is, so that vamos trace writes the same.
				doc, lost = stored, tracing.Lost{}
			}
		}
	}
	if t.path == "" {
		return err
	}
	spans := atTheStart(tracing.Trim(doc.Spans, lost, math.MaxInt))
	if content != nil {
		actx, cancel := context.WithTimeout(ctx, traceAssembleTimeout)
		defer cancel()
		spans = assemble(actx, content, doc, lost)
	}
	werr := finish.WriteFile(t.path, func(w io.Writer) error {
		return tracing.WriteChrome(w, spans)
	})
	if werr == nil {
		return err
	}
	werr = fmt.Errorf("writing trace %s: %w", inv.opts.trace, werr)
	if err != nil {
		inv.warn(werr)
		return err
	}
	return werr
}

// storeTrace stores doc, the command's trace, with what lost says the client's recorder refused, in content,
// within traceStoreTimeout, and prints its digest on stderr, or warns that it couldn't. It returns the trace as it
// was stored, trimmed to its cap, and whether it was.
func (inv *invocation) storeTrace(ctx context.Context, content cache.ContentStore, doc api.CommandTrace,
	lost tracing.Lost) (api.CommandTrace, bool) {
	data, err := doc.Encode(lost)
	if err == nil {
		sctx, cancel := context.WithTimeout(ctx, traceStoreTimeout)
		defer cancel()
		var b cache.Blob
		if b, err = content.PutBytes(sctx, data); err == nil {
			fmt.Fprintf(inv.stderr, "vamos: trace %s\n", b.Digest)
			stored, derr := api.DecodeCommandTrace(data)
			return stored, derr == nil
		}
	}
	inv.warn(fmt.Errorf("the command's trace couldn't be stored: %w", err))
	return api.CommandTrace{}, false
}

// ranJobs reports whether the command ran a job.
func (t *traceSession) ranJobs() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.jobs) > 0
}

// incomplete returns an instant, on the track of the process and track named, at start, saying the trace lacks a
// part, and why; a zero start places it at the trace's start, as atTheStart does.
func incomplete(process, track string, start time.Time, why string, args ...any) tracing.Span {
	s := tracing.Span{ID: "incomplete/" + track + "/" + why, Process: process, Track: track,
		Name: "trace incomplete", Start: start, End: start, Instant: true, Args: map[string]any{"reason": why}}
	if start.IsZero() {
		s.Args["unplaced"] = true
	}
	for i := 0; i+1 < len(args); i += 2 {
		s.Args[fmt.Sprint(args[i])] = args[i+1]
	}
	return s
}

// readAll reads blobs from content, each within tracing.MaxTraceSize, with one ReadMany, falling back to one read
// each if it fails with a blob missing or corrupt, so that one such costs only its own; it returns each blob's
// content, or nil with why it couldn't be read.
func readAll(ctx context.Context, content cache.ContentStore, blobs []cache.Blob) ([][]byte, []error) {
	data := make([][]byte, len(blobs))
	errs := make([]error, len(blobs))
	if len(blobs) == 0 {
		return data, errs
	}
	all, err := content.ReadMany(ctx, blobs, tracing.MaxTraceSize)
	if err == nil {
		return all, errs
	}
	if !errors.Is(err, cache.ErrMissing) && !errors.Is(err, cache.ErrCorrupt) {
		for i := range errs {
			errs[i] = err
		}
		return data, errs
	}
	for i, b := range blobs {
		data[i], errs[i] = content.Read(ctx, b, tracing.MaxTraceSize)
	}
	return data, errs
}

// assemble returns the spans of doc, a command's trace, as a trace file shows them: the client's spans, with an
// instant for those its recorder refused, as lost says, placed on the executor's clock by the offset doc measured;
// each job's own spans, from its manifest; and the spans of each attempt a job took the trace of, once, by the
// attempt's ID: as recorded, under its target, if the job that started it took the trace, and otherwise on the
// target's shared track, part of no span, in the first job in doc's order that took it, whose cache write of it alone
// is kept. A part of the trace that can't be read, or that no job took, is an instant saying so, where it would have
// been. Hits' attempt traces aren't placed: they ran at another time, and the hit's span names its trace.
func assemble(ctx context.Context, content cache.ContentStore, doc api.CommandTrace,
	lost tracing.Lost) []tracing.Span {
	client := tracing.Shift(tracing.Trim(doc.Spans, lost, math.MaxInt),
		tracing.Offset{Offset: -doc.Offset.Offset, Uncertainty: doc.Offset.Uncertainty})
	spans := client
	if doc.OffsetUnknown {
		spans = append(spans, incomplete(clientProcess, clientProcess, time.Time{},
			"the executor's clock couldn't be measured, so this process's spans may be misplaced"))
	}

	// The manifests, in order.
	var blobs []cache.Blob
	var jobs []api.CommandJob
	for _, j := range doc.Jobs {
		if j.Manifest == nil {
			spans = append(spans, incomplete(clientProcess, clientProcess, time.Time{},
				"the executor stored no trace of a job", "job", string(j.ID)))
			continue
		}
		blobs, jobs = append(blobs, *j.Manifest), append(jobs, j)
	}
	data, errs := readAll(ctx, content, blobs)
	manifests := make([]*api.JobTrace, len(blobs))
	for i := range blobs {
		if errs[i] == nil {
			var m api.JobTrace
			if m, errs[i] = api.DecodeJobTrace(data[i]); errs[i] == nil {
				manifests[i] = &m
			}
		}
		if errs[i] != nil {
			spans = append(spans, incomplete(clientProcess, clientProcess, time.Time{}, "a job's trace can't be read",
				"job", string(jobs[i].ID), "error", errs[i].Error()))
		}
	}

	// Which job places each attempt: the starter that took its trace, or else the first that took it.
	type placement struct {
		job     int
		starter bool
		trace   cache.Blob
		entry   api.TraceEntry
	}
	placed := map[string]*placement{}
	var order []string
	for i, m := range manifests {
		if m == nil {
			continue
		}
		if m.Dropped > 0 {
			spans = append(spans, incomplete(clientProcess, clientProcess, time.Time{},
				"a job's trace was cut short, and lacks targets", "job", string(jobs[i].ID), "targets", m.Dropped))
		}
		for _, e := range m.Entries {
			if e.Outcome == api.NodeHit || e.Attempt == "" {
				continue
			}
			p, ok := placed[e.Attempt]
			switch {
			case e.Trace == nil:
				if !ok {
					placed[e.Attempt] = &placement{job: -1, entry: e}
					order = append(order, e.Attempt)
				}
			case !ok || p.job < 0 || e.Started && !p.starter:
				if !ok {
					order = append(order, e.Attempt)
				}
				placed[e.Attempt] = &placement{job: i, starter: e.Started, trace: *e.Trace, entry: e}
			}
		}
	}

	// Each job's own spans, but the cache writes of attempts another job places.
	for i, m := range manifests {
		if m == nil {
			continue
		}
		for _, s := range m.Spans {
			if id, ok := s.Args["attempt"].(string); ok && s.Name == "cache write" {
				if p, ok := placed[id]; ok && p.job >= 0 && p.job != i {
					continue
				}
			}
			spans = append(spans, s)
		}
	}

	// The attempts' traces.
	var traces []cache.Blob
	var ids []string
	for _, id := range order {
		p := placed[id]
		if p.job < 0 {
			why := "the attempt's trace couldn't be stored"
			if p.entry.Left {
				why = "the job left the attempt running"
			}
			spans = append(spans, incomplete(executorProcess, p.entry.Desc, time.Time{}, why, "attempt", id))
			continue
		}
		traces, ids = append(traces, p.trace), append(ids, id)
	}
	data, errs = readAll(ctx, content, traces)
	for i, id := range ids {
		p := placed[id]
		var a tracing.Attempt
		if errs[i] == nil {
			a, errs[i] = tracing.DecodeAttempt(data[i])
		}
		if errs[i] != nil {
			spans = append(spans, incomplete(executorProcess, p.entry.Desc, time.Time{},
				"the attempt's trace is gone from the build cache", "attempt", id, "trace", p.trace.Digest.String()))
			continue
		}
		for _, s := range a.Spans {
			if !p.starter {
				// All on the target's shared track, whatever the tracks of the process that ran them, as a worker's.
				track := p.entry.Desc
				if track == "" {
					track = s.Track
				}
				s.Track, s.Parent = track+" (shared)", ""
			}
			spans = append(spans, s)
		}
	}
	return withoutDanglingArrows(atTheStart(spans))
}

// atTheStart returns spans with each "trace incomplete" instant that had no time of its own, as one placed before
// any span was read, at the earliest start among the others, so that a trace of a job long ended doesn't stretch to
// now.
func atTheStart(spans []tracing.Span) []tracing.Span {
	var earliest time.Time
	for _, s := range spans {
		if s.Name != "trace incomplete" && (earliest.IsZero() || s.Start.Before(earliest)) {
			earliest = s.Start
		}
	}
	if earliest.IsZero() {
		earliest = time.Unix(0, 0).UTC() // nothing else has a time: the trace is its markers alone, written alike
	}
	for i, s := range spans {
		if s.Name == "trace incomplete" && s.Args["unplaced"] == true {
			delete(s.Args, "unplaced")
			s.Start, s.End = earliest, earliest
			spans[i] = s
		}
	}
	return spans
}

// withoutDanglingArrows returns spans without the ends of arrows whose other end isn't among them, such as an
// attempt's use of a base that a pull of another job, or of an earlier build, made, which Perfetto can't draw.
func withoutDanglingArrows(spans []tracing.Span) []tracing.Span {
	produced, used := map[string]bool{}, map[string]bool{}
	for _, s := range spans {
		if s.Produces != "" && s.Uses == "" { // WriteChrome draws a span's Uses alone, if it has both
			produced[s.Produces] = true
		}
		if s.Uses != "" {
			used[s.Uses] = true
		}
	}
	out := make([]tracing.Span, len(spans))
	for i, s := range spans {
		if s.Uses != "" && !produced[s.Uses] {
			s.Uses = ""
		}
		if s.Produces != "" && !used[s.Produces] {
			s.Produces = ""
		}
		out[i] = s
	}
	return out
}

// executorProcess names the executor's spans' process in a trace, as sched names it.
const executorProcess = "executor"
