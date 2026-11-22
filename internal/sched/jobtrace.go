package sched

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/graph"
	"github.com/vamos-build/vamos/internal/tracing"
)

// executorProcess names the executor's spans' process in a trace.
const executorProcess = "executor"

// traceSaveTimeout bounds storing a job's manifest or an attempt's trace, which goes ahead after the job or attempt
// is cancelled, so that a store that doesn't answer delays its end by no more.
const traceSaveTimeout = 10 * time.Second

// JobTrace is what a job records of its trace: its own spans, always, and what became of each node that went
// through the cache, which Save stores as the job's manifest, an api.JobTrace.
// Its methods may be called from several goroutines.
type JobTrace struct {
	recorder *tracing.Recorder

	mu      sync.Mutex
	entries []api.TraceEntry
	index   map[graph.Ref]int // of each node's entry in entries
}

type jobTraceKey struct{}

// ForJob returns a context whose spans the job records, always, and the JobTrace that holds them,
// which the job saves before it returns its outcome.
func ForJob(ctx context.Context) (context.Context, *JobTrace) {
	t := &JobTrace{recorder: tracing.NewRecorder(executorProcess, nil)}
	ctx = tracing.WithRecorder(ctx, t.recorder, "")
	return context.WithValue(ctx, jobTraceKey{}, t), t
}

// jobTraceOf returns the JobTrace of the job whose context ctx is, or nil outside a job, as in tests that run a
// Scheduler alone.
func jobTraceOf(ctx context.Context) *JobTrace {
	t, _ := ctx.Value(jobTraceKey{}).(*JobTrace)
	return t
}

// Record records e, what became of a node, in place of any entry for the same node,
// as a regeneration's miss replaces the hit recorded first.
func (t *JobTrace) Record(e api.TraceEntry) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if i, ok := t.index[e.Node]; ok {
		t.entries[i] = e
		return
	}
	if t.index == nil {
		t.index = map[graph.Ref]int{}
	}
	t.index[e.Node] = len(t.entries)
	t.entries = append(t.entries, e)
}

