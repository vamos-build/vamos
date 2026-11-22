# Design

## Context

See proposal.md for motivation, and the `trace-export` and `operational-metrics` specs for required behavior.

`persistent-traces` (all tasks done, awaiting archive) records every job and attempt, always. On a coordinator, spans become
complete documents at two points, both in `sched`, which the local executor shares:

- `Sharing.attempt` snapshots the attempt's recorder when its runner returns and stores it with `saveTrace`. On a
  coordinator the runner is the pool, which relays the worker's spans, shifted onto the coordinator's clock, into the
  attempt's context (`tracing.Relay`), so this snapshot holds the worker's spans, failed tries included. Each batch of
  events carries the offset the worker estimated before the attempt started, the same for the whole attempt, so the
  relayed spans keep the order they ran in, and so do the spans exported from them.
- `sched.RunJob`, through `Coordinator.traced`, snapshots the job's recorder in `JobTrace.Save` before the job's
  outcome is returned.

The attempt's context is `context.WithoutCancel` of the starting job's, so it carries the starter's context values.
vamos span IDs are a recorder's random 4-byte prefix and a counter, unique within a job's and its attempts' recorders.
Arrows are `Span.Produces`/`Span.Uses` tokens (`graph.ImageFlow`, `graph.TreeFlow`): a commit produces an image, which
a later `start` uses, and the client's load produces trees, which steps use. The pool has no span per try: a retry is an
argument on the current span. Journal records hold each job's request whole, so a field added to a request survives a
restart. The coordinator's instance survives a restart that keeps `Config.State`. `remote.Server.Middleware` is the seam
that tests already use to inject drops. OpenTelemetry's API, metric API and `otelhttp` v0.71.0 are indirect
dependencies, through buildah. The SDK, the exporters and `autoexport` are not.

## Goals / Non-Goals

**Goals:**

- The exported trace and the stored trace come from the same snapshot of spans, so what one shows the other shows,
  truncation included.
- `sched`, `tracing` and `api` take no OpenTelemetry dependency. One new package, `internal/telemetry`, holds the SDK.
- Telemetry that is off costs a build nothing beyond a no-op meter's calls.

**Non-Goals:**

- Export from the local executor, or from `vamos coordinator --local`, which serves one. Both trace to their stores, as
  today.
- Exporting the client's spans. The client may run under a CI tracer of its own, which `TRACEPARENT` joins.
- Live span export as spans end. A job's own spans are exported when it ends and an attempt's when its runner returns,
  as they are stored (see Risks).
- Logs over OpenTelemetry.
- Links from a hit to the attempt that made its result. That attempt's trace ID isn't kept with the result, and it may
  belong to another instance. The hit's span carries the attempt's ID and the attempt trace's digest as attributes.
- One trace per command. Each job is a trace, as the proposal says. Without `TRACEPARENT`, a push's build and copy are
  two traces.

## Decisions

### One package bridges vamos spans to OpenTelemetry

`internal/telemetry` imports `tracing` and the OpenTelemetry SDK, and no other vamos package. `TestImportRules` gives it
`internal: ["tracing"]` and forbids Starlark, buildah and the container store. `coord` and `cli` may import it. `sched`
must not, which a `not` entry on `sched`'s rule states.

`sched` gains a hook it calls with each snapshot it stores:

```go
// SpanSink is told each snapshot of spans that sched stores, a job's or an attempt's, under the context of the job
// that recorded it, or that started the attempt.
type SpanSink interface {
    AttemptStarted(ctx context.Context, id string) tracing.Exported // the attempt span's IDs, for joiners' links
    Attempt(ctx context.Context, id string, spans []tracing.Span, start, end time.Time)
    Job(ctx context.Context, spans []tracing.Span, start, end time.Time, outcome string)
}
```

`Sharing` takes it through `NewSharing`'s options and calls `Attempt` beside `saveTrace`, whether or not the store
succeeded. `RunJob` takes it as an argument and calls `Job` beside `JobTrace.Save`. A nil sink, the local executor's,
costs nothing. `saveTrace` trims an attempt's spans once, to `Sharing`'s limit (`tracing.MaxTraceSize`, or the smaller
one `LimitTraces` sets in tests), and passes that slice both to the encoder, which then has nothing more to trim, and to
the sink; `RunJob` does the same with the job's. So an exported trace drops what the stored one dropped.

