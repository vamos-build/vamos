package api

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/graph"
	"github.com/vamos-build/vamos/internal/tracing"

	"github.com/opencontainers/go-digest"
)

// span returns a span named name that starts ms milliseconds after a fixed time, with padding bytes of arguments.
func span(name string, ms, padding int) tracing.Span {
	start := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC).Add(time.Duration(ms) * time.Millisecond)
	return tracing.Span{ID: name, Process: "executor", Track: "job", Name: name, Start: start, End: start,
		Args: map[string]any{"padding": strings.Repeat("x", padding)}}
}

// TestTraceDocumentsRoundTrip checks that a manifest and a command's trace decode as they were encoded,
// and that each refuses the other's kind.
func TestTraceDocumentsRoundTrip(t *testing.T) {
	blob := &cache.Blob{Digest: digest.FromString("attempt"), Size: 3}
	job := JobTrace{
		Spans: []tracing.Span{span("schedule", 0, 1)},
		Entries: []TraceEntry{
			{Node: 1, Outcome: NodeMiss, Started: true, Cause: "own-inputs", Attempt: "a1", Trace: blob},
			{Node: 2, Outcome: NodeJoined, Attempt: "a2", Left: true},
			{Node: 3, Outcome: NodeHit, Attempt: "a3", Trace: blob},
		},
	}
	data, err := job.Encode(tracing.Lost{})
	if err != nil {
		t.Fatal(err)
	}
	gotJob, err := DecodeJobTrace(data)
	if err != nil {
		t.Fatal(err)
	}
	job.Header = tracing.Header{Version: tracing.Version, Kind: KindJob}
	if !reflect.DeepEqual(gotJob.Entries, job.Entries) || gotJob.Header != job.Header || len(gotJob.Spans) != 1 {
		t.Errorf("decoded manifest %+v; want %+v", gotJob, job)
	}

	command := CommandTrace{
		Spans:  []tracing.Span{span("load", 0, 1)},
		Offset: tracing.Offset{Offset: time.Second, Uncertainty: time.Millisecond},
		Jobs:   []CommandJob{{ID: "j1", Manifest: blob}, {ID: "j2"}},
	}
	data, err = command.Encode(tracing.Lost{})
	if err != nil {
		t.Fatal(err)
	}
	gotCommand, err := DecodeCommandTrace(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotCommand.Jobs, command.Jobs) || gotCommand.Offset != command.Offset ||
		gotCommand.Kind != KindCommand {
		t.Errorf("decoded command trace %+v; want %+v", gotCommand, command)
	}
	if _, err := DecodeJobTrace(data); !errors.Is(err, tracing.ErrNotTrace) {
		t.Errorf("DecodeJobTrace of a command's trace = %v; want ErrNotTrace", err)
	}
	if _, err := DecodeCommandTrace([]byte("a log")); !errors.Is(err, tracing.ErrNotTrace) {
		t.Errorf("DecodeCommandTrace of a log = %v; want ErrNotTrace", err)
	}
}

// TestJobTraceCap checks that a manifest keeps its entries before its spans: spans that fit beside nothing but not
// beside the entries are trimmed, and entries that alone pass the cap are dropped from the end and counted.
func TestJobTraceCap(t *testing.T) {
	entries := func(n int) []TraceEntry {
		out := make([]TraceEntry, n)
		for i := range out {
			out[i] = TraceEntry{Node: graph.Ref(i), Outcome: NodeMiss, Cause: "own-inputs", Attempt: "a"}
		}
		return out
	}
	// Entries of about 10 MiB, and spans of about 10 MiB, each within the cap of 16 MiB alone, but not together.
	const entrySize = len(`{"node":100000,"outcome":"miss","cause":"own-inputs","attempt":"a"},`)
	var spans []tracing.Span
	for i := range 10 {
		spans = append(spans, span("step", i, 1<<20))
	}
	halves := JobTrace{Spans: spans, Entries: entries(10 << 20 / entrySize)}
	data, err := halves.Encode(tracing.Lost{})
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > tracing.MaxTraceSize {
		t.Fatalf("the manifest takes %d bytes; want at most %d", len(data), tracing.MaxTraceSize)
	}
	got, err := DecodeJobTrace(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != len(halves.Entries) || got.Dropped != 0 {
		t.Errorf("kept %d entries, dropped %d; want all %d", len(got.Entries), got.Dropped, len(halves.Entries))
	}
	if n := len(got.Spans); n == 0 || n > len(spans) || got.Spans[n-1].Name != tracing.Truncated {
		t.Errorf("kept %d spans; want the first few and a %q instant", n, tracing.Truncated)
	}

	many := JobTrace{Spans: spans[:1], Entries: entries(20 << 20 / entrySize)}
	data, err = many.Encode(tracing.Lost{})
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > tracing.MaxTraceSize {
		t.Fatalf("the manifest takes %d bytes; want at most %d", len(data), tracing.MaxTraceSize)
	}
	got, err = DecodeJobTrace(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Dropped == 0 || len(got.Entries)+got.Dropped != len(many.Entries) {
		t.Errorf("kept %d entries and counted %d dropped; want some dropped, the two adding up to %d",
			len(got.Entries), got.Dropped, len(many.Entries))
	}
	if got.Entries[0].Node != 0 {
		t.Errorf("the first entry kept is node %d; want node 0, the last dropped first", got.Entries[0].Node)
	}
}

// TestOutcomeTraceClone checks that a clone of an outcome shares neither its trace nor a prune's warnings.
func TestOutcomeTraceClone(t *testing.T) {
	o := Outcome{
		Trace:  &cache.Blob{Digest: digest.FromString("manifest"), Size: 1},
		Pruned: &Pruned{Histories: 2, Warnings: []string{"removing a history: busy"}},
	}
	c := o.Clone()
	c.Trace.Size = 9
	c.Pruned.Warnings[0] = "changed"
	if o.Trace.Size != 1 || o.Pruned.Warnings[0] != "removing a history: busy" {
		t.Errorf("changing a clone changed the outcome: %+v, %+v", o.Trace, o.Pruned)
	}
	data, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	var back Outcome
	if err := json.Unmarshal(data, &back); err != nil || !reflect.DeepEqual(back, o) {
		t.Errorf("the outcome decodes as %+v, %v; want %+v", back, err, o)
	}
}

// TestCommandTraceCap checks that a command's trace keeps its jobs and trims its spans to fit beside them,
// and fails, rather than store a trace that lost jobs, if its jobs alone pass the cap.
func TestCommandTraceCap(t *testing.T) {
	var spans []tracing.Span
	for i := range 20 {
		spans = append(spans, span("load", i, 1<<20))
	}
	jobs := []CommandJob{{ID: "j1", Manifest: &cache.Blob{Digest: digest.FromString("m"), Size: 1}}}
	data, err := CommandTrace{Spans: spans, Jobs: jobs}.Encode(tracing.Lost{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeCommandTrace(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > tracing.MaxTraceSize || len(got.Jobs) != 1 || got.Spans[len(got.Spans)-1].Name != tracing.Truncated {
		t.Errorf("a command trace of %d bytes kept %d jobs and %d spans; want its job and spans cut short, within %d",
			len(data), len(got.Jobs), len(got.Spans), tracing.MaxTraceSize)
	}

	many := make([]CommandJob, tracing.MaxTraceSize/len(`{"id":"j"},`)+1)
	for i := range many {
		many[i] = CommandJob{ID: "j"}
	}
	if _, err := (CommandTrace{Jobs: many}).Encode(tracing.Lost{}); err == nil {
		t.Error("a command trace whose jobs alone pass the cap encoded")
	}
}
