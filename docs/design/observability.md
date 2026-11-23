# Observability

This document describes how a coordinator exports what it does over OpenTelemetry: each job's trace, made from the
same spans that vamos stores as the job's trace, and how those spans become OpenTelemetry's.
The [telemetry](../../internal/telemetry/doc.go) package holds the SDK; no other package imports it but the
coordinator and the CLI, which configure it.

## What is exported, and what isn't

A coordinator exports one trace for each job it runs, a build, a push or a publication, under a span for the job,
holding the coordinator's spans of the job and the spans of the attempts it started, its workers' among them.
The client's own spans aren't exported: a client may run under a CI tracer of its own, which `TRACEPARENT` joins, and
the client is no service an operator watches. The local executor exports nothing, and neither does
`vamos coordinator --local`, which serves one.

Spans are exported as they are stored, not as they end: a job's own spans when it ends, an attempt's once its result
is recorded. So the exported trace and the stored one show the same spans, truncation included, at the cost of a
delay as long as the job.
A recorder callback that exported each span as it ended was rejected: it would need to agree with the trimming of
stored traces, and a worker's spans arrive relayed in batches anyway.

## The hook points

`sched` calls a `SpanSink` with each snapshot it stores: `Sharing` gives it an attempt's spans, with its cache write,
once its result is recorded and before any job it served is told of its end, and `RunJob` gives it a job's as the
job returns. Both are trimmed once and passed to the store and to the sink alike.
`sched` never imports OpenTelemetry; the sink is an interface the coordinator fills with the bridge.

The bridge converts a snapshot off the build's path, on a goroutine of its own, through the SDK's batch processor,
which blocks the converter rather than drop a span. When more than a bounded size of snapshots waits, the bridge drops
the next whole, and the rest of its job, and says so in the coordinator's log: a trace is then cut short, never holed.
Every span is bounded in what it holds, so that the exporters' queues are bounded too.

## IDs

IDs are derived, not drawn: a trace's from the coordinator's instance and the job, the job's span's likewise, and for
a run after the first, as a build resumed after a restart has, from the run as well, an
attempt's span's from its ID, and every other span's from the job and vamos's span ID. A job resumed after a restart
that kept the coordinator's state so goes on in the same trace, and a job that joined another's attempt links to the
attempt's span without asking its starter for it. A job that joined an outer trace, through `TRACEPARENT`, takes its
trace ID, and its job span is a child of the outer span. The SDK is given the chosen IDs through an `IDGenerator`
that reads them from the context, which a test proves the pinned SDK keeps.

The CLI writes a job's exported trace ID on its top-level spans in the `--trace` file, so a user can find a build in
the tracing backend from the file vamos wrote.

## Sampling

The coordinator reads `OTEL_TRACES_SAMPLER` itself rather than the SDK's environment, since it decides once for each
job, as the job starts, before any span exists, and keeps or drops every span of the job's trace, its attempts'
among them. Ratio samplers are a function of the trace ID, and a joined trace's flag is in the job's record, so a
resumed job decides the same way again.

## Links

A job that joined another's attempt links its target's span to the attempt's span, in the starter's trace, once the
attempt's snapshot is queued for export, which `Sharing` does before it releases the joiners. Every job's copy of
the attempt's cache write is marked shared, so that the attempt's export holds it once; a job whose starter's trace
isn't kept exports its own copy.

Where a stored trace draws an arrow, from the span that made something to one that used it, the exported span that
used it links to the one that made it, if that was exported before it, through a bounded index of what spans
produced. What the client's load made isn't exported, so a step that adds a glob's files has no link.

## Metrics

A coordinator's metrics come from three places. Histograms and the cache's counts are measured by the bridge from the
spans it is given, whether or not their trace is kept, each under the context of the span it measures, so that a
sampled one is its exemplar. Gauges are read from the pool's, the jobs' and the stores' state as each collection
asks. Counters of what has no span of its own are counted where it happens, through `coord.Metrics`. Every counter is
exported from zero, by each of the bounded values of its labels, and no label holds a target, a workspace, a job, an
attempt, a worker or a file. A platform label is a registered worker's, which the deployment bounds: workers are
trusted, as they write the cache's results. A histogram's exemplar names a span only once its snapshot is queued for
export, which a shutdown out of time may still abandon; one the bridge drops is measured without one. What is
measured from spans is what the stored trace keeps of them, so a trace trimmed to its bound may leave some out.

The metrics, as Prometheus names them; OpenTelemetry's names are the same with dots, without the unit's suffix or
`_total`, such as `vamos.attempts.duration`.

