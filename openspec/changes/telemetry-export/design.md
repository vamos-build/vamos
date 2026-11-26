# Design

## Context

See proposal.md for motivation, and the `trace-export` and `operational-metrics` specs for required behavior.

`persistent-traces` (all tasks done, awaiting archive) records every job and attempt, always. On a coordinator, spans
become complete documents at two points, both in `sched`, which the local executor shares:

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
dependencies, through buildah. The SDK and the exporters are not.

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
- Live span export as spans end. A job's own spans are exported when it ends and an attempt's once its result is
  recorded, as they are stored (see Risks).
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
    Attempt(ctx context.Context, id string, spans []tracing.Span, start, end time.Time)
    Job(ctx context.Context, spans []tracing.Span, start, end time.Time, outcome string)
}
```

`Sharing` takes it through `NewSharing`'s options and calls `Attempt` beside `saveTrace`, whether or not the store
succeeded. `RunJob` takes it as an argument and calls `Job` beside `JobTrace.Save`. A nil sink, the local executor's,
costs nothing. `saveTrace` trims an attempt's spans once, to `Sharing`'s limit (`tracing.MaxTraceSize`, or the smaller
one `LimitTraces` sets in tests), and passes that slice both to the encoder, which then has nothing more to trim, and to
the sink; `RunJob` does the same with the job's. So an exported trace drops what the stored one dropped.

The bridge doesn't convert a snapshot as the sink is called: it queues it, and converts the queue in order on a
goroutine of its own, so that a large attempt's conversion, some 85 ms for one of `tracing.MaxTraceSize`, isn't on the
build's path, and the sink's caller doesn't change the slice afterwards. The SDK's batch processor, whose queue holds
its default 2,048 spans, so that what it holds is bounded, blocks that goroutine when it is full, rather than drop a
span, so a snapshot is handed over whole, though the SDK sends it in batches, of which one may fail while another
arrives. What the bridge drops, when more than sixteen traces of `tracing.MaxTraceSize` wait, by an estimate of their
size, as they would for a collector that has fallen behind, is the next snapshot whole, and every later snapshot of its
job, whose trace is then cut short; it reports the drops through the error handler, once for each burst of them, from a
goroutine of its own. A job is cut alone, not the outer trace it may share with other jobs, and a span that joined an
attempt whose snapshot was dropped links to nothing. Every exporter of traces shares the converter, so one whose
collector stops answering holds up the others. Shutdown converts what is queued, then stops every batch processor and
metric reader at once, itself, since the providers stop theirs one after another and skip the rest once their context
ends, and then the providers; each step may use what the steps before left of the 10 seconds, but the least share each
later step is left: half for the processors and readers, so that a slow bridge leaves them time to send, and a second
for the providers. A step that runs out of time thus leaves the next a live context. Once the bridge's step is over,
done or not, the snapshots that still wait are dropped and reported, and the converter converts no more, but hands over
the one it was converting, in part if its step ran out of time.

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

When a job starts, `Coordinator.traced` asks the bridge for the job's export context, `telemetry.Bridge.StartJob(ctx,
JobInfo{ID, Kind, Workspace, Parent, State})`, named apart from `SpanSink.Job`, which the bridge also has. That context
carries `tracing.Exported` and the bridge's own job record, which holds the sampling decision, the start time and the
workspace. Every attempt the job starts inherits it, so `Attempt` exports into the starter's trace even after the
starter has left.

`JobTrace.Save` copies `ExportedOf(ctx)` into a new `api.JobTrace.Exported` field. CLI assembly writes `otel trace` and
`otel sampled` arguments on the job's top-level spans, so `--trace` and `vamos trace` name the trace ID, as the user
chose. They print nothing more on standard error. A manifest stored before this change, or by a coordinator that exports
nothing, has no `Exported`, and its spans get no such arguments.

### IDs are derived by hashing

- Trace ID: the outer `traceparent`'s trace ID if the job has one, else the first 16 bytes of
  SHA-256(`"vamos/trace\0"` ‖ instance ‖ `\0` ‖ job ID).
- The job's span: the first 8 bytes of SHA-256(`"vamos/job\0"` ‖ instance ‖ `\0` ‖ job ID), with `\0` ‖ run after it
  for a run after the first, as a resumed build's. Its parent is the outer
  span, if any.
- An attempt's span: SHA-256(`"vamos/attempt\0"` ‖ attempt ID), since attempt IDs are 128 random bits.
- Every other span: SHA-256(`"vamos/span\0"` ‖ instance ‖ `\0` ‖ job ID ‖ `\0` ‖ vamos span ID), with the starter's job
  ID for an attempt's spans. A span whose vamos parent is set takes that parent's derived ID. A job's top-level span
  takes the job's span as its parent. An attempt's top-level span takes the attempt's span, whose own parent is the
  starter's target span, which `WithRecorderAt` already names.

The trace ID depends only on the instance and job, so a job resumed after a restart that kept its state goes on in the
same trace. Its second run's spans have new vamos IDs, since recorders' prefixes are random, and so new span IDs. A
derived all-zero ID, which OpenTelemetry treats as invalid, is replaced by flipping its last bit.

Each run of a job has its own job span, derived from the run as well, but for the first run's, in the job's one trace,
so that a run cut short by a restart and the run that resumed it don't export two spans of one ID.

The job's and the attempt's spans are synthetic. vamos has no single span for either. The job's span runs from the
job's start to `RunJob`'s return, and is exported in `Job`, with the job's own spans, after its attempts'. The
attempt's span runs from the attempt's start until its result is recorded, so that it holds its cache write, and is
exported in `Attempt`; the result's provenance still times the runner alone.

*Alternative:* random IDs, mapped as spans are converted. Rejected: a resumed job would start a new trace, and a joiner
would need the starter's map.

### Spans are made through the SDK with chosen IDs and times

The bridge uses a `TracerProvider` built with an `IDGenerator` that reads the wanted IDs from the context it is handed,
with the configured span processor and exporter. Each vamos span becomes `tracer.Start(ctx, name, WithTimestamp(start),
WithLinks(...), WithAttributes(...))` and `End(WithTimestamp(end))`, under a context that holds the parent as a
`SpanContext` with the job's sampled flag, remote only for the outer span a job joined. The provider's sampler is
`ParentBased(AlwaysOn)`, so the flag set on that parent decides. The job's decision is made once (below). An instant
becomes a zero-length span. An `error` argument sets the span's status to `Error` with the argument as its description.

Task 1.2 proves this against the pinned SDK before anything else relies on it. If the SDK can't take a chosen span ID
for a span with a remote parent, the fallback is to build `ReadOnlySpan`s from `tracetest.SpanStub` snapshots and hand
them to the batch processor directly. The fallback changes only `telemetry`, since exemplars don't depend on SDK spans
(below).

### Sampling is decided once per job

`OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG` are parsed by `telemetry`, not by the SDK's environment reading,
since the decision is needed as the job starts, before any span. The bridge supports the specification's values:
`always_on`, `always_off`, `traceidratio`, and `parentbased_` with each of the three. Any other value, or a ratio
outside [0, 1], fails the coordinator's start. The bridge asks the sampler once, with the job's trace ID and its outer
parent's `SpanContext`, when the job starts, and keeps the result with the job. Ratio samplers are a function of the
trace ID, and the outer flag is in the job's record, so a resumed job gets the same decision. Spans of a dropped trace
aren't made at all. Their metrics are still counted, without exemplars.

### Links

- A join: the attempt's span ID is derived from its ID, so the bridge needs no word from `Sharing` as the attempt
  starts; it remembers an attempt once its snapshot is queued, and a joiner's target span, as it is converted, links
  to it: a span whose `role` is `target`, since others, such as a
  cache write's, name the attempt they are part of. The bridge finds the target span through the manifest entry,
  whose `Attempt` the `Traced` output now records on the target span as an `attempt` argument.
- Arrows: the bridge keeps a bounded LRU of 65,536 `Produces` tokens to the exported span that produced them. Within one
  snapshot it indexes producers first. A span whose `Uses` token is in the index links to that span, whichever trace it
  is in. A token not found, such as a tree the client's load made, gives no link. Producers of a dropped trace aren't
  indexed. A link into a trace that wasn't kept would mislead.

### Attributes

| vamos | exported |
| --- | --- |
| the job's span | `vamos.job`, `vamos.job.kind` (`build`, `push`, `publish`), `vamos.job.run` (0, then 1 for each resumption), `vamos.workspace` |
| every span | `vamos.process`, `vamos.track`, and `vamos.workspace` for a named workspace |
| target span | `vamos.target` (`graph.Node.Target`, which `sched` now sets as a `target` argument), `vamos.miss.cause`, `vamos.miss.added`/`removed`/`changed` as string slices, `vamos.attempt` |
| any other argument | `vamos.arg.<key>`, with spaces as `_`; numbers, booleans and strings as themselves, and other values as JSON |

The resource is `service.name` `vamos-coordinator` or `vamos-worker` unless `OTEL_SERVICE_NAME` says otherwise,
`service.version`, and `service.instance.id`: the coordinator's instance, or the worker's host name.
`OTEL_RESOURCE_ATTRIBUTES` adds to it. The bridge bounds what an exported span holds itself, since the SDK's limits
count runes rather than bytes, keep alive what they cut, and bound neither a list's length nor a span's keys or status:
a span's name and status, each key and string, each list's length, and its attributes in all, past which it keeps only
`vamos.truncated`, so that a span holds some 20 KiB and the 2,048 spans an exporter's queue holds some 40 MiB. The
attributes the table names come first, so that a span over its budget keeps them. Every string is made valid UTF-8,
since OTLP's protobuf encoding refuses a whole request for one that isn't, as a file's name may not be. A request sends
128 spans unless `OTEL_BSP_MAX_EXPORT_BATCH_SIZE` says otherwise, rather than the SDK's 512, which could pass the 4 MiB
a gRPC receiver takes by default.

### Configuration and its defaults

`telemetry.Setup(Config{Role, Instance, Version, Getenv, Warn})` builds the providers, called by `inv.coordinator` and
`inv.worker` before they serve. It reads the variables through `Getenv`, so that a test can give one coordinator of many
in a process its own, and builds the exporters itself, OTLP over HTTP or gRPC, console and Prometheus, passing what it
read as the exporters' and processors' options: the exporters, the sampler, the OTLP endpoints, protocols, headers and
timeouts, `OTEL_BSP_*`, `OTEL_METRIC_EXPORT_*`, `OTEL_EXPORTER_PROMETHEUS_HOST`/`PORT`, `OTEL_SERVICE_NAME` and
`OTEL_RESOURCE_ATTRIBUTES`, each meaning what the specification says, and each passed, its default if it is unset, so
that the exporters don't take a setting `Getenv` left unset, such as another collector's headers, from the process's
environment. An unset endpoint is the specification's plain `http://localhost`, unless
`OTEL_EXPORTER_OTLP_[SIGNAL_]INSECURE=false` or a certificate variable asks for TLS, and is given whole, so that neither
the SDK's own default, which is TLS, nor an ambient endpoint's scheme decides the transport's security. A certificate
the exporters load, a CA or a client certificate with its key, means TLS, as the gRPC exporter uses one whatever the
scheme says and the HTTP one refuses one for plaintext, so an `http` endpoint beside one fails `Setup`, naming both, as
`INSECURE=true` does where no endpoint is set; an endpoint's own scheme otherwise decides. Booleans take only `true` or
`false`, in any case, rather than taking any other value as false, since `INSECURE` chooses the transport's security. An
endpoint may not hold credentials, a query or an escaped path, which the exporters would drop, decode or trim, nor, over
gRPC, a path, which it drops; headers may not repeat a name in another case. A certificate or client key pair named must
load, as the exporters would otherwise only log it and go on with the system's roots or without the client's identity;
half a pair fails too, though the exporters would skip it for the other prefix's whole one, as it is more likely a
mistake than meant. The provider's span limits are given, not read from `OTEL_SPAN_*_LIMIT`, since the bridge bounds a
span itself and lower limits would drop the attributes it keeps first, or the links. The HTTP trace exporter is given
protobuf, which it would otherwise read from the process's `OTEL_EXPORTER_OTLP_PROTOCOL`. Enumerated values are read in
any case, as the specification asks. The exporters read the rest, such as TLS certificates and compression, from the
process's environment, which outside a test is the one `Getenv` reads. `Setup` settles two things itself:

