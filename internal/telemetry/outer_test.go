package telemetry

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// TestOuterTrace checks which traceparents a job joins: a valid one, with its tracestate, and none for an empty one;
// a later version's; and that one malformed, of the invalid version ff, of a wrong length, with an all-zero ID or in
// upper case isn't one, and that a tracestate that isn't one is dropped, saying so.
func TestOuterTrace(t *testing.T) {
	const valid = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	for _, c := range []struct {
		parent, state         string
		wantParent, wantState string
		wantErr               error
	}{
		{parent: valid, state: "ci=pipeline", wantParent: valid, wantState: "ci=pipeline"},
		{parent: " " + valid + " ", wantParent: valid},
		{parent: valid, state: "not a tracestate", wantParent: valid, wantErr: ErrNotTracestate},
		// Empty members, which the W3C allows, are dropped, not the whole state.
		{parent: valid, state: "ci=pipeline, \t,other=kept,", wantParent: valid, wantState: "ci=pipeline,other=kept"},
		// Only spaces and tabs make a member empty; a leading space within one is the W3C's optional whitespace.
		{parent: valid, state: "ci=pipeline,\n,other=kept", wantParent: valid, wantErr: ErrNotTracestate},
		{parent: valid, state: "\n", wantParent: valid, wantErr: ErrNotTracestate},
		{parent: valid, state: " \t ", wantParent: valid},
		{parent: valid, state: "a=" + strings.Repeat("b", maxTracestateSize), wantParent: valid,
			wantErr: ErrNotTracestate},
		{parent: valid, state: "ci=pipeline, other=kept", wantParent: valid, wantState: "ci=pipeline,other=kept"},
		// At most 32 slots, empty ones counted.
		{parent: valid, state: "a=1" + strings.Repeat(",", 31), wantParent: valid, wantState: "a=1"},
		{parent: valid, state: "a=1" + strings.Repeat(",", 32), wantParent: valid, wantErr: ErrNotTracestate},
		{parent: ""},
		{parent: "  "},
		{parent: "00-not-a-trace", wantErr: ErrNotTraceparent},
		// A later version's, with a field more, as the W3C recommends reading it.
		{parent: "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-" + strings.Repeat("later", 1<<10),
			wantParent: valid}, // written as version 00, without the fields it doesn't know
		{parent: valid + "-", wantErr: ErrNotTraceparent},
		// The flags version 00 defines, the random trace ID's of Trace Context Level 2 beside the sampled one, but no
		// later version's others, which version 00 would refuse.
		{parent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-03",
			wantParent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-03"},
		{parent: "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-ff",
			wantParent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-03"},
		{parent: "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-05",
			wantParent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		{parent: "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", wantErr: ErrNotTraceparent},
		{parent: "00-00000000000000000000000000000000-00f067aa0ba902b7-01", wantErr: ErrNotTraceparent},
		{parent: "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01", wantErr: ErrNotTraceparent},
		{parent: "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01", wantErr: ErrNotTraceparent},
	} {
		parent, state, err := OuterTrace(c.parent, c.state)
		if err == nil && parent != "" { // what a job joins reads again as itself, as a resumed job reads it
			if again, _, err := OuterTrace(parent, ""); err != nil || again != parent {
				t.Errorf("OuterTrace(%q) = %q, %v; want it unchanged", parent, again, err)
			}
		}
		if parent != c.wantParent || state != c.wantState || !errors.Is(err, c.wantErr) {
			t.Errorf("OuterTrace(%q, %q) = %q, %q, %v; want %q, %q, error %v", c.parent, c.state, parent, state, err,
				c.wantParent, c.wantState, c.wantErr)
		}
	}
}

// TestShowTraceValue checks that a value a warning shows is quoted, and one too long cut at a character's start.
func TestShowTraceValue(t *testing.T) {
	long := strings.Repeat("a", maxShownTrace-1) + "é" + "tail"
	for value, want := range map[string]string{
		"00-not-a-trace":                        `"00-not-a-trace"`,
		"a\x00b":                                `"a\x00b"`,
		long:                                    `"` + strings.Repeat("a", maxShownTrace-1) + `"…`,
		strings.Repeat("\x80", 2*maxShownTrace): strconv.Quote(strings.Repeat("\x80", maxShownTrace)) + "…",
	} {
		if got := ShowTraceValue(value); got != want {
			t.Errorf("ShowTraceValue(%q) = %s, want %s", value, got, want)
		}
	}
}
