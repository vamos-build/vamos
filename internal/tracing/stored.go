package tracing

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// MaxTraceSize bounds each stored trace document whole, as cache.MaxLogSize bounds a log.
// It is below what one remote ReadMany may return, so that stored traces batch as trees and logs do.
const MaxTraceSize = 16 << 20

// Version is the version of the stored trace encoding that this Vamos writes, and the only one it reads.
const Version = 1

// KindAttempt is the Kind of an attempt's stored trace, which Attempt holds.
// The documents that name other blobs, a job's and a command's, are other packages' kinds.
const KindAttempt = "attempt"

// Truncated names the instant that a trimmed trace holds in place of the spans it lost.
const Truncated = "trace truncated"

// Header begins every stored trace document, so that a reader learns what a blob holds before decoding the rest.
// A document embeds it first.
type Header struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
}

// ErrNotTrace is returned for data that isn't a stored trace document of a version this Vamos reads.
var ErrNotTrace = errors.New("not a stored trace")

// ReadHeader returns the header of the stored trace document data,
// or an error wrapping ErrNotTrace if data isn't one, or is one of another Version.
func ReadHeader(data []byte) (Header, error) {
	var h Header
	if err := json.Unmarshal(data, &h); err != nil {
		return Header{}, fmt.Errorf("%w: %w", ErrNotTrace, err)
	}
	if err := h.check(); err != nil {
		return Header{}, err
	}
	return h, nil
}

// check returns an error wrapping ErrNotTrace unless h is the header of a stored trace of this Version.
func (h Header) check() error {
	if h.Kind == "" {
		return fmt.Errorf("%w: it has no kind", ErrNotTrace)
	}
	if h.Version != Version {
		return fmt.Errorf("%w: its version %d is not %d", ErrNotTrace, h.Version, Version)
	}
	return nil
}

// maxAttemptID bounds an attempt's ID, which is 128 random bits in hex, far below it,
// so that its document always leaves room for its spans, or for the instant that says they were lost.
const maxAttemptID = 1 << 10

// Attempt is the stored trace of one attempt at a target's or a pull's action.
type Attempt struct {
	Header
	ID    string `json:"id"` // the attempt's, which a job's references to it name too
	Spans []Span `json:"spans"`
}

// EncodeAttempt returns the stored trace of the attempt id, holding spans,
// trimmed with Trim to MaxTraceSize with what lost says the recorder refused.
func EncodeAttempt(id string, spans []Span, lost Lost) ([]byte, error) {
	return EncodeAttemptWithin(id, spans, lost, MaxTraceSize)
}

// EncodeAttemptWithin is EncodeAttempt with spans trimmed to fit the document in limit bytes, rather than
// MaxTraceSize, so that a test can see a trace cut short without recording megabytes of spans.
// A limit too small for the document with none of its spans gives it none, and the instant that says so if it fits.
func EncodeAttemptWithin(id string, spans []Span, lost Lost, limit int) ([]byte, error) {
	if len(id) > maxAttemptID {
		return nil, fmt.Errorf("an attempt's ID of %d bytes is longer than %d", len(id), maxAttemptID)
	}
	a := Attempt{Version: Version, Kind: KindAttempt, ID: id, Spans: []Span{}}
	empty, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	if a.Spans = Trim(spans, lost, min(limit, MaxTraceSize)-len(empty)+len("[]")); a.Spans == nil {
		a.Spans = []Span{}
	}
	return json.Marshal(a)
}

// DecodeAttempt decodes an attempt's stored trace, failing with an error wrapping ErrNotTrace
// if data is no stored trace, or another kind of one.
// The header is read first, so that a document of another version fails as one,
// whatever its other fields hold.
func DecodeAttempt(data []byte) (Attempt, error) {
	h, err := ReadHeader(data)
	if err != nil {
		return Attempt{}, err
	}
	if h.Kind != KindAttempt {
		return Attempt{}, fmt.Errorf("%w: it is a %s trace, not an attempt's", ErrNotTrace, h.Kind)
	}
	var a Attempt
	if err := json.Unmarshal(data, &a); err != nil {
		return Attempt{}, fmt.Errorf("%w: %w", ErrNotTrace, err)
	}
	return a, nil
}

// Lost is what spans a recorder refused, or a trim dropped: how many, and where the one that started first lay.
type Lost struct {
	Count int
	// First is the earliest-started span lost, with only its ID, process, track and start,
	// so that a large span refused isn't kept: its zero value while Count is 0.
	First Span
}

// add counts s as lost.
func (l *Lost) add(s Span) {
	l.addMany(s, 1)
}

// addMany counts n spans as lost, of which s started first.
func (l *Lost) addMany(s Span, n int) {
	if l.Count == 0 || s.Start.Before(l.First.Start) {
		l.First = Span{ID: s.ID, Process: s.Process, Track: s.Track, Start: s.Start}
	}
	l.Count += n
}

// spanSize returns the size of s encoded in a stored trace, or false if it can't be encoded.
func spanSize(s Span) (int, bool) {
	data, err := json.Marshal(s)
	if err != nil {
		return 0, false
	}
	return len(data), true
}