- **Nothing configured exports nothing.** An unset `OTEL_TRACES_EXPORTER` or `OTEL_METRICS_EXPORTER` means `otlp` if
  `OTEL_EXPORTER_OTLP_ENDPOINT` or that signal's own endpoint variable is set, and `none` otherwise. The SDK's own
  default, `otlp` to `localhost:4318`, would make every coordinator try to send to a collector nobody set up.
  `OTEL_SDK_DISABLED=true` means `none` for both.
- **Mistakes fail the start.** Exporter names are checked against the lists the specs give, where the SDK's own reading
  of the environment would fall back with only a log line. The error names the variable, its value and the values taken.
  For a worker, `prometheus` is refused and `OTEL_TRACES_EXPORTER` is warned about and ignored.

The Prometheus exporter is served by the bridge on its own `http.Server` at `/metrics`, on
`OTEL_EXPORTER_PROMETHEUS_HOST:PORT`, `localhost:9464` by default, listening before the coordinator serves its API, so
that a port in use fails the start. It serves OpenMetrics when asked, which carries exemplars.

Export errors go to OpenTelemetry's error handler, which is the process's, not a `Setup`'s:
`inv.coordinator` and `inv.worker` install it once with `telemetry.HandleErrors(inv.warn)`,
so that coordinators that tests run in one process don't take over each other's warnings.
It logs each distinct message at most once a minute, with a count of those suppressed, remembering each by a digest
and logging at most 4 KiB of it, as an exporter's error may hold a collector's whole response.
A `Setup` that fails stops what it had started.
On shutdown, `Telemetry.Shutdown` flushes and closes the providers, and returns after 10 seconds whether or not they
have finished, as an exporter whose collector doesn't answer may not.
It runs after the API stops, so the last jobs' spans are in it.

