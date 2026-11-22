package tracing

import "context"

// Exported is how a job's trace was exported over OpenTelemetry: its trace ID and the ID of the job's span, in hex,
// and whether it was sampled. The zero value means it wasn't exported.
type Exported struct {
	TraceID string `json:"trace"`
	SpanID  string `json:"span"`
	Sampled bool   `json:"sampled,omitempty"`
}

type exportedKey struct{}

// WithExported returns ctx carrying e, which work done within it, and the attempts it starts, inherit.
func WithExported(ctx context.Context, e Exported) context.Context {
	return context.WithValue(ctx, exportedKey{}, e)
}

// ExportedOf returns what ctx's job was exported as, or the zero Exported if it wasn't.
func ExportedOf(ctx context.Context) Exported {
	e, _ := ctx.Value(exportedKey{}).(Exported)
	return e
}

// Role is the argument that names what a span is, for whoever reads spans to measure them, such as a coordinator's
// metrics, rather than its display name, which may change.
const Role = "role"

// Shared is the argument, true, of a span a job holds a copy of, of an attempt that served it, such as the attempt's
// cache write, which every job the attempt served holds; an exporter exports it once, with the attempt's.
const Shared = "shared"

// The values of a span's Role argument.
const (
	// RoleTarget is a target's span, a layer's, task's or pull's, which says whether it hit, missed or joined.
	RoleTarget = "target"
	// RoleWait is an attempt's wait for a worker to take it, from its queuing to its assignment.
	RoleWait = "wait"
	// RoleTry is one try of an attempt on a worker, from its assignment to its report or its loss.
	RoleTry = "try"
)