// Trim returns spans, less as many of them as their encoding as a JSON array must lose to take at most limit bytes,
// dropping the latest-started first, so that the spans within a span go before it and a trace keeps its first steps;
// of spans that start together, the shorter goes first, as it nests in the longer,
// and of those that also end together, the one within the other.
// If it dropped any, or lost counts any that a recorder refused, it adds one Truncated instant,
// with the count of both as its "dropped" argument, on the track of the earliest-started span lost,
// and leaves room for it.
// A span that can't be encoded is dropped and counted too,
// and a Truncated instant that spans already hold, from an earlier trim, is folded into its own.
// What it returns always fits in limit: it is empty if limit can't hold even the instant.
// It returns spans itself when they fit and lost counts none.
func Trim(spans []Span, lost Lost, limit int) []Span {
	type sized struct {
		i    int
		size int
	}
	all := make([]sized, 0, len(spans))
	sum := 0 // of the sizes of all[:kept]
	for i, s := range spans {
		if n, ok := truncated(s); ok {
			lost.addMany(Span{ID: strings.TrimSuffix(s.ID, "/truncated"), Process: s.Process, Track: s.Track,
				Start: s.Start}, n)
			continue
		}
		size, ok := spanSize(s)
		if !ok {
			lost.add(s)
			continue
		}
		all = append(all, sized{i, size})
		sum += size
	}
	// arrayLen is the size of the first n of all as a JSON array, with room for more bytes and their comma.
	arrayLen := func(n, more int) int {
		elems, size := n, sum+more
		if more > 0 {
			elems++
		}
		return len("[]") + size + max(elems-1, 0)
	}
	kept := len(all)
	if lost.Count == 0 && arrayLen(kept, 0) <= limit {
		return spans
	}
	instant := truncation(lost)
	instantSize, _ := spanSize(instant)
	if arrayLen(kept, instantSize) > limit {
		// By start, and the longer first, as WriteChrome draws them, then the outer first,
		// so that dropping from the end drops children first.
		depth := depths(spans)
		slices.SortStableFunc(all, func(a, b sized) int {
			x, y := spans[a.i], spans[b.i]
			return cmp.Or(x.Start.Compare(y.Start), y.End.Compare(x.End), cmp.Compare(depth[a.i], depth[b.i]))
		})
		// The instant's size depends on what was lost, so it is measured again after each span dropped.
		for kept > 0 && arrayLen(kept, instantSize) > limit {
			kept--
			sum -= all[kept].size
			lost.add(spans[all[kept].i])
			instant = truncation(lost)
			instantSize, _ = spanSize(instant)
		}
	}

	keep := make([]bool, len(spans))
	for _, s := range all[:kept] {
		keep[s.i] = true
	}
	out := make([]Span, 0, kept+1)
	for i, s := range spans {
		if keep[i] {
			out = append(out, s)
		}
	}
	if lost.Count > 0 && arrayLen(kept, instantSize) <= limit {
		out = append(out, instant)
	}
	return out
}

// depths returns how many of spans each span is within, by its parents among them.
// Each span's depth is computed once, by walking up to the nearest ancestor whose depth is known,
// so that a deep chain of spans costs time in proportion to its length.
func depths(spans []Span) []int {
	index := make(map[string]int, len(spans))
	for i, s := range spans {
		index[s.ID] = i
	}
	const unknown, visiting = -1, -2
	depth := make([]int, len(spans))
	for i := range depth {
		depth[i] = unknown
	}
	var chain []int
	for i := range spans {
		// Walk up until a span of known depth, or none, or one this walk already passed, as a cycle would.
		chain = chain[:0]
		base := 0
		for j := i; ; {
			if depth[j] >= 0 {
				base = depth[j] + 1
				break
			}
			if depth[j] == visiting {
				break // malformed input: a span within itself; the cycle counts from here
			}
			depth[j] = visiting
			chain = append(chain, j)
			p, ok := index[spans[j].Parent]
			if spans[j].Parent == "" || !ok {
				break
			}
			j = p
		}
		for _, j := range slices.Backward(chain) {
			depth[j] = base
			base++
		}
	}
	return depth
}

// truncated returns the count of spans that s, if it is a Truncated instant, says were dropped.
func truncated(s Span) (int, bool) {
	if !s.Instant || s.Name != Truncated || !strings.HasSuffix(s.ID, "/truncated") {
		return 0, false
	}
	n := 1 // the instant's own place, at least, if its count is missing or makes no sense
	switch c := s.Args["dropped"].(type) {
	case int:
		n = max(c, n)
	case float64: // as a stored trace decodes
		n = max(int(c), n)
	}
	return n, true
}

// truncation returns the instant that says a trace lost what lost counts, where its first span would have been.
// It is part of no span, since the span its first was part of may be lost too.
func truncation(lost Lost) Span {
	first := lost.First
	return Span{
		ID:      first.ID + "/truncated",
		Process: first.Process,
		Track:   first.Track,
		Name:    Truncated,
		Start:   first.Start,
		End:     first.Start,
		Instant: true,
		Args:    map[string]any{"dropped": lost.Count},
	}
}
