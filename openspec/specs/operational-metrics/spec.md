# operational-metrics Specification

## Purpose

Exports metrics of a coordinator and its workers over OpenTelemetry and Prometheus, so that operators can see whether
the fleet has enough capacity, whether it is fast, whether the cache earns its keep, and what degrades quietly, without
reading logs.

## Requirements

### Requirement: Metric export is configured by the standard OTEL variables

A coordinator SHALL read its metric export configuration from the standard `OTEL_*` environment variables when it
starts. `OTEL_METRICS_EXPORTER` SHALL take `otlp`, `prometheus`, `console` or `none`, or several, comma-separated. If it
is unset, the exporter is `otlp` when `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` is set, and
`none` otherwise. With `--local` a coordinator SHALL read none of them and export nothing.

#### Scenario: A coordinator with nothing configured serves no metrics

- **GIVEN** a coordinator started with no `OTEL_*` variable set
- **WHEN** I scrape port 9464 of the coordinator's host
- **THEN** nothing answers

#### Scenario: OTLP and Prometheus at once

- **GIVEN** a coordinator started with `OTEL_METRICS_EXPORTER=otlp,prometheus` and `OTEL_EXPORTER_OTLP_ENDPOINT`
  naming an OTLP collector
- **WHEN** I build `app` on the coordinator, and the collector and a scrape each read the coordinator's metrics
- **THEN** both hold `vamos.jobs.duration`, as `vamos_jobs_duration_seconds` in the scrape

#### Scenario: An unknown metrics exporter is a mistake

- **WHEN** I start `vamos coordinator` with `OTEL_METRICS_EXPORTER=statsd`
- **THEN** it fails to start, naming `OTEL_METRICS_EXPORTER` and `statsd` and the exporters it takes

### Requirement: Prometheus is served apart from the API

With the `prometheus` exporter, a coordinator SHALL serve its metrics over HTTP at `/metrics` on its own address,
`OTEL_EXPORTER_PROMETHEUS_HOST` and `OTEL_EXPORTER_PROMETHEUS_PORT`, `localhost:9464` by default, and the API's address
SHALL serve no metrics. An address it can't listen on SHALL fail its start, naming the address.

#### Scenario: Metrics are scraped on their own port

- **GIVEN** a coordinator started with `OTEL_METRICS_EXPORTER=prometheus` and `OTEL_EXPORTER_PROMETHEUS_PORT` a free
  port
- **WHEN** I scrape `/metrics` on that port
- **THEN** it answers with the coordinator's metrics
- **AND** `/metrics` at the coordinator's `--listen` address isn't found

#### Scenario: A metrics port in use is a mistake

- **GIVEN** a port that another process listens on
- **WHEN** I start `vamos coordinator` with `OTEL_METRICS_EXPORTER=prometheus` and `OTEL_EXPORTER_PROMETHEUS_PORT`
  that port
- **THEN** it fails to start, naming the address it couldn't listen on

### Requirement: Metrics are labeled coarsely

No metric SHALL be labeled with a target, a workspace, a job, an attempt, a worker's ID or a file. Labels SHALL be
drawn from bounded sets: a platform, a kind of target or job, an outcome, a miss cause, a reason, a state, a store, a
direction, and a request's method, route and status. A platform is one the deployment offers or a registered worker
runs, which the deployment bounds, as it trusts its workers, whose registrations and relayed spans supply a try's
labels.

#### Scenario: A workspace's builds add no series

- **GIVEN** a coordinator with a worker, serving Prometheus metrics
- **AND** a workspace named `com.acme.storefront`
- **WHEN** I build `app` and then `api` on the coordinator, and scrape the metrics
- **THEN** no series names `com.acme.storefront`, `app`, `api` or a job's ID, in its name or its labels

### Requirement: Capacity metrics

A coordinator SHALL export: `vamos.attempts.wait`, a histogram of how long attempts waited for a worker;
`vamos.attempts`, attempts by platform and `state` `queued` or `running`; `vamos.workers` by platform, and
`vamos.worker.slots` by platform and `state` `busy` or `free`; `vamos.builds.waiting` for a platform; `vamos.jobs.open`;
and `vamos.jobs.refused` by `reason`.

#### Scenario: A build waiting for a platform is counted

- **GIVEN** a coordinator that offers a platform, such as `linux/arm64`, with no worker of it, serving Prometheus
  metrics
- **WHEN** a client builds `app` for that platform on the coordinator, and I scrape the metrics while it waits
- **THEN** `vamos_builds_waiting` for that `platform` is 1, and `vamos_attempts` for `state` `queued` at least 1

#### Scenario: A refused build is counted by reason

- **GIVEN** a coordinator started with `--max-builds 1`, serving Prometheus metrics, and another client's build open on
  it
