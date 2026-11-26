# Spec Delta

## Purpose

Keeps a trace of every command that builds, on the executor that ran it, so that why a build was slow, what it reused
and how long that saved can be answered after the fact, whether or not `--trace` was given.

## ADDED Requirements

### Requirement: Every command that builds is traced

Every command that builds, `build`, `push`, `publish`, `load`, `save` and `export`, SHALL be traced whether or not
`--trace` is given: the client's own work, and every job it runs on its executor, local or a coordinator and its
workers. A command's trace SHALL be one trace that holds all of its work, the client's and each job's.

#### Scenario: A build without --trace has a trace

- **GIVEN** a project whose layer `app` must run
- **WHEN** I build `app` without `--trace`
- **AND** I read the trace that the build printed into a file
- **THEN** it shows `app` and the steps it ran

#### Scenario: A push's trace holds its build and its push

- **GIVEN** a project whose layer `app` must run, and a registry
- **WHEN** I push `app` to the registry
- **AND** I read the trace that the push printed into a file
- **THEN** it shows the client's load, `app` and its steps, and the copy of its image to the registry

### Requirement: A command prints its trace's digest

A command that builds SHALL print, on standard error once its jobs have ended, one line `vamos: trace <digest>` naming
its trace, whether it succeeded or failed. It SHALL print nothing else about a trace kept whole, and nothing on standard
output; it MAY warn on standard error where part of the trace couldn't be stored or measured. A command whose trace
can't be stored SHALL warn, print no digest, and end as it would have otherwise.

#### Scenario: A successful build prints its trace

- **GIVEN** a project whose layer `app` builds
- **WHEN** I build `app`
- **THEN** it succeeds, and its standard error holds one line `vamos: trace sha256:<hex>`

#### Scenario: A failed build prints its trace

- **GIVEN** a project whose task `check` runs a command that fails
- **WHEN** I build `check`
- **THEN** it fails, and its standard error holds one line `vamos: trace sha256:<hex>` before the failure
- **AND** reading that trace shows `check` and the step that failed

#### Scenario: A command that runs no job prints no trace

- **GIVEN** a BUILD file whose second line is not valid Starlark
- **WHEN** I build `app`
- **THEN** it fails naming the BUILD file with line 2 and its column, and prints no trace digest

#### Scenario: A trace that can't be stored is warned about

- **GIVEN** a coordinator whose content store refuses writes once the build has ended
- **WHEN** I build `app` on the coordinator
- **THEN** it succeeds, warning that its trace couldn't be stored, and prints no trace digest

### Requirement: vamos trace writes a stored trace

`vamos trace <digest|job> <file.json>` SHALL write the stored trace that the digest or job names, which Perfetto
opens: a command's as `--trace` would have written it, a job's, or an attempt's. With `--coordinator` it SHALL read
the coordinator's; without, the local executor's. A digest SHALL name a trace until a prune; a job ID SHALL name its
job's while the coordinator remembers the job, after its command ends too. A trace not found SHALL fail saying why.

#### Scenario: A trace is read by its digest

- **GIVEN** a build of `app` that printed its trace's digest
- **WHEN** I run `vamos trace <digest> trace.json`
- **THEN** it succeeds, and `trace.json` shows the build as `--trace trace.json` would have

#### Scenario: A coordinator's job's trace is read by its job ID

- **GIVEN** a build of `app` on a coordinator, which has ended and released its job
- **WHEN** I run `vamos trace <job> trace.json --coordinator <coordinator>` with that job's ID
- **THEN** it succeeds, and `trace.json` shows that job's targets and steps

#### Scenario: A job ID without a coordinator is a mistake

- **WHEN** I run `vamos trace <job> trace.json` without `--coordinator`
- **THEN** it fails saying that a job is read from the coordinator that holds it, and writes no file

#### Scenario: A digest cut short is a mistake

- **WHEN** I run `vamos trace sha256:abc123 trace.json`
- **THEN** it fails saying that `sha256:abc123` is not a valid digest, and writes no file

#### Scenario: A digest that names no trace is a mistake

- **GIVEN** a build of `app`, which writes a log
- **WHEN** I run `vamos trace <digest> trace.json` with the digest of that log
- **THEN** it fails naming the digest and saying it is not a trace, and writes no file

#### Scenario: A pruned trace is gone

- **GIVEN** a build of `app` that printed its trace's digest
- **AND** the build cache pruned since
- **WHEN** I run `vamos trace <digest> trace.json`
- **THEN** it fails naming the digest and saying the executor no longer holds it, and writes no file

#### Scenario: A job the coordinator no longer remembers

- **GIVEN** a coordinator that has forgotten the job of a build of `app`, as after a restart without its state
- **WHEN** I run `vamos trace <job> trace.json --coordinator <coordinator>`
- **THEN** it fails naming the job, saying the coordinator doesn't know it and that the command's trace digest still
  reads its trace, and writes no file

### Requirement: --trace writes the command's trace to a file

