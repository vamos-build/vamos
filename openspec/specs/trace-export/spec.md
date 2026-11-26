# trace-export Specification

## Purpose

Exports the traces of the builds a coordinator runs over OpenTelemetry, to the tools operators already run, and lets a
build join a trace that started outside vamos, such as a CI pipeline's, so that a slow or failing build can be followed
from the pipeline into its targets and steps.

## Requirements

### Requirement: A coordinator is configured by the standard OTEL variables

A coordinator SHALL read its trace export configuration from the standard `OTEL_*` environment variables when it starts.
`OTEL_TRACES_EXPORTER` SHALL take `otlp`, `console` or `none`, or several, comma-separated. If it is unset, the exporter
is `otlp` when `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is set, and `none` otherwise.
`OTEL_SDK_DISABLED=true` SHALL turn off every export. With `--local` a coordinator SHALL read none of them and export
nothing.

#### Scenario: A coordinator with nothing configured exports nothing

- **GIVEN** a coordinator with a worker, started with no `OTEL_*` variable set, and an OTLP collector at the default
  endpoint
- **WHEN** I build `app` on the coordinator
- **THEN** it succeeds, and the collector received no span

#### Scenario: An OTLP endpoint alone turns on trace export

- **GIVEN** a coordinator with a worker, started with `OTEL_EXPORTER_OTLP_ENDPOINT` naming an OTLP collector
- **WHEN** I build `app` on the coordinator
- **THEN** the collector received a trace that shows `app` and its steps

#### Scenario: OTEL_SDK_DISABLED turns export off

- **GIVEN** a coordinator started with `OTEL_SDK_DISABLED=true` and `OTEL_EXPORTER_OTLP_ENDPOINT` naming an OTLP
  collector
- **WHEN** I build `app` on the coordinator
- **THEN** the collector received no span

#### Scenario: An unknown exporter is a mistake

- **WHEN** I start `vamos coordinator` with `OTEL_TRACES_EXPORTER=zipkin`
- **THEN** it fails to start, naming `OTEL_TRACES_EXPORTER` and `zipkin` and the exporters it takes

### Requirement: Export never changes a build

A build SHALL end as it would without export, whether the exporter's endpoint is reachable or not. A coordinator SHALL
report an export that fails in its log, and no client SHALL be told of it.
A coordinator whose exporter falls behind MAY cut a trace short, sending its spans up to a point and none after,
and SHALL then say so in its log.

#### Scenario: An unreachable collector fails nothing

- **GIVEN** a coordinator with a worker, started with `OTEL_EXPORTER_OTLP_ENDPOINT` naming an address where nothing
  listens
- **WHEN** I build `app` on the coordinator
- **THEN** it succeeds, and printed no warning about export

### Requirement: Each job is one trace

Each job a coordinator runs, a build, a push or a publication, SHALL be exported as one trace, under a span
for the job that holds its other spans, unless it joined an outer trace; a build resumed after a restart has a span of
its own for each run, in the same trace. The trace SHALL hold the coordinator's spans of
the job, and the spans of the attempts it started, its workers' among them. The client's own spans SHALL NOT be exported.

#### Scenario: A build's trace holds its target and its worker's steps

- **GIVEN** a coordinator with a worker, exporting traces to an OTLP collector
- **AND** a project whose layer `app` must run a step `echo hello`
- **WHEN** I build `app` on the coordinator
- **THEN** the collector received one trace for the build, whose root span is the job's
- **AND** in it, the span of `layer "app"` holds the span of the step `echo hello`, which the worker recorded

#### Scenario: A push is a build's trace and a push's

- **GIVEN** a coordinator with a worker, exporting traces to an OTLP collector, and a registry
- **WHEN** I push `app` to the registry on the coordinator
- **THEN** the collector received two traces, one holding `app`'s build and one holding the copy of its image to the
  registry

### Requirement: Exported spans keep vamos's timing and nesting

Each exported span SHALL start and end when the vamos span did, on the coordinator's clock, a worker's spans shifted
onto it as stored traces are, and SHALL have as its parent the exported span of the vamos span it was part of if that
span is in the same snapshot, and otherwise the span of its job or attempt. A span of an attempt's trace that was cut
short SHALL be absent, as it is from the stored trace.

#### Scenario: A step's exported span lies within its target's

- **GIVEN** a coordinator with a worker, exporting traces to an OTLP collector
- **AND** a project whose layer `app` must run a step that sleeps one second
- **WHEN** I build `app` on the coordinator
- **THEN** the step's exported span lasts at least one second and lies within the span of `layer "app"`, its ancestor

### Requirement: Exported IDs are derived, and the trace file names them

A job's trace ID, unless it joined an outer trace, and its spans' IDs SHALL be derived from the coordinator's instance,
the job and vamos's span IDs, so that a job resumed after a restart that kept the coordinator's state goes on in the
same trace. Where the coordinator exports traces, the `--trace` and `vamos trace` files SHALL name, on each job's
top-level spans, the trace ID the job was given and whether it was sampled, but for its copies of an attempt's cache
write.

#### Scenario: The trace file names the exported trace

- **GIVEN** a coordinator with a worker, exporting traces to an OTLP collector
- **WHEN** I build `app` on the coordinator with `--trace trace.json`
- **THEN** the top-level spans of the build's job in `trace.json` name the trace ID of the trace that the collector
  received

#### Scenario: A job that wasn't exported names no trace ID

- **GIVEN** a coordinator with a worker, with no `OTEL_*` variable set
- **WHEN** I build `app` on the coordinator with `--trace trace.json`
- **THEN** no span in `trace.json` names an OpenTelemetry trace ID

### Requirement: A shared attempt is exported once and linked by its joiners

An attempt's spans SHALL be exported once, in the trace of the job that started it, under one span for the attempt,
even if that job left the attempt running. Each job that joined the attempt SHALL link its target's span to that
attempt span.

#### Scenario: A joined attempt is linked, not repeated

- **GIVEN** a coordinator with a worker, exporting traces to an OTLP collector
- **AND** two builds of the same `app` on it that share one attempt
- **WHEN** both have finished
- **THEN** the steps of `app`'s attempt are in the trace of the build that started it alone
- **AND** in the trace of the build that joined it, the span of `layer "app"` links to the attempt's span in the other
  trace

### Requirement: A shared attempt's export may fall short when its starter's isn't kept or it is forgotten

A joiner MAY lack its link if its spans are exported before the attempt's are handed to export, as a cancelled joiner's
may be, or if the coordinator has forgotten the attempt, of a bounded number, or dropped its snapshot. If the starter's
trace isn't kept, or the attempt is forgotten or dropped, each sampled job it served whose snapshot reaches export SHALL
export the copy of its cache write its own trace holds, if any; one that left the attempt, or whose trace was cut short,
MAY hold none.

#### Scenario: A joiner of an attempt whose starter's trace isn't kept keeps its cache write

- **GIVEN** a coordinator with a worker, exporting traces to an OTLP collector
- **AND** a build of `app` under an unsampled `TRACEPARENT`, and another of `app`, sampled, that joins its attempt
- **WHEN** both have finished
- **THEN** the collector received no span of the first build
- **AND** the trace of the second build holds the attempt's cache write

### Requirement: Arrows become span links

Where a stored trace draws an arrow from a span that made something to one that used it, the exported span that used it
SHALL link to the exported span that made it, if that span is in the same snapshot, or was exported before it and the
coordinator, of a bounded number, still remembers what it made. A span that used what a cache hit of its own job made,
exported with the job's snapshot after the attempt that used it, MAY have no link.

#### Scenario: A layer's start links to the commit of its base

- **GIVEN** a coordinator with a worker, exporting traces to an OTLP collector
- **AND** a project whose layer `app` is built on its layer `base`, both of which must run
- **WHEN** I build `app` on the coordinator
- **THEN** in the build's trace, the span that starts `app` from its base links to the span that committed `base`

#### Scenario: An arrow from the client has no link

- **GIVEN** a coordinator with a worker, exporting traces to an OTLP collector
- **AND** a project whose layer `app` adds a source and must run
- **WHEN** I build `app` on the coordinator
- **THEN** the exported span of the step that adds the source has no link, since the client's load that made its files
  isn't exported

### Requirement: Sampling keeps or drops a trace whole

A coordinator SHALL sample as `OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG` say, `parentbased_always_on` if unset,
and SHALL decide for each job as it starts, keeping or dropping every span of its trace, its attempts' spans among
them. A job resumed after a restart SHALL be decided again as it resumes, the same way if the sampler's settings are
unchanged. An unknown sampler, or an argument it can't take, SHALL fail the coordinator's start, naming the variable.

#### Scenario: A ratio of zero drops every trace

- **GIVEN** a coordinator with a worker, exporting traces to an OTLP collector, with
  `OTEL_TRACES_SAMPLER=traceidratio` and `OTEL_TRACES_SAMPLER_ARG=0`
- **WHEN** I build `app` on the coordinator
- **THEN** the collector received no span

#### Scenario: A sampled-out trace is dropped whole

- **GIVEN** a coordinator with a worker, exporting traces to an OTLP collector, with
  `OTEL_TRACES_SAMPLER=traceidratio` and `OTEL_TRACES_SAMPLER_ARG=0.5`
- **WHEN** I build 20 different targets on the coordinator, one build each
- **THEN** each build's trace was received whole, with its job's span, its targets' and its steps', or not at all

#### Scenario: An unknown sampler is a mistake

- **WHEN** I start `vamos coordinator` with `OTEL_TRACES_SAMPLER=sometimes`
- **THEN** it fails to start, naming `OTEL_TRACES_SAMPLER` and `sometimes`

### Requirement: A build joins an outer trace through TRACEPARENT

A client building on a coordinator, with a W3C `traceparent` in `TRACEPARENT`, SHALL send it with each job of the
command, and each job's span SHALL then be a child of that span, in its trace, sampled as its flags say under the
default sampler. The coordinator SHALL keep it with the job, so that a resumed job stays in the outer trace.

#### Scenario: A build in CI is part of the pipeline's trace

- **GIVEN** a coordinator with a worker, exporting traces to an OTLP collector
- **AND** `TRACEPARENT` set to `00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01`
- **WHEN** I build `app` on the coordinator
- **THEN** the collector received the build's job span in trace `4bf92f3577b34da6a3ce929d0e0e4736`, as a child of span
  `00f067aa0ba902b7`

#### Scenario: Each job of a command joins the outer trace

- **GIVEN** a coordinator with a worker, exporting traces to an OTLP collector, and a registry
- **AND** `TRACEPARENT` set to `00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01`
- **WHEN** I push `app` to the registry on the coordinator
- **THEN** the build's job span and the push's are both children of span `00f067aa0ba902b7` in trace
  `4bf92f3577b34da6a3ce929d0e0e4736`

#### Scenario: An outer trace that isn't sampled isn't exported

- **GIVEN** a coordinator with a worker, exporting traces to an OTLP collector
- **AND** `TRACEPARENT` set to `00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00`
- **WHEN** I build `app` on the coordinator with `--trace trace.json`
- **THEN** the collector received no span, and `trace.json` names trace `4bf92f3577b34da6a3ce929d0e0e4736` as not
  sampled

#### Scenario: TRACEPARENT without a coordinator is unused

- **GIVEN** `TRACEPARENT` set to `00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01`
- **WHEN** I build `app` without a coordinator
- **THEN** it succeeds, and prints no warning

### Requirement: A malformed TRACEPARENT is warned about and ignored

A client building on a coordinator whose `TRACEPARENT` is not a valid W3C `traceparent` SHALL warn once, naming the
variable and its value, and build as if it were unset. An empty `TRACEPARENT` SHALL be taken as unset, without warning.
A `TRACESTATE` that isn't a W3C tracestate SHALL be warned about likewise, and the build SHALL join the outer trace
without it.

#### Scenario: A malformed TRACEPARENT starts a trace of the build's own

- **GIVEN** a coordinator with a worker, exporting traces to an OTLP collector
- **AND** `TRACEPARENT` set to `00-not-a-trace`
- **WHEN** I build `app` on the coordinator
- **THEN** it succeeds, warning that `TRACEPARENT` `00-not-a-trace` is not a W3C traceparent and is ignored
- **AND** the collector received the build's job span as the root of a trace of its own

#### Scenario: A malformed TRACESTATE is dropped

- **GIVEN** a coordinator with a worker, exporting traces to an OTLP collector
- **AND** `TRACEPARENT` set to `00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01`
- **AND** `TRACESTATE` set to `not a state`
- **WHEN** I build `app` on the coordinator
- **THEN** it succeeds, warning that `TRACESTATE` `not a state` is not a W3C tracestate and is ignored
- **AND** the collector received the build's job span in trace `4bf92f3577b34da6a3ce929d0e0e4736`, with no tracestate

#### Scenario: An all-zero trace ID is malformed

- **GIVEN** a coordinator with a worker
- **AND** `TRACEPARENT` set to `00-00000000000000000000000000000000-00f067aa0ba902b7-01`
- **WHEN** I build `app` on the coordinator
- **THEN** it succeeds, warning that `TRACEPARENT` is not a W3C traceparent and is ignored

### Requirement: Exported spans say what was built and why

Exported spans SHALL carry `vamos.job` on the job's span, `vamos.workspace` on each span of a named workspace's job,
`vamos.target` on a target's span, and on a target that missed, `vamos.miss.cause` and, for an `own-inputs` miss, the
files added, removed and changed that its stored trace names, up to 64 of each as the span's attribute budget allows.
`vamos.truncated` `true` SHALL mark a span with a list cut short or an attribute its budget cut, not a key or string cut
to its own size limit.

#### Scenario: A miss's cause and files are exported

- **GIVEN** a coordinator with a worker, exporting traces to an OTLP collector
- **AND** a workspace named `com.acme.storefront` that has built `app` on the coordinator, whose source `a.txt` changed
  since
- **WHEN** I build `app` on the coordinator
- **THEN** the exported span of `layer "app"` has `vamos.workspace` `com.acme.storefront`, `vamos.target` `//:app`
  with its platform, as the target's configuration names it,
  `vamos.miss.cause` `own-inputs` and `vamos.miss.changed` `a.txt`

#### Scenario: An unnamed workspace's spans carry no workspace

- **GIVEN** a coordinator with a worker, exporting traces to an OTLP collector, and an unnamed workspace
- **WHEN** I build `app` on the coordinator
- **THEN** no exported span has `vamos.workspace`
