# Proposal

## Why

Whoever runs a coordinator and its workers can't see how the fleet is doing: there are no metrics, and what goes
wrong quietly (lost leases, retried attempts, fenced attempts, dropped event batches, failed journal writes,
lost cached results) is written to a log and forgotten. Once every build has a trace (`persistent-traces`),
the same spans can feed the tools operators already run, if vamos exports them in a form those tools read.

## What Changes

- A coordinator exports telemetry through OpenTelemetry, configured with the standard `OTEL_*` environment variables:
  OTLP, Prometheus, both, or neither. What a coordinator does with nothing configured is for the design to settle,
  since exporting to an endpoint nobody set up should not be a surprise. Prometheus is served on its own port,
  apart from the API.
- **Traces**: the coordinator alone exports traces, converting every completed vamos span, its workers' included,
  to an OpenTelemetry span. Span IDs are derived deterministically from the coordinator's instance, the job and
  vamos's span IDs; so is the trace ID, unless the build joined an outer trace, whose trace ID it then takes.
  The coordinator keeps that outer context with the job, so a trace exported and the same trace retrieved with
  `--trace` agree, and a build can print the trace ID it actually used. Arrows between spans become span links,
  and so does each other build's use of a shared attempt.
  Sampling follows `OTEL_TRACES_SAMPLER`, keeping or dropping a build's trace whole.
- **Joining an outer trace**: a client that finds a W3C `TRACEPARENT` in its environment sends it with its request,
  and the build's trace becomes a child of that trace, as a CI pipeline's, rather than starting its own.
  A malformed `TRACEPARENT` is warned about and ignored.
- **Metrics are derived from spans** as the coordinator converts them, so that a histogram's exemplars name the
  very span that was measured. Labels are coarse, such as platform, kind, outcome and miss cause; no metric is
  labeled with a target or a workspace, which spans carry instead. The metrics answer:
  - whether there is enough capacity: attempts' wait for a worker, attempts queued and running, workers and slots
    by platform, builds waiting for a platform, open and refused jobs;
  - whether it is fast: attempts' and jobs' durations, and the API's requests;
  - whether the cache earns its keep: hits, misses by cause and joins by kind of target, the time hits saved,
    results lost and why, bytes moved and the stores' size;
  - whether something degrades quietly: retries, expired leases, kills, rejected reports, journal write failures,
    gaps in event streams, and on workers, fenced attempts, dropped event batches and cgroups that wouldn't empty.
- **Attempt resource usage**: a worker reads its attempt's cgroup as the attempt ends and adds the CPU time and peak
  memory used to the attempt's span; the coordinator derives histograms of both.
- Workers export their own few metrics over OTLP; a deployment that only scrapes Prometheus gets the coordinator's.
- Spans carry the workspace name, when there is one, the target, and a miss's cause and changed files.
- The Helm chart exposes the configuration and the Prometheus port.

## Capabilities

### New Capabilities

- `trace-export`: exporting builds' traces over OpenTelemetry, their IDs, links and sampling, and joining an outer
  trace through `TRACEPARENT`.
- `operational-metrics`: the metrics a coordinator and its workers export, how they're configured and exposed,
  what they're labeled with and what is kept out of labels, exemplars, and attempts' resource usage.

### Modified Capabilities

None.

## Impact

- New direct dependencies: the OpenTelemetry SDK and metric SDK, OTLP, console and Prometheus exporters,
  and runtime instrumentation; `otelhttp` and gRPC are already compiled in.
- A new package that bridges `internal/tracing` spans to OpenTelemetry, kept out of `internal/tracing`, which still
  imports nothing; `TestImportRules` places it.
- `internal/coord`: gauges observed from its state; the bridge applied to spans it receives.
- `internal/remote`: `otelhttp` around the mux, through a `Server.Instrument` inside `Server.Middleware`;
  `internal/worker`: cgroup usage at an attempt's end, and its counters;
  `internal/api`: the outer trace context on requests; `internal/cli`: reading `TRACEPARENT`.
- `remote/helm/vamos`: values for the `OTEL_*` variables and the metrics port.
- Risks to settle early:
  - that the Go SDK can export a finished span with chosen IDs and timestamps (a custom `IDGenerator` with
    `WithTimestamp`), on which exemplars depend;
  - that the pinned `otelhttp` labels requests by the `ServeMux` pattern rather than the path;
  - keeping the stores' sizes as running counts, rather than walking a directory at every collection.
- Documentation: a new `docs/design/observability.md`, the remote environment's knowledge base and the chart's
  values, and the doc.go of each package above.
- Depends on `persistent-traces`, whose spans and miss causes it exports, and on `workspace-name`.
