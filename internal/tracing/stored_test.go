package tracing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// at returns a span on track that starts ms milliseconds after a fixed time and lasts dur milliseconds.
func at(id, track string, ms, dur int) Span {
	start := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC).Add(time.Duration(ms) * time.Millisecond)
	return Span{
		ID:      id,
		Process: "executor",
		Track:   track,
		Name:    id,
		Start:   start,
		End:     start.Add(time.Duration(dur) * time.Millisecond),
		Args:    map[string]any{"padding": strings.Repeat("x", 100)},
	}
}

// encoded returns the size of spans encoded as a JSON array.
func encoded(t *testing.T, spans []Span) int {
	t.Helper()
	data, err := json.Marshal(spans)
	if err != nil {
		t.Fatal(err)
	}
	return len(data)
}

// ids returns the IDs of spans, in order, naming an instant that says a trace was cut short by its name.
func ids(spans []Span) []string {
	var out []string
	for _, s := range spans {
		if s.Name == Truncated {
			out = append(out, s.Name)
			continue
		}
		out = append(out, s.ID)
	}
	return out
}

// TestAttemptRoundTrip checks that an attempt's stored trace decodes as it was encoded,
// and that its header says what it is.
func TestAttemptRoundTrip(t *testing.T) {
	step := at("step", "layer", 1, 2)
	step.Parent, step.Uses = "layer", "blob"
	instant := at("skipped", "other", 2, 0)
	instant.Instant, instant.Async, instant.Produces = true, true, "tree"
	spans := []Span{step, at("layer", "layer", 0, 5), instant}
	data, err := EncodeAttempt("a1", spans, Lost{})
	if err != nil {
		t.Fatal(err)
	}
	h, err := ReadHeader(data)
	if err != nil || h != (Header{Version: Version, Kind: KindAttempt}) {
		t.Fatalf("ReadHeader = %+v, %v; want version %d, kind %q", h, err, Version, KindAttempt)
	}
	got, err := DecodeAttempt(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "a1" || len(got.Spans) != len(spans) {
		t.Fatalf("decoded %q with %v; want a1 with %v", got.ID, ids(got.Spans), ids(spans))
	}
	for i, want := range spans {
		g := got.Spans[i]
		if !g.Start.Equal(want.Start) || !g.End.Equal(want.End) {
			t.Errorf("%s lies at %v–%v; want %v–%v", want.ID, g.Start, g.End, want.Start, want.End)
		}
		g.Start, g.End, want.Start, want.End = time.Time{}, time.Time{}, time.Time{}, time.Time{}
		if !reflect.DeepEqual(g, want) {
			t.Errorf("decoded %+v; want %+v", g, want)
		}
	}
}

// TestEncodeAttemptEdges checks that an attempt without spans lists none, and that an ID too long to store fails.
func TestEncodeAttemptEdges(t *testing.T) {
	data, err := EncodeAttempt("a1", nil, Lost{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"spans":[]`) {
		t.Errorf("an attempt without spans encodes as %s; want an empty list of spans", data)
	}
	if _, err := EncodeAttempt(strings.Repeat("x", maxAttemptID+1), nil, Lost{}); err == nil {
		t.Errorf("an ID of %d bytes encoded; want an error", maxAttemptID+1)
	}
}

// TestReadHeaderRefuses checks that what isn't a stored trace of this version, such as a log, is refused as no trace.
func TestReadHeaderRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "log", data: "step output\n"},
		{name: "JSON without a kind", data: `{"version":1}`},
		{name: "another version", data: `{"version":2,"kind":"attempt"}`},
		{name: "no version", data: `{"kind":"attempt"}`},
		{name: "malformed JSON", data: `{"version":1,"kind":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ReadHeader([]byte(tc.data)); !errors.Is(err, ErrNotTrace) {
				t.Errorf("ReadHeader = %v; want ErrNotTrace", err)
			}
		})
	}
	// The header is read first, so a document of another version fails as one, whatever else it holds.
	_, err := DecodeAttempt([]byte(`{"version":2,"kind":"attempt","spans":42}`))
	if !errors.Is(err, ErrNotTrace) || !strings.Contains(err.Error(), "version 2") {
		t.Errorf("DecodeAttempt of version 2 = %v; want ErrNotTrace naming the version", err)
	}
	if _, err := DecodeAttempt([]byte(`{"version":1,"kind":"job"}`)); !errors.Is(err, ErrNotTrace) {
		t.Errorf("DecodeAttempt of a job's trace = %v; want ErrNotTrace", err)
	}
}

// TestRecorderLimit checks that a recorder keeps spans up to its limit, refuses those past it,
// and says how many it refused and which started first.
func TestRecorderLimit(t *testing.T) {
	r := NewRecorder("executor", nil)
	var spans []Span
	for i, ms := range []int{100, 101, 102, 109, 105, 107} { // the last three are refused, and s5 started first
		spans = append(spans, at(fmt.Sprintf("s%d", i+1), "t", ms, 1))
	}
	// Room for s1–s3 and a small span, but not for any of s4–s6 beside them.
	small := Span{ID: "small", Start: spans[0].Start, End: spans[0].End}
	r.limit, _ = spanSize(small)
	for _, s := range spans[:3] {
		size, _ := spanSize(s)
		r.limit += size
	}
	for _, s := range spans {
		r.add(s)
	}
	// A span that ends later and fits is still kept, as a target's span is after its large children are refused.
	r.add(small)
	kept, lost := r.Snapshot()
	if got := ids(kept); !slices.Equal(got, []string{"s1", "s2", "s3", "small"}) {
		t.Errorf("kept %v; want [s1 s2 s3 small]", got)
	}
	if lost.Count != 3 || lost.First.ID != "s5" || lost.First.Args != nil {
		t.Errorf("lost %d, first %q with %v; want 3, first s5 without its arguments",
			lost.Count, lost.First.ID, lost.First.Args)
	}

	r = NewRecorder("executor", nil)
	r.add(Span{ID: "bad", Args: map[string]any{"f": func() {}}})
	if kept, lost := r.Snapshot(); lost.Count != 1 || len(kept) != 0 {
		t.Errorf("a span that can't be encoded: kept %d, lost %d; want 0 and 1", len(kept), lost.Count)
	}
}

// TestTrim checks which spans a trim keeps, and where and what its instant says.
func TestTrim(t *testing.T) {
	// A target on its track, with a first step, then a second step that holds a child, and another target's span.
	target := at("target", "layer", 0, 100)
	first := at("first", "layer", 10, 10)
	second := at("second", "layer", 30, 50)
	child := at("child", "layer", 40, 10)
	other := at("other", "task", 20, 70)
	spans := []Span{first, child, second, other, target} // in the order they ended
	size := func(ss ...Span) int {
		return encoded(t, ss)
	}
	refused := at("refused", "refused track", 5, 1)

	for _, tc := range []struct {
		name      string
		lost      Lost
		limit     int
		want      []string
		track     string // of the instant, if any
		dropped   int
		noInstant bool
	}{
		{
			name:      "a trace that fits is kept whole",
			limit:     size(spans...),
			want:      []string{"first", "child", "second", "other", "target"},
			noInstant: true,
		},
		{
			name:    "children go before the spans that hold them",
			limit:   size(first, other, target) + 300,
			want:    []string{"first", "other", "target", Truncated},
			track:   "layer",
			dropped: 2,
		},
		{
			name:    "the instant goes on the track of the earliest-started span lost",
			limit:   size(first, target) + 300,
			want:    []string{"first", "target", Truncated},
			track:   "task",
			dropped: 3,
		},
		{
			name:    "spans the recorder refused are counted, and may be the earliest",
			lost:    Lost{Count: 4, First: refused},
			limit:   size(first, target) + 300,
			want:    []string{"first", "target", Truncated},
			track:   "refused track",
			dropped: 7,
		},
		{
			name:    "spans refused alone still leave an instant",
			lost:    Lost{Count: 1, First: refused},
			limit:   MaxTraceSize,
			want:    []string{"first", "child", "second", "other", "target", Truncated},
			track:   "refused track",
			dropped: 1,
		},
		{
			name:      "a limit that can't hold the instant keeps nothing",
			limit:     10,
			want:      nil,
			noInstant: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Trim(spans, tc.lost, tc.limit)
			if !slices.Equal(ids(got), tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
				t.Errorf("kept %v; want %v", ids(got), tc.want)
			}
			if n := encoded(t, got); n > tc.limit && len(got) > 0 {
				t.Errorf("the trimmed trace takes %d bytes; want at most %d", n, tc.limit)
			}
			if tc.noInstant {
				return
			}
			instant := got[len(got)-1]
			if instant.Parent != "" {
				t.Errorf("the instant is part of %q; want no span", instant.Parent)
			}
			if !instant.Instant || instant.Track != tc.track || instant.Args["dropped"] != tc.dropped {
				t.Errorf("instant on %q, dropped %v; want an instant on %q, dropped %d",
					instant.Track, instant.Args["dropped"], tc.track, tc.dropped)
			}
		})
	}
}