// Save stores the job's manifest, its spans with each node's entry and how ctx says its trace is exported, in store,
// under ctx without its cancellation and within traceSaveTimeout, so that a cancelled job's manifest is stored too,
// and returns its blob, nil, with why, if it couldn't be stored, which leaves the job's outcome as it is,
// and the spans the manifest holds, trimmed to fit it, even if it couldn't be stored, but none if it couldn't be made.
// The job's spans must have ended: it is called last, before the job returns its outcome.
func (t *JobTrace) Save(ctx context.Context, store api.BlobPutter) (*cache.Blob, []tracing.Span, error) {
	spans, lost := t.recorder.Snapshot()
	t.mu.Lock()
	entries := slices.Clone(t.entries)
	t.mu.Unlock()
	manifest, err := api.JobTrace{Spans: spans, Entries: entries, Exported: tracing.ExportedOf(ctx)}.Trimmed(lost)
	if err != nil { // with no spans, rather than ones the manifest might not have held
		return nil, nil, fmt.Errorf("encoding the job's trace: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), traceSaveTimeout)
	defer cancel()
	b, err := manifest.Put(ctx, store)
	if err != nil {
		return nil, manifest.Spans, err
	}
	return &b, manifest.Spans, nil
}

// SpanSink is told each snapshot of spans that sched stores, a job's or an attempt's, trimmed as it is stored, under
// the context of the job that recorded it, or that started the attempt, even if that job has since left it.
// It must not change the spans, and may read them after it returns.
// A coordinator's exports them; nil, as an executor's that exports nothing, is told nothing.
// Its methods are called on the paths of jobs and attempts, which wait for them, from several goroutines at once, and
// with contexts that may have been cancelled: a sink that exports hands the spans off rather than sending them.
type SpanSink interface {
	// Attempt is told the spans of the attempt id, which ran from start until its result was recorded, at end,
	// whether or not its trace could be stored, though none of the runner's if it couldn't be made, followed by the
	// span of its cache write, if any, which its trace can't hold; and before any job it serves is told of its end.
	Attempt(ctx context.Context, id string, spans []tracing.Span, start, end time.Time)
	// Job is told the spans of the job, which ran from start to end and ended as outcome says, succeeded, failed or
	// cancelled, as RunJob returns, whether or not its manifest could be stored, though none if it couldn't be made.
	// Those marked tracing.Shared are copies of an attempt's, such as its cache write, which Attempt was told of.
	Job(ctx context.Context, spans []tracing.Span, start, end time.Time, outcome string)
}

// The outcomes SpanSink.Job is told.
const (
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
)

// outcomeOf returns how a job whose context is ctx ended with o, as SpanSink.Job is told: cancelled if it didn't
// succeed and its context has ended, as its target spans say, whatever error it failed with, and failed otherwise.
func outcomeOf(ctx context.Context, o api.Outcome) string {
	switch {
	case o.Failure == nil && !o.Cancelled:
		return JobSucceeded
	case ctx.Err() != nil:
		return JobCancelled
	default:
		return JobFailed
	}
}

// RunJob runs work, a job's, recording its trace, always, and stores the job's manifest once work returns, before
// the job does, naming it in the outcome; a manifest that can't be stored, or a content store that can't be opened,
// leaves the outcome naming none, and otherwise as it is, and is reported to warn, if it isn't nil.
// It tells sink, if it isn't nil, of the spans the manifest holds, beside storing it.
func RunJob(ctx context.Context, content func() (cache.ContentStore, error), warn func(error), sink SpanSink,
	work func(context.Context) api.Outcome) api.Outcome {
	ctx, trace := ForJob(ctx)
	start := time.Now()
	outcome := work(ctx)
	ended := outcomeOf(ctx, outcome) // as work returned, whatever befalls the job as its manifest is stored
	var store api.BlobPutter
	if opened, err := content(); err != nil {
		store = refusing{err}
	} else {
		store = opened
	}
	var (
		spans []tracing.Span
		err   error
	)
	outcome.Trace, spans, err = trace.Save(ctx, store)
	if err != nil && warn != nil {
		warn(fmt.Errorf("storing the job's trace: %w", err))
	}
	if sink != nil {
		sink.Job(ctx, spans, start, time.Now(), ended) // as RunJob returns, its manifest stored
	}
	return outcome
}

// refusing is a content store that can't be opened, whose puts fail with why.
type refusing struct{ err error }

func (r refusing) PutBytes(context.Context, []byte) (cache.Blob, error) { return cache.Blob{}, r.err }

// Traced is an Output that a Runner tells of the attempt that served it, once the job is done with the attempt:
// as Joiner is told whose attempt runs it, before.
type Traced interface {
	Traced(t AttemptTrace)
}

// AttemptTrace is what a job learns of the trace of an attempt that served it.
type AttemptTrace struct {
	ID string // the attempt's
	// Trace is the blob of the attempt's stored trace, or nil if the job left the attempt running, or the trace
	// couldn't be stored.
	Trace *cache.Blob
	Left  bool // the job left the attempt running, and learned only its ID
	// Write is the span of the attempt's cache write, which isn't in the trace, since it names the trace's blob:
	// the job that started the attempt records it as its own, and a job that joined it beside the attempt's spans.
	Write []tracing.Span
}

// traced tells ev, if it is Traced, of the attempt that served it.
func traced(ev Output, t AttemptTrace) {
	if tr, ok := ev.(Traced); ok {
		tr.Traced(t)
	}
}

// sharedTrack returns the track that a job that joined the attempt serving the target desc puts the attempt's spans
// on, apart from the target's own, since the job that started it has the spans that hold them.
func sharedTrack(desc string) string {
	return fmt.Sprintf("%s (shared)", desc)
}

// entry returns the manifest entry of node, served by the attempt t, which the job started if starter is set.
func (t AttemptTrace) entry(node graph.Ref, desc string, starter bool) api.TraceEntry {
	e := api.TraceEntry{Node: node, Desc: desc, Outcome: api.NodeJoined, Attempt: t.ID, Left: t.Left}
	if starter {
		e.Outcome, e.Started = api.NodeMiss, true
	}
	e.Trace = cloneBlob(t.Trace)
	return e
}