| Metric | Type | Labels | What it says |
| --- | --- | --- | --- |
| `vamos_attempts_wait_seconds` | histogram | | How long each try of an attempt waited for a worker |
| `vamos_attempts_duration_seconds` | histogram | `platform`, `kind`, `outcome` | How long each try ran on its worker, from its assignment to its report, its loss, or the kill the coordinator decided on, before the worker hears of it; `outcome` is `succeeded`, `failed`, `lost` or `killed`, as a try the coordinator's shutdown ended is, and `platform` the worker's, or `any` for work that runs no command |
| `vamos_attempts_cpu_seconds` | histogram | `platform`, `kind` | The CPU time a try's processes used, as its worker read its cgroup once its subprocess exited |
| `vamos_attempts_memory_peak_bytes` | histogram | `platform`, `kind` | The peak memory a try's processes used, where the memory controller reaches the worker's cgroups, which it must have been given |
| `vamos_jobs_duration_seconds` | histogram | `kind`, `outcome` | How long each run of a job took; `outcome` is `succeeded`, `failed` or `cancelled` |
| `vamos_cache_hits_total` | counter | `kind` | Targets whose cached result was reused |
| `vamos_cache_misses_total` | counter | `kind`, `cause` | Targets that missed, whether or not their run then succeeded, but for those that joined another's attempt and those retried, counted as their retry, by why: `forced`, `new`, `lost`, `own-inputs`, `upstream`, `dependency`, or `unknown`, as a pull's, which is counted only if it succeeds |
| `vamos_cache_joins_total` | counter | `kind` | Targets that joined another job's attempt, whether or not it then succeeded |
| `vamos_cache_saved_seconds_total` | counter | `kind` | What the runs that hits reused took, less the hits' own time |
| `vamos_cache_lost_total` | counter | `kind`, `reason` | Results found but unusable: `missing`, `corrupt` or `unusable` |
| `vamos_transfer_bytes_total` | counter | `direction` | The bytes of blobs the API accepted, `in`, and sent, `out`, as far as each read went |
| `vamos_store_size_bytes` | gauge | `store` | What the build cache holds, `content` or `records`, once its count of what it held at start is done, which reads each directory's names at once; a directory it couldn't read holds the gauge back until a later collection reads it |
| `vamos_attempts` | gauge | `state`, `platform` | Attempts `queued` for a worker, by the platform they run on, and `running`, by their worker's; `any` for work that runs no command, as its tries are labelled |
| `vamos_workers` | gauge | `platform` | Registered workers |
| `vamos_worker_slots` | gauge | `state`, `platform` | The registered workers' slots, `busy` or `free` |
| `vamos_builds_waiting` | gauge | `platform` | Builds with an attempt that runs commands waiting for a platform no registered worker runs, by the build that started it, which builds that share it aren't; work waiting for a worker that publishes isn't counted |
| `vamos_jobs_open` | gauge | `kind` | Jobs running: `build`, `push` or `publish` |
| `vamos_jobs_refused_total` | counter | `reason` | Jobs refused: `busy`, `platform`, `version` or `generation` |
| `vamos_attempts_killed_total` | counter | `reason` | Attempts taken back, queued or running: `withdrawn`, as every job that wanted it ended, `demand`, as no build demands it, or `stale`, killed at the heartbeat of an attempt no longer live, as one lost to its lease or run by an instance before a restart, but for one withdrawn, and once for a worker that heartbeats again; what the coordinator's shutdown takes back isn't counted |
| `vamos_reports_rejected_total` | counter | `reason` | Workers' reports refused: `stale` or `generation` |
| `vamos_leases_expired_total` | counter | | Tries lost as their worker's lease expired |
| `vamos_attempts_retries_total` | counter | | Attempts tried again after their worker was lost |
| `vamos_journal_failures_total` | counter | | Writes of the job journal that failed |
| `vamos_events_gaps_total` | counter | | Gaps in a job's events that watches were served, as a watch from before what is kept is |
| `http_server_request_duration_seconds` | histogram | `http_request_method`, `http_route`, `http_response_status_code` | How long the API's requests took, by route, never by path |
| `http_server_request_body_size_bytes` | histogram | as above | The size of the API's requests' bodies |
| `http_server_response_body_size_bytes` | histogram | as above | The size of the API's responses' bodies |

`kind` is a target's, `layer`, `task` or `pull`, or a try's, those and `push`, `upload` or `tag`. The Go runtime's
metrics, `go_*`, come from OpenTelemetry's runtime instrumentation, and aren't listed.

## Worker metrics

A worker exports its own counters over OTLP, or to its console, from zero; it exports no spans, which its coordinator
does, and serves no Prometheus, since a fleet of workers would be scraped one by one.

| Metric | Type | What it says |
| --- | --- | --- |
| `vamos.worker.fenced` | counter | Attempts the worker killed as their lease couldn't be renewed, whose subprocess was still running as the fence came |
| `vamos.worker.events.dropped` | counter | Batches of an attempt's output and spans the coordinator didn't take, which aren't sent again, but for those of an attempt it no longer counts, cancelled or lost |
| `vamos.worker.cgroups.stuck` | counter | Attempts' cgroups that a kill couldn't empty in time, once each |

## Configuration

`Setup` reads the standard `OTEL_*` variables through a `Getenv` the coordinator passes, so that tests can run
coordinators of different collectors at once, and builds the exporters itself rather than through `autoexport`,
which reads the process's environment. Every setting it reads is passed to the SDK, its default if unset, so that
the process's environment changes nothing `Getenv` decided; the exporters still read what `Setup` doesn't, such as
certificate files and compression, from the process's environment, which outside a test is the same one.
Nothing configured exports nothing, and a value `Setup` can't take fails the coordinator's start, naming the variable.

## Shutdown

Each run of a job has a job span of its own, in the job's one trace: a build cut short by a restart and the run
that resumes it are two spans, the resumed one's ID derived from its run, rather than two spans of one ID, which a
backend would hold only one of. OpenTelemetry's error handler, where export failures and drops are reported, is the
process's: the last coordinator to listen in a process replaces it, which only tests, running several, notice, and
what fails before a coordinator listens, as a resumed job's export may, goes to whichever handler the process has, the
SDK's own if none. A job cut short by the
coordinator's shutdown is cancelled, its context ending before the worker pool does, rather than failed on the pool.

The coordinator shuts its telemetry down after its API stops, so that the last jobs' spans are sent: the bridge
converts what waits, then every exporter stops at once, within ten seconds, so that one whose collector doesn't
answer strands no other.
