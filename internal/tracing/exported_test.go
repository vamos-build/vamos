package tracing

import (
	"context"
	"testing"
)

// TestExportedRidesTheContext checks that what WithExported puts in a context, ExportedOf finds in it and in the
// contexts made from it, as an attempt's is from its starter's, and that a context without one gives the zero value.
func TestExportedRidesTheContext(t *testing.T) {
	if got := ExportedOf(context.Background()); got != (Exported{}) {
		t.Errorf("ExportedOf a context without one = %+v, want the zero value", got)
	}
	want := Exported{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", Sampled: true}
	ctx := WithExported(context.Background(), want)
	inherited := context.WithoutCancel(WithRecorderAt(ctx, NewRecorder("executor", nil)))
	for name, c := range map[string]context.Context{"its own": ctx, "one made from it": inherited} {
		if got := ExportedOf(c); got != want {
			t.Errorf("ExportedOf %s = %+v, want %+v", name, got, want)
		}
	}
}
