# Scheduling and concurrency

This document describes the scheduling and concurrency model in vamos: how the scheduler traverses target graphs,
coordinates runner backends, deduplicates concurrent attempts, streams events, and enforces synchronization.

Both the local executor and the remote coordinator schedule graph execution using [sched](../../internal/sched/doc.go).
The scheduler walks the target graph in topological order, computes action keys, inspects cached results,
and submits un-cached actions to a `Runner` implementation.
Concurrent jobs requesting identical actions share in-flight execution attempts,
and the runner's completed outputs are written to the record store.
`sched` imports neither buildah, container storage, nor Starlark.
This decoupling keeps the `sched` and `coord` packages, which coordinate work without executing actions directly,
free of container runtime and evaluation dependencies.
It is a package boundary, not a binary one: the single `vamos` executable serves every subcommand,
including `coordinator`, and still links those libraries through the CLI.

## The scheduler and backend interfaces

The following diagram shows the scheduler and its backend interfaces, along with local and coordinator implementations.

```mermaid
classDiagram
    direction TB
    class Scheduler {
        +Run(ctx) ([]api.Result, error)
        -walk()
        -inputs(node) *Action
        -lookup()
        -resolve()
        -epoch()
    }
    class Backend {
        Content ContentStore
        Records RecordStore
        Checker Checker
        Runner Runner
    }
    class Runner {
        <<interface>>
        +Run(ctx, req Request, out Output) (*Report, error)
    }
    class Checker {
        <<interface>>
        +Check(ctx, action, result) (problem, err error)
    }
    class Events {
        <<interface>>
        +Status(node, status)
        +Output(node, stream, data)
        +Spans(spans)
    }
    class Sharing {
        attempts in flight by key
        +Run(ctx, req, out) (*Report, error)
    }
    class localRunner["run.localRunner"]
    class pool["coord.pool"]
    class checker["coord.checker"]

    Scheduler --> Backend
    Scheduler --> Events
    Backend --> Runner
    Backend --> Checker
    Runner <|.. Sharing
    Sharing o-- Runner : wraps
    Runner <|.. localRunner : buildah, in process
    Runner <|.. pool : assigns to workers
    Checker <|.. localRunner : container store
    Checker <|.. checker : registry and stores
```

The following diagram shows the action inputs and reports exchanged between the scheduler and runner.

```mermaid
classDiagram
    direction LR
    class Request {
        Action
        RunPlatform, Emulated
        Secrets, never encoded
    }
    class Action {
        Version, Kind, Base, Platform
        Image, Index, Pinned
        Steps, Command, Entrypoint
        Source, Output
        +Key() digest
        +RecordName() string
    }
    class Report {
        Result ActionResult
        Tag, TagRecord
    }
    Request *-- Action
    Request ..> Report : Runner.Run answers with
```

The scheduler and the runner share no in-memory state other than access to the content store.
A [`sched.Request`](../../internal/sched/doc.go) carries all inputs an action requires,
including resolved tag records for pull actions;
the runner does not read the record store or the target graph directly.
Requests and reports serialize to JSON for transport between the coordinator and workers over HTTP.
Only `sched.Sharing` writes results and tag records to the record store, and only after the runner has published
target artifacts and returned its report; the scheduler adds to target history, which names no artifact.
This sequencing guarantees that the record store never references missing artifacts;
when concurrent writes occur, the latest write wins.

## Walking a graph

The following diagram illustrates how `Scheduler.walk` processes pulls, evaluates cache hits, and dispatches actions.