// TestTrimTies checks that of spans that start and end together, the one within the other is dropped first,
// whatever order they ended in.
func TestTrimTies(t *testing.T) {
	parent := at("parent", "layer", 0, 0)
	child := at("child", "layer", 0, 0)
	child.Parent = parent.ID
	grandchild := at("grandchild", "layer", 0, 0)
	grandchild.Parent = child.ID
	spans := []Span{grandchild, child, parent} // in the order they ended
	got := Trim(spans, Lost{}, encoded(t, []Span{parent})+300)
	if !slices.Equal(ids(got), []string{"parent", Truncated}) {
		t.Errorf("kept %v; want [parent %s]", ids(got), Truncated)
	}
}

// TestDepths checks that a span's depth counts its parents among the spans,
// and that a long chain of them is quick and a cycle ends.
func TestDepths(t *testing.T) {
	spans := []Span{{ID: "c", Parent: "b"}, {ID: "a", Parent: "elsewhere"}, {ID: "b", Parent: "a"}, {ID: "d"}}
	if got := depths(spans); !slices.Equal(got, []int{2, 0, 1, 0}) {
		t.Errorf("depths = %v; want [2 0 1 0]", got)
	}
	branches := []Span{{ID: "a"}, {ID: "b", Parent: "a"}, {ID: "c", Parent: "a"}, {ID: "d", Parent: "b"}}
	if got := depths(branches); !slices.Equal(got, []int{0, 1, 1, 2}) {
		t.Errorf("depths of branches = %v; want [0 1 1 2]", got)
	}
	cycle := []Span{{ID: "x", Parent: "y"}, {ID: "y", Parent: "x"}}
	if got := depths(cycle); len(got) != 2 || slices.Min(got) < 0 {
		t.Errorf("depths of a cycle = %v; want two depths, none negative", got)
	}

	chain := make([]Span, 100_000)
	for i := range chain {
		chain[i] = Span{ID: fmt.Sprint(i)}
		if i > 0 {
			chain[i].Parent = fmt.Sprint(i - 1)
		}
	}
	slices.Reverse(chain) // so that each span's walk reaches spans whose depth isn't known yet
	start := time.Now()
	got := depths(chain)
	if got[0] != len(chain)-1 || got[len(got)-1] != 0 {
		t.Errorf("a chain's depths run %d to %d; want %d to 0", got[0], got[len(got)-1], len(chain)-1)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("depths of a chain of %d took %v", len(chain), took)
	}
}