*Alternative:* a recorder callback that exports each span as it ends. Rejected: recorders keep spans for storage, so a
second path would have to agree with `Trim`, and a worker's spans arrive relayed in batches anyway. See Risks for the
delay this costs.

*Alternative:* `coord` wraps the content store and decodes each stored trace. Rejected: it would parse every blob put,
and the job's identity isn't in the blob.

### The job's identity rides in the context

`tracing` gains a value type with no dependency:

```go
// Exported is how a job's trace was exported: its trace ID and the ID of the job's span, in hex, and whether it was
// sampled; the zero value means it wasn't exported.
type Exported struct{ TraceID, SpanID string; Sampled bool }
func WithExported(ctx context.Context, e Exported) context.Context
func ExportedOf(ctx context.Context) Exported
```

When a job starts, `Coordinator.traced` asks the bridge for the job's export context, `telemetry.Bridge.Job(ctx,
JobInfo{Instance, ID, Kind, Workspace, Parent})`. That context carries `tracing.Exported` and the bridge's own job
record, which holds the sampling decision, the start time and the workspace. Every attempt the job starts inherits it,
so `Attempt` exports into the starter's trace even after the starter has left.

`JobTrace.Save` copies `ExportedOf(ctx)` into a new `api.JobTrace.Exported` field. CLI assembly writes `otel trace` and
`otel sampled` arguments on the job's top-level spans, so `--trace` and `vamos trace` name the trace ID, as the user
chose. They print nothing more on standard error. A manifest stored before this change, or by a coordinator that exports
nothing, has no `Exported`, and its spans get no such arguments.

### IDs are derived by hashing

- Trace ID: the outer `traceparent`'s trace ID if the job has one, else the first 16 bytes of
  SHA-256(`"vamos/trace\0"` ‖ instance ‖ `\0` ‖ job ID).
- The job's span: the first 8 bytes of SHA-256(`"vamos/job\0"` ‖ instance ‖ `\0` ‖ job ID). Its parent is the outer span,
  if any.
- An attempt's span: SHA-256(`"vamos/attempt\0"` ‖ attempt ID), since attempt IDs are 128 random bits.
- Every other span: SHA-256(`"vamos/span\0"` ‖ instance ‖ `\0` ‖ job ID ‖ `\0` ‖ vamos span ID), with the starter's job
  ID for an attempt's spans. A span whose vamos parent is set takes that parent's derived ID. A job's top-level span
  takes the job's span as its parent. An attempt's top-level span takes the attempt's span, whose own parent is the
  starter's target span, which `WithRecorderAt` already names.

The trace ID depends only on the instance and job, so a job resumed after a restart that kept its state goes on in the
same trace. Its second run's spans have new vamos IDs, since recorders' prefixes are random, and so new span IDs. A
derived all-zero ID, which OpenTelemetry treats as invalid, is replaced by flipping its last bit.

The job's and the attempt's spans are synthetic. vamos has no single span for either. The job's span runs from the
job's start to `RunJob`'s return, and is exported last, in `Job`. The attempt's span runs from the attempt's start to
its runner's return, and is exported in `Attempt`.

*Alternative:* random IDs, mapped as spans are converted. Rejected: a resumed job would start a new trace, and a joiner
would need the starter's map.

### Spans are made through the SDK with chosen IDs and times

The bridge uses a `TracerProvider` built with an `IDGenerator` that reads the wanted IDs from the context it is handed,
with the configured span processor and exporter. Each vamos span becomes `tracer.Start(ctx, name,
WithTimestamp(start), WithLinks(...), WithAttributes(...))` and `End(WithTimestamp(end))`, under a context that holds the
parent as a remote `SpanContext` with the job's sampled flag. The provider's sampler is `ParentBased(AlwaysOn)`, so the
flag set on that parent decides. The job's decision is made once (below). An instant becomes a zero-length span. An
`error` argument sets the span's status to `Error` with the argument as its description.

Task 1.2 proves this against the pinned SDK before anything else relies on it. If the SDK can't take a chosen span ID
for a span with a remote parent, the fallback is to build `ReadOnlySpan`s from `tracetest.SpanStub` snapshots and hand
them to the batch processor directly. The fallback changes only `telemetry`, since exemplars don't depend on SDK spans
(below).

### Sampling is decided once per job

`OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG` are parsed by `telemetry`, not by the SDK's environment reading,
since the decision is needed as the job starts, before any span. The bridge supports the specification's values:
`always_on`, `always_off`, `traceidratio`, and `parentbased_` with each of the three. Any other value, or a ratio outside
[0, 1], fails the coordinator's start. The bridge asks the sampler once, with the job's trace ID and its outer parent's
`SpanContext`, when the job starts, and keeps the result with the job. Ratio samplers are a function of the trace ID,
and the outer flag is in the job's record, so a resumed job gets the same decision. Spans of a dropped trace aren't
made at all. Their metrics are still counted, without exemplars.

### Links

- A join: `sched.AttemptTrace` gains `Exported tracing.Exported`, the attempt span's trace and span ID, which `Sharing`
  takes from the bridge when the attempt starts, through a `SpanSink.AttemptStarted(ctx, id) tracing.Exported` method.
  A joiner's target span, as it is converted, links to it. The bridge finds the target span through the manifest entry,
  whose `Attempt` the `Traced` output now records on the target span as an `attempt` argument.
- Arrows: the bridge keeps a bounded LRU of 65,536 `Produces` tokens to the exported span that produced them. Within one
  snapshot it indexes producers first. A span whose `Uses` token is in the index links to that span, whichever trace it
  is in. A token not found, such as a tree the client's load made, gives no link. Producers of a dropped trace aren't
  indexed. A link into a trace that wasn't kept would mislead.

### Attributes

| vamos | exported |
| --- | --- |
| the job's span | `vamos.job`, `vamos.job.kind` (`build`, `push`, `publish`, `export`), `vamos.workspace` |
| every span | `vamos.process`, `vamos.track`, and `vamos.workspace` for a named workspace |
| target span | `vamos.target` (`graph.Node.Target`, which `sched` now sets as a `target` argument), `vamos.miss.cause`, `vamos.miss.added`/`removed`/`changed` as string slices, `vamos.attempt` |
| any other argument | `vamos.arg.<key>`, with spaces as `_`; numbers, booleans and strings as themselves, and other values as JSON |

The resource is `service.name` `vamos-coordinator` or `vamos-worker` unless `OTEL_SERVICE_NAME` says otherwise,
`service.version`, and `service.instance.id`: the coordinator's instance, or the worker's host name. `OTEL_RESOURCE_ATTRIBUTES`
adds to it. The SDK's attribute limits, which `OTEL_ATTRIBUTE_*_LIMIT` set, bound what a span carries.

### Configuration and its defaults

`telemetry.Setup(role, getenv)` builds the providers, called by `inv.coordinator` and `inv.worker` before they serve.
Behind it, `autoexport` builds the exporters, so `OTEL_EXPORTER_OTLP_*` (protocol, headers, timeouts, TLS),
`OTEL_BSP_*`, `OTEL_METRIC_EXPORT_*` and `OTEL_EXPORTER_PROMETHEUS_HOST`/`PORT` mean what the specification says.
Before calling it, `Setup` settles two things itself:

- **Nothing configured exports nothing.** An unset `OTEL_TRACES_EXPORTER` or `OTEL_METRICS_EXPORTER` means `otlp` if
  `OTEL_EXPORTER_OTLP_ENDPOINT` or that signal's own endpoint variable is set, and `none` otherwise. The SDK's own
  default, `otlp` to `localhost:4318`, would make every coordinator try to send to a collector nobody set up.
  `OTEL_SDK_DISABLED=true` means `none` for both.
- **Mistakes fail the start.** Exporter names are checked against the lists the specs give, before `autoexport`, which
  would otherwise fall back with only a log line. The error names the variable, its value and the values taken. For a
  worker, `prometheus` is refused and `OTEL_TRACES_EXPORTER` is warned about and ignored.

The Prometheus exporter is served by the bridge on its own `http.Server` at `/metrics`, on
`OTEL_EXPORTER_PROMETHEUS_HOST:PORT`, `localhost:9464` by default, listening before the coordinator serves its API, so
that a port in use fails the start. It serves OpenMetrics when asked, which carries exemplars.

Export errors go to `otel.SetErrorHandler`, which logs them through `inv.warn`, at most once a minute for each distinct
message, with a count of those suppressed. On shutdown, `Setup`'s returned `shutdown` flushes and closes the providers,
bounded by 10 seconds. It runs after the API stops, so the last jobs' spans are in it.

### Metrics are derived from spans, gauges from state, and counters where things happen

Histograms and per-target counts are made in the bridge from the spans it converts, recorded under a context holding the
span's `SpanContext` with the job's sampled flag. The SDK's default `trace_based` exemplar filter then attaches the
exemplar only for sampled spans, as the spec says. To find those spans without parsing display names, the spans that
metrics read carry a `role` argument, whose values `tracing` names as constants:

| role | recorded by | metrics |
| --- | --- | --- |
| `target` | `sched`, on each layer's, task's and pull's span, with `kind` and `outcome` (`hit`, `miss`, `joined`, `failed`) | `vamos.cache.hits`, `.misses` (by `cause`), `.joins`, `.saved` (from `saved`), `.lost` (by `reason`, from a `lost` argument that lookup and regeneration set) |
| `wait` | the pool: a new `wait for a worker` span per try, from queuing to assignment | `vamos.attempts.wait` |
| `try` | the pool: a new span per try, from assignment to report or loss, with `platform`, `kind`, `outcome` (`succeeded`, `failed`, `lost`, `killed`) and the worker's usage | `vamos.attempts.duration`, `.cpu`, `.memory.peak` |
| the job's synthetic span | the bridge | `vamos.jobs.duration` by `kind` and `outcome` |

Kinds are `layer`, `task`, `pull`, `push`, `upload` and `tag`. The new `wait` and `try` spans also appear in stored
traces, where the pool's retries become visible as tries.

Gauges are observable instruments whose callbacks read state under the owner's lock, once per collection: the pool's
attempts by state and platform, workers and slots by platform, and builds waiting for a platform; `sched.Jobs`'s open
jobs by kind; and the stores' sizes. Counters of events with no span of their own are incremented where the event
happens, through a `coord.Metrics` struct of instruments that `coord.Config` takes. Its zero value is a no-op meter's.
These are refused jobs by reason (`busy`, `platform`, `version`, `generation`), expired leases, kills by reason
(`withdrawn`, `demand`, `stale`), rejected reports by reason (`stale`, `generation`), journal write failures, gaps in
event streams, which remote's watch serves, and retries. Each is created at start and so exported from zero. The worker
has its own three counters: fenced attempts, event batches the coordinator refused, and cgroups that `kill` couldn't
empty in time.

`otelhttp` wraps the API in `Server.Middleware`, composed outside any middleware the server already has. It reports
`http.server.request.duration` labeled by `http.route`. Since Go 1.22's `ServeMux`, `otelhttp` takes the route from
`Request.Pattern`. Task 1.3 checks that the pinned version does. If it doesn't, the middleware sets the route itself
from `Pattern` with `otelhttp.WithMetricAttributesFn`. Bytes moved come from a `Server.Transferred(direction, n)` hook
that the blob handlers call, so `remote` stays free of the metric SDK.

`runtime` instrumentation from `contrib/instrumentation/runtime` adds the Go runtime's metrics on both roles.

*Alternative:* a Prometheus client registry beside OpenTelemetry. Rejected: two metric systems, and no exemplars from
derived spans.

### The stores' sizes are running counts

`cache.Directory` keeps the content store's total size and blob count as atomics. They are set by one walk when the
directory opens, which runs in the background and makes the gauge report nothing until it has finished. They are then
adjusted by each put that created a file, each removal of a corrupt blob, and `ClearContent`. The record store is
counted the same way. `Directory.Size()` returns both and whether the walk has finished. `cachetest.Content` checks that
the size follows puts, duplicate puts and clears.

*Alternative:* walk at every collection. Rejected: a store of millions of blobs, every 60 seconds.

### Attempts report their resources

After the attempt's subprocess exits and before its cgroup is removed, the worker reads `cpu.stat`'s `usage_usec` and
`memory.peak` (Linux 5.19 and later; absent before), and sends them in `AttemptReport.Usage`, a new optional field. The
pool sets them as `cpu` and `memory peak` arguments on the try's span, which the stored trace and the export both carry.
A killed attempt reports nothing, so its try has no usage. An older worker's report has none.

### TRACEPARENT travels on each request

The CLI reads `TRACEPARENT`, and `TRACESTATE` with it, when it opens a coordinator, and checks the traceparent with
`propagation.TraceContext`: version `00`, the right length and non-zero IDs. A malformed value is warned about once and
dropped, and an empty one is taken as unset. `GraphRequest`, `PushRequest`, `PublishRequest` and `ExportRequest` gain
`Parent string` (JSON `traceparent`) and `State string` (`tracestate`), as metadata like `Workspace`. The journal keeps
them with the request. The coordinator checks them again, and ignores and logs one it can't parse, since a client may be
older or another program. The local executor ignores them.

### The Helm chart

`values.yaml` gains:

```yaml
telemetry:
  otlp:
    endpoint: ""        # OTEL_EXPORTER_OTLP_ENDPOINT, for the coordinator and the workers
    protocol: ""        # OTEL_EXPORTER_OTLP_PROTOCOL
    headersSecret: ""   # a Secret whose key `headers` becomes OTEL_EXPORTER_OTLP_HEADERS
  traces:
    sampler: ""         # OTEL_TRACES_SAMPLER
    samplerArg: ""      # OTEL_TRACES_SAMPLER_ARG
  prometheus:
    enabled: false      # adds prometheus to the coordinator's OTEL_METRICS_EXPORTER
    port: 9464
    podAnnotations: false # prometheus.io/scrape, port and path on the coordinator's pod
