# Tasks

## 1. Dependencies and early risks

- [ ] 1.1 Add the OpenTelemetry SDK and metric SDK, `contrib/exporters/autoexport`, the OTLP trace and metric exporters,
  the Prometheus exporter and `contrib/instrumentation/runtime`, at releases at least two weeks old and matching the
  indirect `otel` v1.46.0 line where possible. Run `go mod tidy`, record the release binary's size before and after in
  the commit message, and verify that `scripts/check` passes and `govulncheck` reports nothing reachable
- [x] 1.2 Create `internal/telemetry` with its `doc.go` from the documentation template, and a test proving the design's
  span path against the pinned SDK: a span started under a remote parent with an `IDGenerator` that takes its IDs from
  the context, and `WithTimestamp` start and end, is exported with exactly those IDs, parent and times, to
  `tracetest.InMemoryExporter`. If it isn't, switch the design to the `tracetest.SpanStub` fallback in `design.md`
  before going on. Verify with `go test ./internal/telemetry -run TestChosenIDs`
- [x] 1.3 Add a test in `internal/remote` that wraps a `Server` in the pinned `otelhttp` handler and checks, with an
  `sdkmetric.ManualReader`, that `http.server.request.duration` is labeled with the `ServeMux` pattern, such as
  `http.route` `/v1/jobs/{id}/events` with `http.request.method` `GET`, and never with a job's ID.
  If it isn't, add `WithMetricAttributesFn` setting `http.route` from `Request.Pattern`, and verify that the test passes
- [x] 1.4 Add `telemetry` to `TestImportRules` with `internal: ["tracing"]` and Starlark, buildah and the container
  store forbidden, and forbid `go.opentelemetry.io/...` and `.../telemetry` to `sched`, `api` and `tracing`,
  and `go.opentelemetry.io/otel/sdk/...` to `remote`.
  Verify with `go test ./internal -run TestImportRules`

## 2. The telemetry package: configuration, sampling, IDs and conversion

- [ ] 2.1 Implement `telemetry.Setup(role, getenv)`: the unset-means-none defaults, `OTEL_SDK_DISABLED`, the validation
  of `OTEL_TRACES_EXPORTER` and `OTEL_METRICS_EXPORTER` against each role's list (a worker refuses `prometheus` and
  warns about `OTEL_TRACES_EXPORTER`), the resource attributes, and the returned shutdown that flushes within 10
  seconds. Verify with a table test of environments and the errors each gives, each naming the variable, its value and
  what it takes
- [ ] 2.2 Implement sampler parsing of `OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG` (`always_on`, `always_off`,
  `traceidratio`, and `parentbased_` with each), and the per-job decision from the trace ID and outer parent. Verify with
  a table test that includes ratios 0, 0.5 and 1, an unknown sampler, a ratio outside [0, 1], and that the same job
  decides the same way twice
- [ ] 2.3 Implement the ID derivations for traces, jobs, attempts and spans, with the zero-ID guard and the parent
  rules: a job's top-level spans under the job span, an attempt's top-level spans under the attempt span, and the
  attempt span under the starter's target. Verify with tests that IDs are stable across calls, differ across instances
  and jobs, and take the outer trace ID when there is one
- [ ] 2.4 Implement `Bridge.Job`, which returns the job's context carrying `tracing.Exported`, and the conversion of a
  snapshot: names, times, instants as zero-length spans, the `error` argument as an `Error` status, the attributes table
  in `design.md`, the workspace on every span, and no spans at all for a dropped trace. Verify with tests against
  `tracetest.InMemoryExporter`
- [ ] 2.5 Implement links: joins through the attempt span's `tracing.Exported`, and arrows through the bounded `Produces`
  index of 65,536 entries, with producers in the same snapshot indexed first and producers of dropped traces never
  indexed. Verify with tests for a join, an arrow within a snapshot, one across snapshots, one whose producer is
  unknown, and the index's eviction
- [ ] 2.6 Add `BenchmarkConvertAttempt`, which converts an attempt trace at `tracing.MaxTraceSize` with export on and
  sampled, reporting allocations. Verify that it runs under `go test -bench ConvertAttempt ./internal/telemetry`, and
  note the result in the commit
- [ ] 2.7 Serve the Prometheus exporter on its own `http.Server` at `/metrics`, listening during `Setup` so that a port
  in use fails it, with OpenMetrics negotiation. Install the rate-limited `otel.SetErrorHandler`. Verify with tests: a
  scrape answers, a port in use fails naming the address, and a repeated error is logged once a minute with a count
  of those suppressed