// TestTrimKeepsAnInstantNamedLikeTheMarker checks that an instant a build recorded under the marker's name stays.
func TestTrimKeepsAnInstantNamedLikeTheMarker(t *testing.T) {
	instant := at("own", "layer", 0, 0)
	instant.Name, instant.Instant = Truncated, true
	if got := Trim([]Span{instant}, Lost{}, MaxTraceSize); len(got) != 1 || got[0].ID != "own" {
		t.Errorf("kept %v; want the instant alone", got)
	}
}

// TestTrimAgain checks that trimming a trimmed trace, as a stored one read back, leaves one instant counting all.
func TestTrimAgain(t *testing.T) {
	spans := []Span{at("first", "layer", 0, 10), at("second", "task", 20, 10), at("third", "task", 30, 10)}
	once := Trim(spans, Lost{Count: 2, First: at("refused", "refused", 5, 1)}, MaxTraceSize)
	data, err := json.Marshal(once)
	if err != nil {
		t.Fatal(err)
	}
	var stored []Span
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	twice := Trim(stored, Lost{}, encoded(t, spans[:1])+300)
	if !slices.Equal(ids(twice), []string{"first", Truncated}) {
		t.Fatalf("kept %v; want [first %s]", ids(twice), Truncated)
	}
	if instant := twice[1]; instant.Track != "refused" || instant.Args["dropped"] != 4 {
		t.Errorf("the instant lies on %q, dropped %v; want refused, 4", instant.Track, instant.Args["dropped"])
	}
}