```

With `otlp.endpoint` set, both roles get it. With Prometheus enabled, the coordinator gets `OTEL_METRICS_EXPORTER` with
`prometheus`, and `otlp` too if an endpoint is set, `OTEL_EXPORTER_PROMETHEUS_HOST=0.0.0.0` and the port, a
`containerPort` and a Service port named `metrics`. The defaults set nothing. `extraEnv` still overrides.

## Risks / Trade-offs

- [A job's own spans reach the backend only when the job ends, so a long build shows only its finished attempts while it
  runs] → Attempts, which hold most spans, are exported as each ends. Exporting job spans in batches as they end is a
  follow-up if operators need it, and needs no change to the specs.
- [The SDK may not take chosen IDs for spans with a remote parent] → Task 1.2 proves it first. The `SpanStub` fallback is
  confined to `telemetry`.
- [Hashing every span's ID costs CPU on a coordinator] → One SHA-256 of under 100 bytes per span, at export only.
  Sampled-out traces make no spans. Task 2.6's benchmark measures conversion of a large attempt trace.
- [The arrow index misses producers that were evicted or exported after their user] → Links are a convenience. A user
  exported before its producer, which can happen only across jobs, gets no link.
- [`role` arguments and the new `wait` and `try` spans change what stored traces show] → They add spans and arguments
  and change none. The trace feature's scenarios check nesting and names they already list. The new spans nest within
  the attempt's.
- [Gauges' callbacks take the pool's and jobs' locks on every collection] → Once per export interval or scrape, reading
  counts that are kept as maps by platform, not walked.
- [`autoexport` pulls in gRPC and the Prometheus client] → gRPC is already compiled in through buildah's dependencies.
  The Prometheus client and `prometheus/common` are new. Binary size is checked in task 1.1.
- [A misconfigured collector floods the coordinator's log] → The error handler logs each distinct message at most once
  a minute.
- [Exemplars depend on scraping OpenMetrics] → The classic text format has no exemplars. The chart's annotations and
  the docs say so. OTLP carries them either way.

## Migration Plan

Nothing is exported until an operator sets `OTEL_*` variables or the chart's `telemetry` values. A coordinator or worker
without them behaves as before, apart from the new `wait` and `try` spans and `role` arguments in stored traces.
Requests' new fields are optional both ways: an older client sends no `traceparent`, and an older coordinator ignores
it. An older worker's report has no usage. Rolling back drops the export and leaves stored traces readable, since their
extra arguments are only arguments.
