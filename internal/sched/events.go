package sched

import (
	"github.com/vamos-build/vamos/internal/graph"
	"github.com/vamos-build/vamos/internal/tracing"
)

// Events receives a run's progress, within the executor: a job sends them on as api.Events.
// Its methods may be called from several goroutines.
type Events interface {
	// Status reports what is happening to node, such as "building" or "cached".
	Status(node graph.Ref, status string)
	// Output passes on what node's run() steps print,
	// or printed when its cached result was made, to stream: cache.StreamStdout or cache.StreamStderr.
	// data is valid only during the call.
	Output(node graph.Ref, stream byte, data []byte)
	// Spans passes on spans of a trace that the client asked for, in batches: an attempt's as it ends, and the
	// job's own once its work is done.
	Spans(spans []tracing.Span)
}