// TestTrimAlwaysFits checks that whatever the spans and the limit, a trimmed trace fits, and keeps a prefix by start.
func TestTrimAlwaysFits(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for range 200 {
		var spans []Span
		for i := range rng.IntN(40) {
			track := strings.Repeat("t", 1+rng.IntN(200)) // so that instants differ in size by their track
			spans = append(spans, at(fmt.Sprintf("s%d", i), track, rng.IntN(1000), rng.IntN(100)))
		}
		lost := Lost{}
		if rng.IntN(2) == 0 {
			first := at("r", strings.Repeat("r", rng.IntN(300)), rng.IntN(1000), 1)
			lost = Lost{Count: 1 + rng.IntN(1_000_000), First: first}
		}
		limit := rng.IntN(encoded(t, spans) + 500)
		got := Trim(spans, lost, limit)
		if n := encoded(t, got); len(got) > 0 && n > limit {
			t.Fatalf("%d spans trimmed to %d bytes take %d", len(spans), limit, n)
		}
		// No span nor instant here takes 1,000 bytes, so a limit of 2,000 keeps the first span and any instant.
		if limit >= 2000 && len(spans) > 0 {
			kept := 0
			for _, s := range got {
				if !s.Instant {
					kept++
				}
			}
			if kept == 0 {
				t.Fatalf("%d spans trimmed to %d bytes kept none", len(spans), limit)
			}
			if (kept < len(spans) || lost.Count > 0) && got[len(got)-1].Name != Truncated {
				t.Fatalf("%d spans trimmed to %d bytes kept %v without an instant", len(spans), limit, ids(got))
			}
		}
		var latestKept time.Time
		for _, s := range got {
			if !s.Instant && s.Start.After(latestKept) {
				latestKept = s.Start
			}
		}
		for _, s := range spans {
			if !slices.ContainsFunc(got, func(k Span) bool { return k.ID == s.ID }) && s.Start.Before(latestKept) {
				t.Fatalf("dropped %s, started at %v, but kept a span started at %v", s.ID, s.Start, latestKept)
			}
		}
	}
}