`--trace <file.json>` SHALL write the command's trace, retrieved from the executor once the command's jobs have ended,
to the file. It SHALL hold the client's spans and every job's, with the steps of the attempts they started or joined,
and SHALL say where a part of the trace was cut short or couldn't be read. Where it may be written and what happens
when it can't be SHALL be as before this change.

#### Scenario: --trace writes a command that ran no job

- **GIVEN** a BUILD file whose second line is not valid Starlark
- **WHEN** I build `app` with `--trace trace.json`
- **THEN** it fails, and `trace.json` holds the client's load, which failed

#### Scenario: --trace and vamos trace write the same trace

- **GIVEN** a project whose layer `app` must run
- **WHEN** I build `app` with `--trace first.json`
- **AND** I run `vamos trace <digest> second.json` with the digest it printed
- **THEN** both files hold the same spans

### Requirement: An attempt's trace is kept with its result

The trace of the attempt that made a layer's or task's result SHALL be kept with the result, and a later build that
reuses the result SHALL refer to it, and no part of the key. Every build that waits for an attempt to end, whenever it
joined, SHALL refer to the same trace of it, and SHALL show the attempt's steps: in the target's span for the build that
started it, and on a track named for the target for one that joined it.

#### Scenario: Two builds that share an attempt both show its steps

- **GIVEN** a coordinator, and two builds of the same `app` on it that share one attempt
- **WHEN** I read each build's trace
- **THEN** both show the steps of `app`'s attempt, the same spans in each
- **AND** in the trace of the build that joined it, they lie on the track `layer "app" (shared)`

#### Scenario: A pull's download is traced

- **GIVEN** a project that pulls an image that the executor hasn't pulled
- **WHEN** I build the pull and read its trace
- **THEN** it shows the pull resolving its tag and copying the image

### Requirement: A build that leaves an attempt says so

A build that leaves an attempt while it runs on, as a cancelled build does while another build waits for it, SHALL say
on the target's track that it left the attempt running, in place of the attempt's steps.

#### Scenario: A build that left a shared attempt says so

- **GIVEN** a coordinator, and two builds of the same `app` on it that share one attempt
- **WHEN** the build that started it is cancelled while `app` runs, and the other build finishes
- **THEN** the cancelled build's trace says on `app`'s track that it left the attempt running, and the other's shows all
  of `app`'s steps

### Requirement: A hit shows what its result cost and saved

A reused target SHALL show, in the trace, when its result was made, on what platform, how long the run that made it
took, the time the reuse saved, being that run's duration less the hit's, and the digest of the attempt's trace. The
run that made it SHALL exclude waiting for a worker and failed tries. A result made before vamos kept attempt traces
SHALL show none of these.

#### Scenario: A reused target shows the time it saved

- **GIVEN** a project whose layer `app` runs a step that takes two seconds, and that has built `app`
- **WHEN** I build `app` again
- **THEN** in its trace, `app`'s span says it was cached, when its result was made, that making it took at least two
  seconds, and that it saved at least two seconds less the time the hit took
- **AND** `vamos trace` given the attempt trace's digest that the span names writes a trace of `app`'s steps

### Requirement: A missing trace never costs a hit

A result whose attempt trace is gone SHALL still be reused: vamos SHALL NOT check the trace before reuse, and SHALL NOT
rebuild a target for want of it. A trace that would place the spans of an attempt whose trace is gone, and which a
target started or joined, SHALL say so, on that target's track, where the attempt's spans would have been.

#### Scenario: A result whose trace was removed is reused

- **GIVEN** a project that has built `app`
- **AND** the trace of `app`'s attempt removed from the build cache
- **WHEN** I build `app` again
- **THEN** it succeeds and `app` was reused

#### Scenario: A job whose attempt's trace is gone says so

- **GIVEN** a build of `app` that ran it and printed its trace's digest
- **AND** the trace of `app`'s attempt removed from the build cache
- **WHEN** I run `vamos trace <digest> trace.json`
- **THEN** it succeeds, and the trace says on `app`'s track that the attempt's trace is gone

### Requirement: A trace is capped in size

Each stored trace, an attempt's, a job's with its targets' outcomes, and a command's, SHALL be kept whole within a size
cap, as a log is. Spans past the cap SHALL be dropped, the latest started first, so that children go before the target
spans that hold them, and the trace SHALL say that it was cut short and how many spans it dropped. It SHALL say so in
the trace alone, and warn about nothing.

#### Scenario: An attempt that traces past the cap is cut short

- **GIVEN** a task whose attempt records more spans than an attempt's trace keeps
- **WHEN** I build it and read its trace
- **THEN** the trace shows the task's span, with its outcome, and its first steps, and says the attempt's trace was
  cut short
- **AND** the build printed no warning about its trace

### Requirement: Secrets never reach a stored trace

A stored trace SHALL hold no secret's value, as `--trace` holds none.

#### Scenario: A stored trace holds no secret

- **GIVEN** a task whose step is given a secret
- **WHEN** I build it with the secret and read its trace by its digest
- **THEN** the trace file holds no part of the secret's value