### Metrics are derived from spans, gauges from state, and counters where things happen

Histograms and per-target counts are made in the bridge from the spans it converts, recorded under a context holding the
span's `SpanContext` with the job's sampled flag. The SDK's default `trace_based` exemplar filter then attaches the
exemplar only for sampled spans, as the spec says. To find those spans without parsing display names, the spans that
metrics read carry a `role` argument, whose values `tracing` names as constants:

| role | recorded by | metrics |
| --- | --- | --- |
| `target` | `sched`, on each layer's, task's and pull's span, with `kind` and `outcome` (`hit`, `miss`, `joined`, `failed`, `cancelled`, `retried`) | `vamos.cache.hits`, `.misses` (by `cause`), `.joins`, `.saved` (from `saved`), `.lost` (by `reason`, from a `lost` argument that lookup and regeneration set) |
| `wait` | the pool: a new `wait for a worker` span per try, from queuing to assignment | `vamos.attempts.wait` |
| `try` | the pool: a new span per try, from assignment to report or loss, with `platform`, `kind`, `outcome` (`succeeded`, `failed`, `lost`, `killed`) and the worker's usage | `vamos.attempts.duration`, `.cpu`, `.memory.peak` |
| the job's synthetic span | the bridge, with `outcome` (`succeeded`, `failed`, `cancelled`), cancelled when its context ended, as its targets' | `vamos.jobs.duration` by `kind` and `outcome` |

