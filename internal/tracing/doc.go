// Package tracing records spans independently of Vamos's build implementation, and encodes them for storage.
//
// # Invariants
//
// Spans are transportable data; recording is optional and context-scoped.
// This package imports no other Vamos package.
// Synchronous spans on a track must nest; overlapping work needs separate tracks or asynchronous spans.
// A recorder that keeps spans keeps at most twice MaxTraceSize of them, by their encoded size,
// refusing each span that would pass that and counting it as Lost, so that its memory is bounded
// whatever a build records.
// A recorder that passes spans to a callback passes none over MaxSpanSize, encoded,
// but a stub of it without its arguments, so that whoever sends spans one at a time can always send each.
// Every stored trace document begins with a Header, whose Version and Kind say how to read the rest,
// and fits in MaxTraceSize whole.
//
// # Lifecycle
//
// Active spans become completed data for a recorder or its callback.
// CallContext carries parentage across the executor boundary;
// callers own event transport, final flushing and clock alignment.
// Recording without a callback retains spans in memory until the recorder is released;
// callback mode delegates retention.
// WithRecorderAt gives work whose spans are stored apart from its caller's, such as an attempt's,
// a recorder of its own whose spans still nest under the caller's span and lie on its track.
// Trim fits a trace into a size when it is stored, and marks what its recorder lost in any trace written:
// it drops the latest-started spans until the trace fits,
// so that children go before the spans that hold them, even those that start and end with them,
// and a trace keeps its first steps,
// and leaves one Truncated instant counting what the recorder and the trim lost,
// and what any earlier trim of the same spans did.
//
// A job whose spans a coordinator exports over OpenTelemetry carries how in its context, WithExported's Exported,
// which the attempts it starts inherit, and which its stored manifest records,
// so that a stored trace names the exported one; this package only carries it, and exports nothing itself.
// Spans that are measured, rather than only shown, say what they are with a Role argument, RoleTarget, RoleWait or
// RoleTry, so that whoever measures them needn't parse their names.
//
// # Changes
//
// Inspect Recorder and Active for synchronization and ownership.
// StartTrack, StartAsync and Span's flow fields own visualization constraints.
// EstimateOffset and Shift in clock.go align clocks.
// stored.go owns the stored encoding, its Version, MaxTraceSize and Trim;
// documents of other kinds, which name other blobs, embed its Header and fit their spans with Trim.
// exported.go owns Exported and the Role values, which the telemetry package reads.
// WriteChrome in chrome.go owns ordering and timestamp precision required by Perfetto;
// consumers must preserve those rules.
package tracing
