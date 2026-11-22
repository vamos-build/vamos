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