Kinds are `layer`, `task`, `pull`, `push`, `upload` and `tag`. A target span's status text, such as `cached` or
`building, shared with …`, which its `outcome` argument held before, moves to a `status` argument, so that `outcome`
means the same on every span that has one. A target span names its `attempt` when one served it, a miss's, a join's or a
failure's, but not a hit's, whose result carries only the attempt trace's digest; `lost` is one of `missing`, `corrupt`
and `unusable`, so that its metric's labels stay bounded. An attempt's cache write, which its trace can't hold, goes to
the sink with the attempt's spans, once its result is recorded, so the attempt's export holds it even if its starter
left; every job's copy of it, on the target's own track or the shared one, carries `shared` (`tracing.Shared`), which
the bridge doesn't export again, unless the attempt wasn't exported, as when its starter's trace isn't kept, when each
sampled job that holds a copy exports its own, and CLI assembly doesn't tag with the job's trace. The bridge tells
whether it exported the attempt from its bounded index of attempts, so one forgotten before a long joiner ends exports
its cache write a second time, a bounded duplicate, as a link to it may be lost. The new `wait` and `try` spans also
appear in stored traces, where the pool's retries become visible as tries.

Gauges are observable instruments whose callbacks read state under the owner's lock, once per collection: the pool's
attempts by state and platform, workers and slots by platform, and builds waiting for a platform; `sched.Jobs`'s open
jobs by kind; and the stores' sizes. Counters of events with no span of their own are incremented where the event
happens, through a `coord.Metrics` struct of instruments that `New` makes from the meter of the telemetry
`Config.Telemetry` returns, since that is set up only once the instance is read; without one, a no-op meter's.
These are refused jobs by reason (`busy`, `platform`, `version`, `generation`), expired leases, kills by reason
(`withdrawn`, `demand`, `stale`), rejected reports by reason (`stale`, `generation`), journal write failures, gaps in
event streams, which remote's watch serves, and retries. Each is created at start and so exported from zero. The worker
has its own three counters: fenced attempts, event batches the coordinator refused, and cgroups that `kill` couldn't
empty in time.