- [ ] 2.8 Write `internal/telemetry/doc.go`'s invariants, lifecycle and changes sections, and verify with `go doc
  ./internal/telemetry` that they render

## 3. Hooks in tracing, sched and api

- [ ] 3.1 Add `tracing.Exported`, `WithExported` and `ExportedOf`, and the `role` argument's constants (`target`, `wait`,
  `try`), to `tracing` without imports. Update `internal/tracing/doc.go`, and verify with unit tests and
  `TestImportRules`
- [ ] 3.2 Add `sched.SpanSink` and call it from `Sharing.attempt` (`AttemptStarted` as the attempt starts, `Attempt`
  beside `saveTrace`) and from `RunJob` (`Job` beside `JobTrace.Save`) with the same trimmed spans that are stored. Add
  `AttemptTrace.Exported`. Verify with `sched` tests using a fake sink: its spans equal the stored document's, one cut
  short by `LimitTraces` included, an attempt whose starter left is still passed under the starter's context, and a joiner's `Traced` gets the attempt's
  `Exported`
- [ ] 3.3 Have the scheduler set `role` `target`, `kind`, `outcome`, `target` (`graph.Node.Target`), `attempt`, and
  `lost` with its reason, on layers', tasks' and pulls' spans. Verify with `scheduler_trace_test.go` cases for a hit, a
  miss, a join, a lost result and a failure, and check that the `build-traces` scenarios still pass with
  `go test ./internal/features -run TestTrace_`
- [ ] 3.4 Add `api.JobTrace.Exported`, set by `JobTrace.Save` from `tracing.ExportedOf`, and have CLI assembly write
  `otel trace` and `otel sampled` on each job's top-level spans when it is set. Verify with `cli` trace assembly tests
  for a manifest with and without it, and an older manifest, and update `internal/api/doc.go` and `internal/sched/doc.go`

## 4. TRACEPARENT

- [ ] 4.1 Add `Parent` (`traceparent`) and `State` (`tracestate`) to `GraphRequest`, `PushRequest`, `PublishRequest` and
  `ExportRequest`. Verify that they round-trip through `apitest.Contract` over `remote`, and that the coordinator's
  journal keeps them across a restart, with a case in `restart_test.go`
- [ ] 4.2 Have the CLI read `TRACEPARENT` and `TRACESTATE` when it builds on a coordinator, validate them with
  `propagation.TraceContext`, warn once about a malformed one and drop it, take an empty one as unset, and send them on
  every job of the command. Verify with `cli` tests for valid, malformed, all-zero and empty values, and for a push
  sending them on both its jobs
- [ ] 4.3 Have the coordinator check a request's `Parent` again, log and ignore one it can't parse, and pass it to
  `Bridge.Job`. Verify with a `coord` test of a malformed parent from a raw request
- [ ] 4.4 Document `TRACEPARENT` in the CLI reference's environment section and in the user docs on building on a
  coordinator, and verify the docs' link check passes

## 5. Exporting a coordinator's traces

- [ ] 5.1 Add `coord.Config.Telemetry` (a bridge, or nil for none), wrap each job's context with `Bridge.Job` in
  `Coordinator.traced`, give `Sharing` the bridge as its `SpanSink`, and have `inv.coordinator` call `telemetry.Setup`
  and shut it down after the API stops, but not with `--local`. Verify with `coord/trace_test.go` cases with fake
  workers against `tracetest.InMemoryExporter`: one trace per job, worker spans within it, a shared attempt exported
  once with the joiner's link, and a resumed job in the same trace after a restart
- [ ] 5.2 Add `internal/features/steps_telemetry_test.go`: `GivenAnOTLPCollector`, an in-process OTLP/HTTP receiver that
  decodes `go.opentelemetry.io/proto/otlp`, a coordinator option for its environment, passed to `telemetry.Setup` as
  its `getenv` rather than set in the process's, so that rules using it can run in parallel, `GivenTRACEPARENT`, which
  sets the process's environment through `GivenTheEnvironmentSets`, and Then steps for traces, spans, parents, links
  and attributes. List the file in `.agents/kb/testing.md`, and verify that it
  compiles with `go vet ./internal/features`
- [ ] 5.3 Add `internal/features/trace_export_test.go` with a rule for each `trace-export` requirement and a scenario for
  each of its scenarios, with doc comments in the words of the reference pages. Rules call `parallelRule(t)`, but for
  those that set the process's environment, as `GivenTRACEPARENT` does, or rely on a default port, the collector's
  4318 or the scrape's 9464, which run alone. The joined attempt's scenario holds the step with `GivenABarrier` until
  `GivenTheBarrierOpensOnceTheOtherClientIsJoined`, rather than sleeping. Verify with
  `go test ./internal/features -run TestTraceExport_` as root with buildah, and that the start-up mistakes' scenarios
  run under `-short`
- [ ] 5.4 Write `docs/design/observability.md`: the bridge, the hook points, ID derivation, sampling, links, and why the
  client isn't exported. Link it from `docs/design/README.md` and `remote-execution.md`, add the environment variables
  to `docs/user/reference/cli/coordinator.md`, update `internal/coord/doc.go`, and add the route to `AGENTS.md`. Verify
  the link check and `go doc ./internal/coord`

## 6. Coordinator metrics

- [ ] 6.1 Have the pool record a `wait for a worker` span (`role` `wait`) and a `try` span (`role` `try`, with
  `platform`, `kind` and `outcome`) per try, replacing the `retry N` argument. Verify with `coord` tests that a retried
  attempt's stored trace shows both tries, and that `go test ./internal/features -run TestTrace_` still passes
- [ ] 6.2 Derive `vamos.attempts.wait`, `.duration`, `.cpu`, `.memory.peak`, `vamos.jobs.duration` and the
  `vamos.cache.*` counters from converted spans in the bridge, recorded under the span's `SpanContext` so that sampled
  spans carry exemplars. Verify with `telemetry` tests on an `sdkmetric.ManualReader`: values, labels, an exemplar for a
  sampled span and none for a dropped one
- [ ] 6.3 Add `coord.Metrics`, created at start so that every counter is exported from zero, and increment it where jobs
  are refused, leases expire, attempts are killed or retried, reports are rejected, the journal fails to write and event
  streams gap. Register the gauges' callbacks for attempts, workers, slots, waiting builds and open jobs. Verify with
  `coord` tests on a `ManualReader` for each counter's site and each gauge
- [ ] 6.4 Keep `cache.Directory`'s content and record sizes as running counts after a background walk at open, expose
  them with `Size()`, and observe them as `vamos.store.size`. Verify with `cachetest.Content` cases for puts, duplicate
  puts, removals of corrupt blobs and clears, and a `Directory` test that the walk counts what was there before
- [ ] 6.5 Wrap the coordinator's API's mux in `otelhttp` through `Server.Instrument`, inside `Server.Middleware`,
  with a view that keeps only the method, route and status code on each of `otelhttp`'s instruments,
  and add `Server.Transferred` for `vamos.transfer.bytes`. Move 1.3's test onto `Server.Instrument`, and verify with
  `remote` tests that the route survives a middleware that passes on `r.WithContext`, that requests with other `Host`s
  and ports add no series to any HTTP metric, that a 200 and a 404 on one route are separate series, and the bytes
  counted for puts and reads
- [ ] 6.6 Add Prometheus scrape steps to `steps_telemetry_test.go`, plain and OpenMetrics, and
  `internal/features/metrics_test.go` with a rule for each coordinator requirement of `operational-metrics` and its
  scenarios, parallel or alone as in 5.3. The lost worker's scenario keeps its step running with `GivenABarrier`.
  Verify with `go test ./internal/features -run TestMetrics_`
- [ ] 6.7 Document every metric, its unit, labels and meaning in `docs/design/observability.md`, and the Prometheus
  variables in `coordinator.md`. Verify that the doc lists exactly the instruments a test scrape shows, with a Go test
  that compares the two

## 7. Workers

- [ ] 7.1 Read `cpu.stat`'s `usage_usec` and `memory.peak` after an attempt's subprocess exits and before its cgroup is
  removed, send them in `AttemptReport.Usage`, and have the pool set `cpu` and `memory peak` on the try's span. Verify
  with a `worker_test.go` case whose step burns CPU, its worker started with `testenv.CgroupRoot(t)` as its
  `CgroupRoot`, and a `coord` test that an older report without usage leaves them
  out
- [ ] 7.2 Have `inv.worker` call `telemetry.Setup` for the worker role, and count fenced attempts, event batches the
  coordinator refused and cgroups that `kill` couldn't empty. Verify with `worker` tests on a `ManualReader`, and the
  worker scenarios of `metrics_test.go`
- [ ] 7.3 Update `internal/worker/doc.go` and `docs/user/reference/cli/worker.md` with the usage report and the
  variables a worker reads, and verify `go doc ./internal/worker` and the link check

## 8. Helm chart

- [ ] 8.1 Add the `telemetry` values, the coordinator's and workers' `OTEL_*` environment from them, the headers Secret,
  and with Prometheus on, the coordinator's `containerPort`, Service port named `metrics` and optional scrape
  annotations. Verify with `helm lint --strict remote/helm/vamos` and `helm template ... | kubeconform -strict -summary`
  for the defaults, an OTLP endpoint, and Prometheus on, and check that the defaults render no `OTEL_` variable
- [ ] 8.2 Run the chart's Kubernetes smoke checks on kind with Prometheus on, scrape the coordinator's `metrics` port
  through the Service after a build, and add that scrape to `.agents/kb/smoke-checks.md`. Document the values in
  `values.yaml`'s comments and `.agents/kb/remote-environment.md`

## 9. Integration

- [ ] 9.1 Run `scripts/check full` as root in the devcontainer and verify that it passes, with the new features'
  scenarios running rather than skipping (`VAMOS_REQUIRE_INTEGRATION=1`)
- [ ] 9.2 Run the compose environment in `remote/compose.yaml` with an OpenTelemetry Collector added for the check
  only, build an example project with `TRACEPARENT` set, and verify that the collector logs the job's span under the
  given parent and the coordinator's metrics, including an exemplar
- [ ] 9.3 Verify that `openspec validate telemetry-export --strict` passes

## Workflow follow-up

- Archive the change once `persistent-traces` is archived and review is done, then verify that
  `openspec/specs/trace-export` and `openspec/specs/operational-metrics` exist.