- **WHEN** I build `app` on the coordinator, which is refused as busy, and scrape the metrics
- **THEN** `vamos_jobs_refused_total` for `reason` `busy` is 1

#### Scenario: Workers and their slots are counted by platform

- **GIVEN** a coordinator with a worker of capacity 2, serving Prometheus metrics
- **WHEN** I scrape the metrics while no build runs
- **THEN** `vamos_workers` for the worker's platform is 1, and `vamos_worker_slots` for `state` `free` is 2

### Requirement: Speed metrics

A coordinator SHALL export `vamos.attempts.duration`, a histogram of attempts' runs, by platform, kind and outcome;
`vamos.jobs.duration`, of jobs, by kind and outcome; and the duration of the API's requests that its middleware passes,
by method, route pattern and status, never by a path that holds an ID or digest,
and without a route for a request that matches none.

#### Scenario: A build's job and attempt are timed

- **GIVEN** a coordinator with a worker, serving Prometheus metrics
- **AND** a project whose layer `app` must run
- **WHEN** I build `app` on the coordinator, and scrape the metrics
- **THEN** `vamos_jobs_duration_seconds_count` for `kind` `build` and `outcome` `succeeded` is 1
- **AND** `vamos_attempts_duration_seconds_count` for `kind` `layer` and `outcome` `succeeded` is 1

#### Scenario: Requests are labeled by route, not by path

- **GIVEN** a coordinator with a worker, serving Prometheus metrics
- **WHEN** I build `app` on the coordinator, and scrape the metrics
- **THEN** the request durations have `http_route` values such as `/v1/jobs/{id}/events`, with the method in
  `http_request_method`, and no label holds a job's ID

### Requirement: Cache metrics

A coordinator SHALL export, by kind of target, `vamos.cache.hits`; `vamos.cache.misses` by cause, `unknown` for a miss
with none, as a pull's, even a pull whose unusable result `vamos.cache.lost` counts; `vamos.cache.joins`;
`vamos.cache.saved`, the time hits saved; `vamos.cache.lost` by reason, results found but unusable; and
`vamos.transfer.bytes` by direction, and `vamos.store.size` by store.

#### Scenario: A hit and a miss are counted, with what the hit saved

- **GIVEN** a coordinator with a worker, serving Prometheus metrics
- **AND** a project whose layer `app` must run a step that takes two seconds
- **WHEN** I build `app` on the coordinator, then build it again, and scrape the metrics
- **THEN** `vamos_cache_misses_total` for `kind` `layer` and `cause` `new` is 1, `vamos_cache_hits_total` for `kind`
  `layer` is 1, and `vamos_cache_saved_seconds_total` is at least 1

#### Scenario: A result lost to a missing blob is counted

- **GIVEN** a coordinator with a worker, serving Prometheus metrics, that has built `app`, which adds the files of the
  task `out`
- **AND** the blobs of its build cache removed, which hold `out`'s files
- **WHEN** I build `app` on the coordinator, and scrape the metrics
- **THEN** `vamos_cache_lost_total` for `kind` `task` and `reason` `missing` is 1, and `vamos_cache_misses_total` for
  `cause` `lost` is 1

#### Scenario: The store's size follows what it holds

- **GIVEN** a coordinator with a worker, serving Prometheus metrics, that has built `app`
- **WHEN** I prune the coordinator, and scrape the metrics
- **THEN** `vamos_store_size_bytes` for `store` `content` is less than it was before the prune

### Requirement: Metrics of what degrades quietly

A coordinator SHALL count `vamos.attempts.retries`, `vamos.leases.expired`, `vamos.attempts.killed` by reason,
`vamos.reports.rejected` by reason, `vamos.journal.failures` and `vamos.events.gaps`. A worker SHALL count
`vamos.worker.fenced`, `vamos.worker.events.dropped` and `vamos.worker.cgroups.stuck`. Each SHALL be exported from zero.

#### Scenario: A lost worker's attempt is counted as retried

- **GIVEN** a coordinator with two workers, serving Prometheus metrics
- **AND** a project whose layer `app` runs a step that is still running when its worker is stopped
- **WHEN** I build `app` on the coordinator, and the worker running it is stopped without warning, and the build
  succeeds on the other
- **THEN** `vamos_leases_expired_total` is 1 and `vamos_attempts_retries_total` is 1

#### Scenario: A quiet coordinator exports its counters at zero

- **GIVEN** a coordinator, serving Prometheus metrics, that has run nothing
- **WHEN** I scrape the metrics
- **THEN** `vamos_leases_expired_total`, `vamos_reports_rejected_total` and `vamos_journal_failures_total` are each 0

### Requirement: Histograms carry exemplars of their spans