```mermaid
flowchart TD
    start(["job starts"]) --> pulls["Resolve every pull<br/>(pulls of one tag in order)"]
    pulls --> ready{"Ready targets<br/>and below the bound?"}
    ready -- "no, and nothing runs" --> done(["terminal event"])
    ready -- "yes" --> withdrawn{"Demand<br/>withdrawn?"}
    withdrawn -- "yes" --> drain["Dispatch nothing more,<br/>let what runs finish"]
    withdrawn -- "no" --> key["Compute action key<br/>from inputs"]
    key --> lookup{"Result recorded<br/>for the key?"}
    lookup -- "yes" --> check{"Checker: artifacts<br/>still present?"}
    check -- "yes" --> hit["cached: replay log"]
    check -- "no" --> run
    lookup -- "no" --> run["Sharing.Run:<br/>attach or start attempt"]
    run --> ok{"Succeeded?"}
    ok -- "yes" --> record["Record result"] --> ready
    hit --> ready
    ok -- "no" --> fail["First failure is the job's,<br/>dispatch nothing more"]
    fail --> drain
    drain --> done
```

Graph execution proceeds in two stages:

1. **Pull resolution**: Every base image pull resolves before any build target begins.
   Pulls referencing the same tag resolve sequentially in declaration order to maintain deterministic tag selection.
2. **Target dispatch**: Ready nodes whose dependencies have completed dispatch concurrently
   up to the concurrency limit configured in `sched.Options.Bound`.

In the local executor, `Bound` is set to 1, executing ready targets sequentially.
The coordinator configures a higher bound, using a worker pool runner that distributes actions across workers.

When a target fails, the scheduler dispatches no further work.
Targets currently executing are allowed to finish and record their results.
The first failed target determines the job failure outcome, which the client maps back to its local graph.
Internal state maps in `walk` are modified exclusively by the main scheduling goroutine between dispatches;
running actions read only their immutable request values and the storage layers.

## Sharing execution attempts

An **attempt** is a single execution of an action.
Multiple concurrent jobs requiring the same action key share a single attempt managed by `sched.Sharing`.

The following sequence diagram shows two jobs requesting the same action key concurrently,
sharing a single in-flight attempt through `sched.Sharing`.

```mermaid
sequenceDiagram
    participant A as Job A
    participant B as Job B
    participant S as Sharing
    participant R as Runner
    participant Rec as Record store

    A->>S: Run(key K)
    S->>R: start attempt for K
    Note over S: A is attached and gets output live
    B->>S: Run(key K), a cache miss
    Note over S: B attaches to the attempt in flight
    R-->>S: Report
    S->>Rec: SetResult(K) once the artifact is published
    S-->>A: result
    S-->>B: result, with the log replayed
```

Sharing operates under specific coordination rules:
- **Cache miss attachment**: An ordinary job encountering a cache miss attaches to an existing in-flight attempt
  for that action key, including an attempt initiated by a forced rebuild.
- **Forced rebuilds**: A job running with `--rebuild` bypasses cache lookups and does not attach to existing attempts.
  Its new attempt replaces any previous attempt for that key, and subsequent jobs attach to the new execution.
- **Result delivery**: Each attempt delivers its outcome to all attached jobs,
  and the latest completed attempt updates the record store.
- **Cancellation**: Cancelling a job detaches that job from its shared attempt.
  An attempt continues running as long as at least one job remains attached;
  it is cancelled only when all attached jobs have detached.
- **Trace spans**: An attempt records its spans apart from every job's, and stores them as its own trace
  when its runner returns; each job that waits for the attempt to end is told of that trace,
  and a job that detaches while others remain learns only the attempt's ID (see [Trace transport](#trace-transport)).
- **Intra-job deduplication**: Nodes within a single job that share an identical action key
  (such as multiple platform variants of an index running a task on the execution platform)
  share an attempt unless a rebuild is forced.

Emulation permission governs whether an action may be dispatched under an emulator.
A job without emulation permission may attach to an in-flight emulating attempt,
because the completed artifact is valid for reuse, but it cannot start an emulated attempt itself.
When an unpermitted action requires emulation, the scheduler reports a platform mismatch error
specifying both the requested platform and the host platform, along with the options to resolve it
(enabling emulation with `--emulate` or running on a native worker).
Conversely, a job configured to run an action natively never attaches to an in-flight emulated attempt;
it starts a native attempt to avoid waiting on an emulator.

## Job lifecycle, events, and demand