// TestEncodeAttemptFits checks that an attempt's stored trace fits in MaxTraceSize however many spans it recorded,
// and says it was cut short.
func TestEncodeAttemptFits(t *testing.T) {
	big := strings.Repeat("x", 64<<10)
	var spans []Span
	for i := range 2 * MaxTraceSize / len(big) {
		s := at(fmt.Sprintf("s%d", i), "layer", i, 1)
		s.Args = map[string]any{"padding": big}
		spans = append(spans, s)
	}
	data, err := EncodeAttempt("a1", spans, Lost{})
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > MaxTraceSize {
		t.Fatalf("the stored trace takes %d bytes; want at most %d", len(data), MaxTraceSize)
	}
	a, err := DecodeAttempt(data)
	if err != nil {
		t.Fatal(err)
	}
	last := a.Spans[len(a.Spans)-1]
	if last.Name != Truncated || a.Spans[0].ID != "s0" {
		t.Errorf("kept %d spans from %q, ending with %q; want the first, ending with %q",
			len(a.Spans), a.Spans[0].ID, last.Name, Truncated)
	}
}

// TestEncodeAttemptWithin checks that an attempt's stored trace fits in a limit below MaxTraceSize, keeping its first
// spans and saying it was cut short, and that a trace within a limit above MaxTraceSize is stored as EncodeAttempt
// stores it.
func TestEncodeAttemptWithin(t *testing.T) {
	var spans []Span
	for i := range 100 {
		spans = append(spans, at(fmt.Sprintf("s%d", i), "layer", i, 1))
	}
	const limit = 2 << 10
	data, err := EncodeAttemptWithin("a1", spans, Lost{}, limit)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > limit {
		t.Fatalf("the stored trace takes %d bytes; want at most %d", len(data), limit)
	}
	a, err := DecodeAttempt(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Spans) < 2 {
		t.Fatalf("kept %d spans; want the first, and the instant saying the trace was cut short", len(a.Spans))
	}
	if last := a.Spans[len(a.Spans)-1]; last.Name != Truncated || a.Spans[0].ID != "s0" {
		t.Errorf("kept %d spans from %q, ending with %q; want the first, ending with %q",
			len(a.Spans), a.Spans[0].ID, last.Name, Truncated)
	}
	whole, err := EncodeAttemptWithin("a1", spans, Lost{}, 2*MaxTraceSize)
	if err != nil {
		t.Fatal(err)
	}
	if want, err := EncodeAttempt("a1", spans, Lost{}); err != nil || string(whole) != string(want) {
		t.Errorf("within twice MaxTraceSize = %d bytes, %v; want what EncodeAttempt stores, %d bytes", len(whole),
			err, len(want))
	}
}

// TestWithRecorderAt checks that spans started from a context it returns are recorded apart from ctx's,
// yet nest and lie as ctx's own would.
func TestWithRecorderAt(t *testing.T) {
	owner := NewRecorder("executor", nil)
	ctx, target := StartTrack(WithRecorder(context.Background(), owner, ""), `layer "app"`, `layer "app"`)
	attempt := NewRecorder("executor", nil)
	actx := WithRecorderAt(ctx, attempt)
	sctx, step := Start(actx, `run(["true"])`)
	Instant(sctx, "skipped")
	step.End()
	target.End()

	if got := ids(owner.Spans()); !slices.Equal(got, []string{target.ID()}) {
		t.Errorf("the owner recorded %v; want the target alone", got)
	}
	got := attempt.Spans()
	if len(got) != 2 {
		t.Fatalf("the attempt recorded %d spans; want 2", len(got))
	}
	for _, s := range got {
		if s.Track != `layer "app"` {
			t.Errorf("%s lies on %q; want the target's track", s.Name, s.Track)
		}
	}
	if got[1].Parent != target.ID() || got[0].Parent != got[1].ID {
		t.Errorf("parents %q and %q; want the step in the target and the instant in the step",
			got[1].Parent, got[0].Parent)
	}
	if Current(actx) != target {
		t.Error("Current of the attempt's context isn't the target's span")
	}

	untraced := NewRecorder("executor", nil)
	_, s := Start(WithRecorderAt(context.Background(), untraced), "step")
	s.End()
	if got := untraced.Spans(); len(got) != 1 || got[0].Parent != "" || got[0].Track != "step" {
		t.Errorf("from an untraced context it recorded %+v; want one span of no parent, on its own track", got)
	}
}
