package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/graph"
	"github.com/vamos-build/vamos/internal/tracing"
)

// The kinds of the stored trace documents that name other blobs, beside tracing.KindAttempt.
const (
	KindJob     = "job"     // a JobTrace, a job's manifest
	KindCommand = "command" // a CommandTrace, a command's trace
)

// What became of a node of a job, as its manifest entry says.
const (
	NodeHit    = "hit"    // its cached result was reused
	NodeMiss   = "miss"   // it ran, in an attempt the job started
	NodeJoined = "joined" // it waited for an attempt another job, or another node, started
)

// JobTrace is a job's manifest: its own spans, such as scheduling, lookups, checks, transfers and pushes,
// and an entry for each node that went through the cache, which names the attempt trace it took.
// It is stored as one content blob within tracing.MaxTraceSize, which Outcome.Trace names.
type JobTrace struct {
	tracing.Header
	Spans   []tracing.Span `json:"spans"`
	Entries []TraceEntry   `json:"entries"`
	// Dropped counts the entries left out, from the end, since even they alone passed the cap.
	Dropped int `json:"dropped,omitempty"`
}

// TraceEntry says what became of one node of a job, and where the trace of the work that served it is.
type TraceEntry struct {
	Node    graph.Ref `json:"node"`
	Outcome string    `json:"outcome"`           // NodeHit, NodeMiss or NodeJoined
	Started bool      `json:"started,omitempty"` // it started the attempt that served it
	// Cause is why a layer or task missed, as attribution found it; a hit, and a pull, have none.
	Cause   string `json:"cause,omitempty"`
	Attempt string `json:"attempt,omitempty"` // the ID of the attempt that served it, or made the result it reused
	// Trace is the stored trace of that attempt: the one it took, or the one that made the result it reused.
	// It is nil if the job left the attempt running, or the result was made before vamos kept traces.
	Trace *cache.Blob `json:"trace,omitempty"`
	// Left says the job left the attempt running, so that it took no trace of it.
	Left bool `json:"left,omitempty"`
}

// CommandTrace is a command's trace: the client's spans, how far the executor's clock was ahead of the client's,
// and each job of the command, in order, by its manifest.
type CommandTrace struct {
	tracing.Header
	Spans  []tracing.Span `json:"spans"`
	Offset tracing.Offset `json:"offset"`
	// OffsetUnknown says the offset couldn't be measured, and is zero.
	OffsetUnknown bool         `json:"offset_unknown,omitempty"`
	Jobs          []CommandJob `json:"jobs"`
}

// CommandJob is one job of a command, as its trace names it.
type CommandJob struct {
	ID JobID `json:"id"`
	// Manifest is the job's JobTrace, or nil if its outcome named none, as when it couldn't be stored.
	Manifest *cache.Blob `json:"manifest,omitempty"`
}

// Encode returns the manifest encoded within tracing.MaxTraceSize, as a JobTrace of the current version.
// Its entries come first: if even they pass the cap, the last are dropped and counted in Dropped;
// its spans, with what lost says the recorder refused, are then trimmed by tracing.Trim to fit beside them.
func (t JobTrace) Encode(lost tracing.Lost) ([]byte, error) {
	t.Header = tracing.Header{Version: tracing.Version, Kind: KindJob}
	spans := t.Spans
	t.Spans = []tracing.Span{}
	if t.Entries == nil {
		t.Entries = []TraceEntry{} // so that it encodes as a list; dropping below reslices this copy of t alone
	}
	empty, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	if len(empty) > tracing.MaxTraceSize {
		// Drop the last entries, by their sizes, each with its comma, until the rest fit; Dropped's digits are allowed
		// for by measuring again.
		size := len(empty)
		for len(t.Entries) > 0 && size > tracing.MaxTraceSize {
			last := t.Entries[len(t.Entries)-1]
			t.Entries = t.Entries[:len(t.Entries)-1]
			t.Dropped++
			data, _ := json.Marshal(last) // of numbers, strings and booleans alone, which always encode
			size -= len(data) + len(",")
			if size <= tracing.MaxTraceSize {
				if empty, err = json.Marshal(t); err != nil {
					return nil, err
				}
				size = len(empty)
			}
		}
	}
	if t.Spans = tracing.Trim(spans, lost, tracing.MaxTraceSize-len(empty)+len("[]")); t.Spans == nil {
		t.Spans = []tracing.Span{}
	}
	return json.Marshal(t)
}

// Save stores the manifest, encoded by Encode, in store, and returns its blob.
func (t JobTrace) Save(ctx context.Context, store cache.ContentStore, lost tracing.Lost) (cache.Blob, error) {
	data, err := t.Encode(lost)
	if err != nil {
		return cache.Blob{}, fmt.Errorf("encoding the job's trace: %w", err)
	}
	b, err := store.PutBytes(ctx, data)
	if err != nil {
		return cache.Blob{}, fmt.Errorf("storing the job's trace: %w", err)
	}
	return b, nil
}

// DecodeJobTrace decodes a manifest, failing with an error wrapping tracing.ErrNotTrace
// if data is no stored trace, or another kind of one.
func DecodeJobTrace(data []byte) (JobTrace, error) {
	var t JobTrace
	if err := decodeTrace(data, KindJob, &t); err != nil {
		return JobTrace{}, err
	}
	return t, nil
}

// Encode returns the command's trace encoded within tracing.MaxTraceSize, as a CommandTrace of the current version,
// its spans, with what lost says the recorder refused, trimmed by tracing.Trim to fit beside its jobs.
func (t CommandTrace) Encode(lost tracing.Lost) ([]byte, error) {
	t.Header = tracing.Header{Version: tracing.Version, Kind: KindCommand}
	spans := t.Spans
	t.Spans = []tracing.Span{}
	if t.Jobs == nil {
		t.Jobs = []CommandJob{}
	}
	empty, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	if len(empty) > tracing.MaxTraceSize {
		return nil, fmt.Errorf("a command of %d jobs leaves no room for its trace", len(t.Jobs))
	}
	if t.Spans = tracing.Trim(spans, lost, tracing.MaxTraceSize-len(empty)+len("[]")); t.Spans == nil {
		t.Spans = []tracing.Span{}
	}
	return json.Marshal(t)
}

// DecodeCommandTrace decodes a command's trace, failing with an error wrapping tracing.ErrNotTrace
// if data is no stored trace, or another kind of one.
func DecodeCommandTrace(data []byte) (CommandTrace, error) {
	var t CommandTrace
	if err := decodeTrace(data, KindCommand, &t); err != nil {
		return CommandTrace{}, err
	}
	return t, nil
}

// decodeTrace decodes data into v, a stored trace document of kind, reading its header first,
// so that a document of another version or kind fails as one, whatever else it holds.
func decodeTrace(data []byte, kind string, v any) error {
	h, err := tracing.ReadHeader(data)
	if err != nil {
		return err
	}
	if h.Kind != kind {
		return fmt.Errorf("%w: it is a %s trace, not a %s trace", tracing.ErrNotTrace, h.Kind, kind)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%w: %w", tracing.ErrNotTrace, err)
	}
	return nil
}