The `sched.Jobs` registry manages all active jobs on an executor.
Each job runs in its own goroutine with a context that terminates only upon cancellation via `Cancel`.
Jobs stream progress events to a bounded buffer that enforces limits on both event count and total bytes.
Clients read events by position without blocking execution:
- If a client requests a position that has expired from the buffer, the stream emits a gap event.
- If a client falls behind the stream write buffer, the watch connection drops with `api.ErrFellBehind`,
  requiring the client to reconnect from its last received position.

A job's demand is defined by the presence of active client watch connections.
`Jobs` tracks the number of open watches on each build.
On a coordinator, builds configure a demand grace period:
if a build remains unwatched longer than the grace period, its demand is withdrawn.
When demand is withdrawn, running attempts finish and record their results, but no new actions are dispatched.
Explicit cancellation, by contrast, terminates the job's context immediately,
detaching it from all in-flight attempts.

The following state diagram shows the lifecycle of a job,
tracking client watch attachments, demand expiration, cancellation, and eviction.

```mermaid
stateDiagram-v2
    [*] --> Watched: Submit, then Watch
    Watched --> Unwatched: last watch closes
    Unwatched --> Watched: Watch again
    Unwatched --> Withdrawn: grace period passes
    Watched --> Withdrawn: Release while running
    Watched --> Cancelled: Cancel
    Unwatched --> Cancelled: Cancel
    Withdrawn --> Finished: running attempts finish, job fails as expired
    Watched --> Finished: terminal event
    Unwatched --> Finished: terminal event
    Cancelled --> Finished: terminal event
    Finished --> Evicted: window after end or last watch, or Release
    Evicted --> [*]: calls answer ErrExpired while remembered
```

In the local executor, no grace period is configured, so jobs never expire from lack of demand.
On a coordinator, a periodic sweep routine calls `Jobs.Sweep`,
which enforces the grace period and evicts completed jobs after their retention window passes.
While an attempt is queued for worker assignment, the worker pool polls `sched.Demanded` and `sched.Permitted`
at each heartbeat interval.
If all attached jobs have disconnected or cancelled,
or if an emulated attempt is no longer permitted by any attached job,
the pool cancels the queued attempt to prevent unneeded work.

### Cancellation

Cancellation takes effect at boundaries between non-interruptible steps:
between shell commands within buildah, between file transfers during checkout,
between replayed log records, between directory scans during capture,
and between statement evaluations in BUILD files.
Calling `Local.Close` cancels all active jobs, waits for running goroutines to exit,
removes lingering container state and temporary files, and returns any encountered errors.

## Concurrency and callbacks

Event listeners and progress consumers do not run in lockstep with the target scheduler:
file streaming, container layer extraction, and standard output delivery execute across multiple goroutines,
and distinct jobs run concurrently within the executor.
When invoking callbacks, data buffers may be shared, callback functions may execute in parallel,
and invoking client callbacks while holding internal mutexes can induce latency spikes or deadlocks.
Synchronization contracts are documented alongside the symbols that enforce them:
`sched.Events`, `sched.JobTrace`, `blobTracker`, `cache.LogRecorder`, `tracing.Recorder`, `tracing.Active`,
`cli.eventPrinter`, and `cli.traceSession`.

## Trace transport

Every job records its trace, whether or not the client asked for one, and stores it with its outcome:
traces are content blobs, not a stream, so that a build's trace can be read after the fact.
`sched.ForJob` gives a job a recorder and returns a `sched.JobTrace`,
which gathers the job's own spans and an entry for each node that went through the cache.
The job saves it as its manifest, an `api.JobTrace`, before it returns its outcome,
and `api.Outcome.Trace` names it; a manifest that can't be stored leaves the outcome naming none.