`otelhttp` reports `http.server.request.duration` labeled by `http.route`.
Since Go 1.22's `ServeMux`, `otelhttp` takes the route from `Request.Pattern`:
the pinned version, as task 1.3 checks, labels `GET /v1/jobs/{id}/events`
with `http.route` `/v1/jobs/{id}/events` and `http.request.method` `GET`, as the semantic conventions split them,
and never with the path.
The mux sets `Pattern` on the request it is handed, and `otelhttp` reads the request it handed on,
so a middleware between them that passes on a copy, as `r.WithContext` makes, would leave every request without a route.
So `otelhttp` wraps the mux directly, through a new `Server.Instrument` that `Handler` applies inside
`Server.Middleware`, rather than through `Middleware` itself;
a request that the middleware refuses, such as an unauthenticated one, isn't measured.
`otelhttp` also labels by `server.address` and `server.port`, from the request's `Host`, which a client chooses,
so a metric view keeps only the method, the route and the status code on every instrument of `otelhttp`'s scope,
the request and response body sizes' as well as the duration's, as the labels must be bounded.
Bytes moved come from a `Server.Transferred(direction, n)` hook that the blob handlers call,
so that `remote` stays free of the metric SDK.

`runtime` instrumentation from `contrib/instrumentation/runtime` adds the Go runtime's metrics on both roles.

*Alternative:* a Prometheus client registry beside OpenTelemetry. Rejected: two metric systems, and no exemplars from
derived spans.

### The stores' sizes are running counts

`cache.Directory` keeps a running count of the bytes its blobs and its records hold. One walk counts what was there
when the directory opens, in the background, and the gauge reports nothing until it has finished; each write and
removal through the directory, a put, a duplicate put, a dropped corrupt blob, a cleared store, a pruned history, is
counted as it happens, one at a time, and one that the walk hasn't reached yet is counted as written and left by the
walk, so that nothing is counted twice. `Directory.Size()` returns both and whether the walk has finished.
`cachetest.Content` checks that the size follows puts, duplicate puts, dropped corrupt blobs and clears.

*Alternative:* walk at every collection. Rejected: a store of millions of blobs, every 60 seconds.

### Attempts report their resources

After the attempt's subprocess exits and before its cgroup is removed, the worker reads `cpu.stat`'s `usage_usec` and
`memory.peak` (Linux 5.19 and later; absent before), and sends them in `AttemptReport.Usage`, a new optional field. The
pool sets them as `cpu` and `memory peak` arguments on the try's span, which the stored trace and the export both carry.
A killed attempt reports nothing, so its try has no usage. An older worker's report has none.

### TRACEPARENT travels on each request