With the default exemplar filter, `trace_based`, a histogram's exemplars SHALL be drawn only from spans of sampled
traces queued for export, each naming the trace ID and span ID its span was exported under, so that an outlier leads to
its trace. A Prometheus scrape in the plain text format SHALL show no exemplars.

#### Scenario: A slow attempt leads to its trace

- **GIVEN** a coordinator with a worker, exporting traces to an OTLP collector and serving Prometheus metrics
- **WHEN** I build `app` on the coordinator, and scrape the metrics as OpenMetrics
- **THEN** `vamos_attempts_duration_seconds` has an exemplar whose `trace_id` and `span_id` name the span of `app`'s
  attempt that the collector received

#### Scenario: A sampled-out span leaves no exemplar

- **GIVEN** a coordinator with a worker, serving Prometheus metrics and sampling no trace
- **WHEN** I build `app` on the coordinator, and scrape the metrics as OpenMetrics
- **THEN** `vamos_attempts_duration_seconds` counts the attempt, and has no exemplar

### Requirement: Attempts report the resources they used

A worker SHALL read an attempt's cgroup as the attempt ends and record the CPU time and peak memory it used on the
attempt's try span, which the stored trace shows; the coordinator SHALL export `vamos.attempts.cpu` and
`vamos.attempts.memory.peak`, histograms by platform and kind. What the cgroup can't tell SHALL be left out, as SHALL
all of it for an attempt killed or cancelled before its cgroup is read.

#### Scenario: A busy step's CPU time is reported

- **GIVEN** a coordinator with a worker, serving Prometheus metrics
- **AND** a project whose layer `app` runs a step that keeps a CPU busy for two seconds
- **WHEN** I build `app` on the coordinator with `--trace trace.json`, and scrape the metrics
- **THEN** in `trace.json`, the span of `app`'s try says it used at least 0.1 seconds of CPU, and a peak of memory
  where the worker's cgroups count memory
- **AND** `vamos_attempts_cpu_seconds_count` for `kind` `layer` is 1

### Requirement: Workers export their own metrics over OTLP

A worker SHALL export its metrics as `OTEL_METRICS_EXPORTER` and the OTLP variables say, and take one of `otlp` or
`console`, or `none`, `none` beside another being ignored; `prometheus` SHALL fail its start, saying workers export over
OTLP. A worker SHALL export no traces. If `OTEL_TRACES_EXPORTER` names an exporter other than `none`, or is a mistake, a
worker SHALL warn that it is ignored, since its coordinator exports its spans, unless `OTEL_SDK_DISABLED` is true.

#### Scenario: A worker's counters reach the collector

- **GIVEN** a coordinator with a worker, the worker started with `OTEL_EXPORTER_OTLP_ENDPOINT` naming an OTLP collector
- **WHEN** the worker has run for an export interval
- **THEN** the collector received `vamos.worker.fenced` from the worker, at 0

#### Scenario: A worker refuses Prometheus

- **WHEN** I start `vamos worker` with `OTEL_METRICS_EXPORTER=prometheus`
- **THEN** it fails to start, naming `OTEL_METRICS_EXPORTER` and saying that workers export metrics over OTLP

### Requirement: Telemetry is flushed on shutdown

A coordinator or worker that shuts down SHALL export what it holds of traces and metrics before it exits, for at most
10 seconds, and then exit whether or not the export finished.

#### Scenario: A coordinator flushes on interrupt

- **GIVEN** a coordinator with a worker, exporting traces to an OTLP collector with `OTEL_BSP_SCHEDULE_DELAY` one hour
- **WHEN** I build `app` on the coordinator, and interrupt the coordinator
- **THEN** the collector received the build's trace before the coordinator exited

### Requirement: The Helm chart exposes telemetry

The chart SHALL pass, as `OTEL_*` variables, the OTLP endpoint, protocol and headers it is given to both roles, and the
trace sampler to the coordinator alone. With its Prometheus switch on, it SHALL set the coordinator's metrics exporters,
adding `otlp` if a shared or metrics OTLP endpoint is set, in the values or its extra environment, which can't then set
the exporters, and expose port `metrics` on its Service. Its defaults SHALL set no `OTEL_*` variable and expose no
metrics port.

#### Scenario: The chart's defaults export nothing

- **WHEN** I render the chart with its default values
- **THEN** neither the coordinator nor a worker sets an `OTEL_*` variable, and the coordinator's Service has no port
  named `metrics`

#### Scenario: Prometheus on in the chart

- **WHEN** I render the chart with `telemetry.prometheus.enabled=true`
- **THEN** the coordinator sets `OTEL_METRICS_EXPORTER` to include `prometheus`, `OTEL_EXPORTER_PROMETHEUS_HOST` to
  `::`, every address, and its Service exposes the metrics port, named `metrics`