`sched.Sharing` owns each attempt's spans.
It gives the attempt a recorder of its own through `tracing.WithRecorderAt`,
so that the attempt's spans still nest under the starting target's span and lie on its track,
and an ID of 128 random bits.
When the runner returns, whatever the outcome, `Sharing` stores the spans, capped, as the attempt's trace,
under `context.WithoutCancel` and a 10-second bound, before it publishes:
a layer's or task's result names the trace, with `Provenance.Started` and `Took` from `sched.Report.Ran`,
the run that made it, so that a later hit says what it saved without reading the trace.
`publish`'s `cache write` span, which names the trace, is recorded apart.
Every job that waits for the attempt to end is told of the trace and the write span through `sched.Traced`,
and records them in its manifest: the starter in the target's span, a joiner on the target's `(shared)` track;
a job that leaves the attempt running learns its ID alone.
On a coordinator the runner is the pool, which relays each worker's spans, shifted onto its own clock,
into the attempt's context, retries included; workers always record, since an assignment always carries a trace.

The following sequence diagram shows where each part of a build's trace is recorded and stored.

```mermaid
sequenceDiagram
    participant C as Client
    participant J as sched.JobTrace
    participant S as sched.Sharing
    participant W as Runner or worker
    participant CS as Content store

    C->>J: Submit, ForJob records the job's spans
    J->>S: Run a target's action
    S->>W: Run with the attempt's own recorder
    W-->>S: spans as they end, relayed onto the coordinator's clock
    S->>CS: the attempt's trace, before its result names it
    S-->>J: Traced: the attempt's ID, trace and cache write span
    J->>CS: the job's manifest, before its outcome
    J-->>C: the outcome, naming the manifest
```

No span crosses as an event: the client reads the stored traces once its jobs end.
Its `cli.traceSession` measures the executor's clock against its own through `api.Capabilities.Clock`, bounded,
each sample without the client's retries, since a retried one is skewed and a coordinator that has gone would hold
the command,
and stores the offset in the command trace, by which assembly places the client's spans among the executor's.
The local executor runs on the client's own clock, so its offset is none, exactly, rather than measured:
a measurement is only as good as half a sample's round trip, which, under load, can put a job's span before the
client's that started it.

Recording costs every build something now, so `tracing.Enabled` guards no longer spare an executor anything:
arguments prepared only for a trace are prepared whenever a job runs.
A recorder keeps at most twice `tracing.MaxTraceSize` of spans, and each stored trace is trimmed to the cap.
The 10-second bounds on storing traces are deadlines the content store must honor:
one that ignores its context, as a stalled local disk may, delays the job or attempt for as long as it stalls.
Span hierarchy, flow endpoints, the stored encoding and its cap are managed by [tracing](../../internal/tracing/doc.go).
Network progress tracking in [transfer.go](../../internal/transfer/transfer.go) wraps containers/image progress
events through `blobTracker`; because image configurations may report completion without transferring bytes,
progress events reflect transfer progress rather than an absolute network audit.

## Cache directory locking

Disk-based storage in `cache.Directory` relies on filesystem operations rather than a process-wide build lock,
and does not provide multi-resource transactional rollbacks across graph blobs and container layers.
However, cache pruning and regular build operations exclude each other using a local file lock.

The following sequence diagram shows mutual exclusion between a client build holding a shared lock
on `cache.Directory` and a prune operation requesting an exclusive lock.

```mermaid
sequenceDiagram
    participant Client as Client (capture → finish)
    participant Dir as cache.Directory lock
    participant Prune as Directory.Prune

    Client->>Dir: open: flock shared
    Note over Client,Dir: the client and local executor share one handle
    Prune->>Dir: convert to exclusive, without waiting
    Dir-->>Prune: fails at once, naming the open store
    Client->>Dir: close
    Prune->>Dir: convert to exclusive
    Dir-->>Prune: granted
    Prune->>Prune: wipe store, remove cached images
```

Both build and prune operations request non-blocking locks (`flock`):
- If a prune request encounters an active build holding a shared lock,
  or a build encounters an active prune holding an exclusive lock,
  the operation fails immediately with an error identifying the conflicting store access.
- An executor also rejects prune requests whenever any of its internal jobs are currently running.

This lock operates as a local host kernel lock; it does not coordinate access across multiple network nodes.
On a remote coordinator, cache pruning across concurrent clients is fenced by store generation instead;
see [Storage and identity](storage.md#pruning).