The CLI reads `TRACEPARENT`, and `TRACESTATE` with it, as it sends its first job to a coordinator, so that a command
that sends none, such as a prune, says nothing of them, and checks the traceparent with `propagation.TraceContext`:
lower-case hex, non-zero IDs, and version `00`'s length, or a later version's fields as the W3C recommends reading them.
A malformed traceparent, or a tracestate that `trace.ParseTraceState` refuses, is warned about once and dropped, and an
empty one is taken as unset. `GraphRequest`, `PushRequest`, `PublishRequest` and `ExportRequest` gain `Parent string`
(JSON `traceparent`) and `State string` (`tracestate`), as metadata like `Workspace`. The coordinator checks them again
once it has checked the rest of the request and before it journals it, since a client may be older or another
program, and drops one it can't parse, logging it, with the job and the value, if it exports traces; it keeps the
traceparent written as version `00`, with the flags version `00` defines, so that the record holds only what a job
joins, bounded, and reads again as itself; it checks and repairs a resumed record's as the build resumes, reporting it
once if it exports traces, as one journaled by an older coordinator wasn't checked. The local executor ignores them.
`telemetry.OuterTrace` is the one check both make. `coord.Config.Telemetry` is a function of the coordinator's
instance that returns the bridge, rather than the bridge itself, since `coord.New` reads the instance from its state
and resumes jobs, which export into the bridge, before it returns; `inv.coordinator` runs `telemetry.Setup` in it, and
shuts down what it made whether or not `New` succeeds, while `New` releases the state's lock if the function fails.

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
`prometheus`, and `otlp` too if an endpoint is set, `OTEL_EXPORTER_PROMETHEUS_HOST=::`, every address, and the port, a
`containerPort` and a Service port named `metrics`. The defaults set nothing. `extraEnv` still overrides, but for the
exporters, the Prometheus host and port, which the chart sets alone with Prometheus on, and an OTLP endpoint there,
literal or from a reference, counts as one when the chart chooses the exporters.

## Risks / Trade-offs

- [A job's own spans reach the backend only when the job ends, so a long build shows only its finished attempts while it
  runs] → Attempts, which hold most spans, are exported as each ends. Exporting job spans in batches as they end is a
  follow-up if operators need it, and needs no change to the specs.
- [The SDK may not take chosen IDs for spans with a remote parent] → Task 1.2 proves it first. The `SpanStub` fallback
  is confined to `telemetry`.
- [Hashing every span's ID costs CPU on a coordinator] → One SHA-256 of under 100 bytes per span, at export only.
  Sampled-out traces make no spans. Task 2.6's benchmark measures conversion of a large attempt trace.
- [A joiner's link is made when its target's span is converted, so an attempt whose snapshot is dropped later leaves the
  link pointing at a span that never arrives] → Links are a convenience; a link is made only to an attempt whose
  snapshot was queued, so the sched hook reports an attempt before its joiners are released; a joiner cancelled while
  the attempt runs on, or one exported once the bounded index has forgotten the attempt, lacks the link. A snapshot
  abandoned at shutdown can still leave one dangling.
- [The arrow index misses producers that were evicted or exported after their user] → Links are a convenience. A user
  exported before its producer gets no link. That happens across jobs, and within one too: a cache hit's span, which
  produces the image a later attempt of the same job starts from, is in the job's snapshot, exported when the job
  ends, after that attempt's.
- [A collector that falls behind fills the bridge's queue, and a trace is cut short: the snapshots sent before the
  first one dropped, its attempts' without its job's] → Each snapshot is sent whole or not at all, the rest of a
  trace that lost one is dropped too, and the drops are reported. The queue holds sixteen traces of
  `tracing.MaxTraceSize`.
- [An attempt's span names its starter's target as its parent, which the starter's trimmed snapshot may have dropped,
  since attempts' spans aren't in it] → The attempt's spans are then a subtree of their own in the backend, whose
  parent is missing, with no mark that the trace is incomplete. The stored trace refers to the target the same way,
  and marks its truncation. Trimming drops the latest-started spans, so it takes a job of many targets for it to drop
  one that started an attempt.
- [`role` arguments and the new `wait` and `try` spans change what stored traces show] → They add spans and arguments
  and change none. The trace feature's scenarios check nesting and names they already list. The new spans nest within
  the attempt's.
- [Gauges' callbacks take the pool's and jobs' locks on every collection] → Once per export interval or scrape, reading
  counts that are kept as maps by platform, not walked.
- [The exporters pull in gRPC and the Prometheus client] → gRPC is already compiled in through buildah's dependencies.
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
