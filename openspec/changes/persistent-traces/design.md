# Design

## Context

Today a trace exists only when the client asks. With `--trace`, `inv.traced` gives the client a `tracing.Recorder`,
every request carries a `tracing.CallContext`, and each executor job calls `sched.ForCall`, which records only if
that is set and streams spans back as `api.EventSpans`. The client shifts them by a clock offset that
`tracing.EstimateOffset` measures, then writes Chrome JSON. An attempt's spans reach the job that started it through
`tracing.Revocable` in `sched.Sharing.attempt`. A job that joins someone else's attempt sees none of its spans. A
coordinator relays a worker's spans into the attempt's context (`pool` events, `tracing.Relay`), and a worker records
only when its assignment carries `Trace`.

`cache.ActionResult` already carries `Log` and `Provenance`. Neither is part of the key, and `Scheduler.lookup` checks
the log before reuse. `RecordStore` has results, tags and the generation. `Directory` and `Memory` are its only
implementations: `remote.Client` is a content store, not a record store, so the proposal's "remote store" has no
counterpart, and history needs none, since only executors write records. `Directory.ClearResults` removes `ac/` and
leaves `tags/`.

`sched.Action` holds everything a key covers: the base's image ID, steps with each added tree's blob and epoch, and the
command, entrypoint and output selection. It doesn't say who supplied the base or the files, and the graph has no
stable target identity. `Node.Desc` leaves out the platform when it is the command's, and parameters at their defaults,
so the same target can have different `Desc`s in two invocations.

See proposal.md for motivation, and the two specs for required behavior.

## Goals / Non-Goals

**Goals:**

- One recording path, always on, for the client, executors and workers, with the client's `--trace` and `vamos trace`
  reading the same stored data.
- Attempt traces written once, by `Sharing`, for both executors, and shared by every job that starts or joins the
  attempt.
- Miss attribution computed in `sched`, so that the local executor and a coordinator agree, from history that never
  refers to a content blob.

**Non-Goals:**

- Exporting traces or metrics (`telemetry-export`).
- Showing a miss's cause in the status line. It is in the trace only, where the files it names have room.
- Keeping traces past a prune, or storing a trace for a command that ran no job, or for a prune. `--trace` still
  writes such a command's client spans, as today.
- History and causes for pulls and indexes. A pull's resolve attempt is traced like any other, but a pull is a
  supplier (`upstream`, `dependency`), not a target with a result of its own to miss.
- Attribution across executors: a target's history on the local executor and on a coordinator are separate.
- A partial trace of an attempt for a job that left it while it ran on. Such a job's trace says it left; the jobs that
  waited for the attempt hold it whole.

## Decisions

### Three stored traces: attempt, job manifest, command

Each is a content blob of versioned JSON, with a `kind` field so that `vamos trace` can read any of them by digest.

- **Attempt trace**: the spans of one attempt that `Sharing` ran: a layer's, a task's, or a pull's resolve.
  `ActionResult.Trace` names a layer's or task's on success, and the job manifest of every job that started or joined
  the attempt and waited for its end names it whatever the outcome, so that a failed step's spans are kept.
- **Job manifest**: the job's own spans (scheduling, lookups, checks, transfers, pushes) and, per node that went
  through `Sharing` or the cache, its node, outcome (`hit`, `miss` or `joined`), whether it started the attempt, its
  miss cause, the attempt's ID, and the attempt trace it took, or that made the result it reused, or none if it left the
  attempt running. A pull's entry has no cause. `api.Outcome.Trace` names the manifest. A cause's file lists live in the
  target's span arguments alone, not again in its entry.
- **Command trace**: the client's spans, the clock offset measured against the executor, and each job's manifest in
  order. The client stores it once its jobs have ended and prints its digest.

The span encoding, its version and the size cap belong to `tracing`, which still imports nothing. The documents that
name blobs (`api.JobTrace`, `api.CommandTrace`) belong to `api`, which already imports `cache`, `graph` and `tracing`,
and which both `sched` and the CLI read.

*Alternative:* one blob per command, written by the executor. Rejected: the executor doesn't know which jobs make up a
command. A push is a graph job and then a push job, submitted separately.

*Alternative:* inline attempt spans in each manifest. Rejected: a shared attempt would be stored once per build, and a
hit could not refer to the trace of the attempt that made its result.

### Recording is always on, and live span events end

`sched.ForCall` becomes `sched.ForJob(ctx)`. It always gives the job a recorder and returns a `*sched.JobTrace`, which
gathers the job's own spans and each target's outcome, and saves the manifest. Every executor job except a prune calls
it: `run.Local`'s graph, push, export and publish jobs and the coordinator's. Each saves its manifest before returning
its outcome, so the terminal event names it. The coordinator's journal already records outcomes, so a job's manifest
survives a restart with it. A manifest that can't be saved leaves the outcome naming none and never changes the job's
outcome otherwise; the client warns that the job's trace couldn't be stored, the command trace records the job without
a manifest, and assembly says so with a `trace incomplete` instant on the client's track.

A cancelled job or command still stores its trace, since a cancelled build is one whose trace is wanted. A job's
manifest is written under `context.WithoutCancel` of the job's context, and an attempt's trace under
`context.WithoutCancel` of the attempt's context, since the last job to detach cancels that context, each bounded by 10
seconds. `publish` keeps its own uncancelled context without one, so that a slow trace store never stops the result
being recorded. The client's cleanup once its jobs end runs under `context.WithoutCancel` of the command's context, each
step with its own bound: storing the command trace, at most 16 MiB, has 10 seconds; assembling `--trace`'s file (below)
has 60 seconds, since a trace of many attempts can take longer than 10 seconds to read over a slow link; and releasing
the jobs has 10 seconds. So an interrupted command still stores and writes its trace, and every step is bounded.
Against an executor that stops answering, an interrupted command can take up to those bounds longer to exit than today.

*Alternative:* one deadline over the whole cleanup. Rejected: trace work could use it up and leave release none, and
the jobs would stay held until their hold lapses.

Clients stop sending `Trace` on requests, and executors stop sending `api.EventSpans`. `--trace` retrieves instead of
streaming (below). Both fields stay decodable. An older client that sends `Trace` gets no spans, and an older
executor's outcome names no manifest, so a new client's trace holds only its own spans and warns that the executor
keeps none. Workers always record: `pool.NextAssignment` always gives a `Trace`, since an attempt's context always
records now. `tracing.Enabled` guards stay where they are, but they no longer save anything on a build: on executors
they are always true, and on the client they are true whenever a command builds, so arguments prepared only for a trace
are now prepared by every build (see Risks).

The client records always: `inv.traced` creates the recorder whether or not `--trace` was given, and only the file
checks in `newTraceSession` depend on the flag. The clock offset is measured whenever a job runs. Today a failure to
measure it fails the command, which only `--trace` could cause; now it would be every build. A failure is instead
warned about, and the command trace stores a zero offset marked unknown, which assembly notes on the client's track.

*Alternative:* keep streaming spans and also store them. Rejected: two paths to keep in agreement. A hit's attempt
trace has to be fetched from the store anyway, and nothing needs spans before the command ends.

### Sharing owns the attempt's recorder and writes its trace

In `Sharing.attempt`, the attempt's context gets a recorder of its own in place of `tracing.Revocable`, for resolves
as for layers and tasks, and the attempt gets an ID of 128 random bits. A new `tracing.WithRecorderAt(ctx, r)` keeps
ctx's parent span and track, so the attempt's spans nest under the starting target's span and lie on its track, as
today. `Revocable` goes: no attempt span reaches a job's recorder any more, so there is nothing to revoke. When the
runner returns, whatever the outcome, `Sharing` puts the spans into the content store, capped, with the attempt's ID.
On success, before `publish` calls `SetResult`, it sets `Result.Trace` and `Provenance.Started`/`Provenance.Took`. It
keeps the blob, and `publish`'s `cache write` span (below), on the attempt before it closes `done`, and every job that
waits on `done` takes them from there in `await`: one attached when the runner returned, one that joined while the
attempt published, as the attempt stays in flight until then, and the last job to detach, which waits for the
cancelled attempt to end. Its `Output`, if it implements a new `sched.Traced` interface, as `Joiner` is implemented, is
told the blob, and the scheduler's `targetOutput`, and the resolve's output, record it in the job's manifest.

The attempt's trace is therefore saved before `publish`, and can't hold the `cache write` span, which `SetResult` ends
and which needs the blob's digest first. `publish` records that span apart, and `Traced` hands it, with the blob, to
every job that takes the blob. The job that started the attempt records it as its own on the target's track, where it
nests under the target's span as today, error included, since that span is still open while the attempt publishes. A
job that joined the attempt records it on its `(shared)` track beside the attempt's spans (see Assembly), since its
target span may have begun after the write did.

A job that detaches while the attempt runs on, as a cancelled starter does when another job is attached, can't wait for
the blob, since its manifest is saved as it ends. `Traced` tells it the attempt's ID alone, and its manifest entry names
the attempt as left running, with no blob. A recorder keeps only spans that have ended, as today, so a stored trace
holds no span still open when it was stored: a cancelled runner ends its steps as it returns, and on a coordinator a
lost or withdrawn worker's running step, whose end never arrives, is missing from the attempt's trace, as it is from
`--trace` today.

*Alternative:* a snapshot of the attempt's spans for a job that leaves, with the step then running cut off where it
left. Rejected: it needs recorders to track open spans, workers to send each span as it starts, and the pool to cut off
each try's spans before a retry, all for the trace of a build cancelled while another went on with its attempt. That
build's target says it left the attempt, and the other build's trace holds the attempt whole.

`Took` is the run that made the result, not the attempt's whole life: locally the runner's run; on a coordinator, the
successful try from its assignment to its report, which the pool returns in a new `sched.Report.Ran` interval, so that
waiting for a worker and failed tries don't inflate `saved`. `Started` is that run's start. A runner that reports no
interval, as an older worker's, falls back to the attempt's start and end.

On a coordinator the runner is the pool, which relays the worker's shifted spans into this same context, so the
attempt's trace holds the worker's spans, retries included, on the coordinator's clock.

A trace that can't be written is reported as a span error and never fails the attempt: the result is recorded without
`Trace`.

### A hit reads nothing to show what it saved

`Provenance` gains `Started` and `Took`, so a hit sets its span's `made`, `platform`, `took`, `saved` (`Took` less the
hit span's duration, never below zero) and `attempt trace` arguments without reading the trace blob. `Checker`s and
`lookup` ignore `Trace`, so a missing trace never costs a hit. A result made before this change has no `Started`, and
its span gets none of these arguments.

### Assembly is the client's

`cli` reads a command trace, then its manifests with one `ReadMany`, then every attempt trace that a manifest took,
deduplicated, with another. If a batch fails with `ErrMissing` or `ErrCorrupt`, its blobs are read one by one, so one
missing trace costs only its own spans. Hits' attempt traces aren't placed on the timeline: they happened at another
time, and the hit's span names the digest for `vamos trace`. A missing attempt trace becomes an instant
`trace incomplete` on its target's track.

An attempt's spans are placed as they were recorded only in the trace of the job that started it, where they nest under
its target's span. In a job that joined it, they go on a track of their own, named for the target and `(shared)`, with
their parents cleared, since the starter's spans aren't in that trace and a step that began before the joiner's target
span would otherwise overlap it on one track without nesting, which `WriteChrome` forbids. Client spans are shifted onto
the executor's clock by the stored offset, and everything is written by `tracing.WriteChrome`, as today. The executor's
spans no longer name a client span as their parent. Nothing reads that link: `WriteChrome` nests by track and time, and
the trace feature checks nesting only within one process.

A manifest entry and a `cache write` span's `attempt` argument name the attempt's ID, so that assembly knows the entries
of one attempt. An attempt that one of a command's jobs started and another joined, as a push's graph job and push job
might, is placed once: under the starter's target if the starter took its trace, and otherwise on the `(shared)` track
of the first job, in the command trace's order, that took it, so that `--trace` and `vamos trace` place it alike. The
other jobs' targets note the join, and their `cache write` spans of that attempt are left out. An attempt that no job of
the command took, as when its only job left it running, becomes a `trace incomplete` instant on the target's track,
saying the job left the attempt running.

Two nodes of one job can share an attempt too, as two targets with the same action do. The job records the attempt's
`cache write` span once, as the starter's, and takes no `(shared)` track for it, so assembly places it once, under the
starter's target; the other node's entry names it as joined, with that node's own outcome and cause.

`--trace` stores the command trace and assembles from it, so `--trace` and `vamos trace` give the same file. If the
store fails, it assembles from the same document in memory, so `--trace` still writes the file. A command that ran no
job, as one whose BUILD file has a mistake, stores nothing and prints no digest, and `--trace` writes its client spans
alone, as `TestTrace_IsWrittenEvenIfTheCommandFails` expects today.

The client stores the command trace, and assembles `--trace`'s file, before it releases its jobs. A coordinator refuses
a prune while any job is open, so no prune can take a manifest between the job's end and the command trace that names
it. The jobs are held until `--trace`'s file is written too, since assembly reads the manifests and attempt traces,
which a prune would take; without `--trace`, they go once the command trace is stored.

### `vamos trace <digest|job> <file.json>`

An argument that holds a `:` is a digest, since a job ID is hex alone, and one that doesn't parse as a digest, such as
one cut short in copying, fails naming it as an invalid digest rather than being taken for a job. A digest is read from
the executor's content store, and the blob's `kind` says what it is. A blob that isn't a stored trace, such as a log, or
whose `kind` or version this vamos doesn't know, fails naming the digest as no trace it can read, and writes nothing.
One whose `Stat` size passes `MaxTraceSize` isn't read, and fails as no trace, so that the digest of a layer is never
read whole. Every read of the store takes a `cache.Blob`, digest and size, and a wrong size reads as corrupt and drops
the blob, so a digest alone can't be read today. `ContentStore` gains `Stat(ctx, digest) (Blob, error)`, which gives
the size of the blob the store holds under the digest, or `ErrMissing`: `Directory` stats the file, `Memory` looks it
up, and `remote` adds an endpoint for it. `cachetest.Content` holds every implementation to it. The printed digest
stays a bare digest.

Anything else is a job ID, valid only with `--coordinator`. A client releases its jobs as its command ends, and a
released job that has finished is evicted at the next sweep, so its outcome is gone almost at once. `sched.Jobs`
remembers its last 4,096 evicted jobs in memory as a set of IDs; that memory becomes a map from ID to the manifest's
blob, so that it serves a coordinator with or without `Config.State`, and the journal's tombstone keeps the blob too, so
that `RememberEvicted` restores it after a restart. `api.Executor` gains `Trace(ctx, id) (cache.Blob, error)`, which
answers from a held job's outcome or a remembered evicted job's blob, and which `remote` serves and `apitest.Contract`
checks. A job ID so reads its trace for as long as the coordinator remembers the job: while it is held, and then among
the last 4,096 it evicted, until a prune. `run.Local` implements it for its held jobs, which the contract checks, though
the CLI never asks a local executor by job. `ErrRunning`, `ErrExpired` and `ErrNoJob` become messages; the last two say
that the command's trace digest still reads the trace. Clients see no job IDs: they reach operators through the
coordinator's log and, once `telemetry-export` lands, its exported spans. Without `--coordinator`, a job ID fails at
once: a local job lives only as long as the process that ran it. The output file follows `--trace`'s rules
(`finish.CheckSaveDest`, `finish.WriteFile`). The command opens the executor as other commands do and loads no BUILD
file.

### Size caps against ReadMany

`tracing.MaxTraceSize` is 16 MiB, and bounds each stored document whole, as the `build-traces` spec says: an attempt
trace, a manifest with its entries, and a command trace. A recorder counts each span's encoded size as it ends and keeps
at most twice the cap, so its memory is bounded too; past that it refuses spans and counts them. The bound is per
recorder, not per executor: see Risks.

A document is trimmed to the cap only when it is stored, by one rule: spans are dropped latest-started first until it
fits. A span starts before the spans inside it, so children go before the target and job spans that hold them, and a
trace keeps its first steps. A span refused while recording can leave children kept that name it as their parent;
`WriteChrome` nests by track and time, not by parent, so they are drawn in place. A stored document that lost spans,
while recording or when stored, gets one `trace truncated` instant, on the track of the earliest-started span it lost,
with the count of spans dropped, and the trim leaves room for it.

A manifest keeps its entries before its own spans: they are small, one per node, and carry every target's outcome and
cause whatever spans are dropped. `JobTrace.Save` trims spans by the same rule to fit beside the entries. Entries that
alone would pass the cap are dropped from the end, the manifest counts them, and assembly says on the client's track
how many targets are missing, so even a graph of hundreds of thousands of nodes stores a valid manifest. 16 MiB is
below `remote.maxReadManyBytes` (64 MiB), so manifests and attempt traces batch in `ReadMany` as trees and logs do, and
nothing about `ReadMany` changes. This settles the proposal's open question.

### Targets are identified in the graph

`graph.Node` gains `Target string`: the canonical label (`//pkg:name`), the platform, and every parameter's value,
sorted by name, so that reordering a BUILD file's declarations changes no identity,
`//ui:build platform=linux/arm64 mode="debug"`. `load` sets it from `confLabel` for pulls, layers and tasks, beside
`Desc`. Only layers and tasks keep history; a pull's `Target` identifies it as a supplier. It is metadata: no key reads
it. `graph.Version` stays at 4, since an older executor ignores the field and a graph without it builds the same. A node
without `Target`, as from an older client, keeps no history, as an unnamed workspace keeps none. Adding a parameter
gives every configuration of the target another `Target`, and changing a default gives one to each configuration that
takes it. Either way, a configuration's first miss under its new `Target` is `new`, unless that `Target` was built
before; keying history by label alone would mix every configuration in one record of 8 entries.

*Alternative:* key history by `Desc`. Rejected: it changes with the command's platform, which `Target` always names, so
one configuration has different `Desc`s in invocations for different platforms.

### Target history is a record kind with an atomic add

`RecordStore` gains:

- `History(ctx, name) (*History, error)`
- `Newest(ctx, name) (HistoryHead, error)`, the newest add's fingerprint and the record's `Used`, decoded from the
  record's leading fields without the rest.
- `AddHistory(ctx, name, entry, keep) error`, which, atomically against every other add to the same store, takes the
  entry's fingerprint as the newest, puts the entry first unless it is too large to keep (below), drops any earlier
  entry with its key, keeps at most `keep`, drops trees no entry still uses, and sets `Used` to the time it publishes.
- `PruneHistory(ctx, before) (removed int, err error)`, which, under the same lock as adds, removes every record whose
  `Used` is before `before`. A record it fails to remove stays for the next prune while it goes on with the rest; it
  counts every record it removed and returns the failures joined.

`name` is the digest of the workspace name and `Target`, which `sched.HistoryName` computes. `cache.History` holds
`Newest`, the newest add's fingerprint, which is the first entry's unless that add's entry was too large to keep,
`Used`, `Entries` and `Trees`, encoded in that order so that `Newest` stops reading after `Used`. `Trees` holds tree
entries (path, type, mode, link target, content digest) by tree digest, so that entries with the same source share it.
A `HistoryEntry` holds the action's key, `ActionVersion`, when it was recorded, the encoded `sched.Action` (as raw JSON,
since `cache` can't import `sched`), and what supplied each input: for the base, its `Target`, and for a pull also its
image reference and manifest digest; for each `add`, the source's tree or the task's `Target`.

`Directory` keeps each record as `history/<name>`, published by a rename, which `ClearResults` leaves alone. Every add
and history prune takes the history lock: an in-process mutex, then an exclusive `flock` on `history/lock`, taken
without blocking, as every other `Directory` lock is, and retried for up to 10 seconds in all, mutex wait included, or
until the context ends. A lock an add can't have is a history failure, a span error that fails no target. Reads take no
lock, since a rename publishes a record whole. So two local processes sharing the directory don't lose each other's
entries, and adds and prunes never race. `Memory` does all of it under its mutex. `cachetest.Records` checks concurrent
adds, `keep`, the deduplication of keys and trees, that `ClearResults` leaves history, and that `PruneHistory` judges a
record by `Used`.

A history record is capped at `MaxHistorySize`, 16 MiB, budgeted over the whole record. Adding drops the oldest entries
until the record fits, so a target with very large sources keeps fewer than 8. If the newest entry alone doesn't fit,
its trees are kept by digest without their entries, largest first, until it does, so the newest action's key and
configuration are always kept. An action whose entry passes the cap even then, by steps or variables of megabytes, adds
no entry, so a miss that follows it compares with older entries, or is `new`. Its add still publishes the record, with
its fingerprint as the newest and `Used` moved, so that the action's hits find it newest and add nothing more, and a
target built every day keeps its history. A miss that compares with a source kept by digest counts it as one difference
and says it changed without listing files. The `cache-miss-attribution` spec states these limits.

A prune keeps history, as attribution after a prune needs it, but records would otherwise grow with every target ever
built. Both executors' prune jobs call `PruneHistory` right after `ClearResults`, with a cutoff of
`cache.HistoryRetention`, 100 days, before now, and before the coordinator clears its cache repository's tags, whose
failure ends the job early. `Used` lags a target's last build or reuse by at most the 7 days a hit waits before it
refreshes the record (below), so a target built or reused in the last 90 days keeps its history across the prune, and
one neither built nor reused in 100 days loses it. `Used`, not the newest entry's time, decides, since a target reused
every day for months adds no entry after its first: pruning by entry time would drop its history, and its next miss
would read as `new`. The history lock keeps a prune apart from every add, in this process or another, so
`PruneHistory` asks nothing of its caller and `Directory.Prune` doesn't change. A history prune that fails keeps the
records it failed to remove for the next prune, never ends the job early, and is a warning in the prune's outcome, with
the count of those it removed: `api.Pruned` gains `Histories`, the records removed, which the client prints with the
other counts, and `Warnings`, one for each failure, which it prints as warnings.

*Alternative:* read-modify-write through `SetHistory`. Rejected: concurrent builds would drop each other's entries,
which the spec forbids.

*Alternative:* refer to trees in the content store. Rejected: a prune would take them, and attribution after a prune
would lose its files.

*Alternative:* a lock per target, and marking a record used without rewriting it, by its modification time. Rejected:
per-target lock files must then be cleaned up apart from their records, and a lock-free mark races adds. Adds are rare
enough for one lock, and a weekly rewrite on reuse costs little.

### The cost of history on hits

An entry's fingerprint is the digest of its key and its suppliers' identities, the `Target` of each and a pull's image
reference and manifest digest, since suppliers are no part of the key: a target whose base switched to another layer of
the same image hits the same key, and its history must still learn the new supplier, or the next change of that supplier
would read as `own-inputs`; and a pull whose tag moved to another manifest of the same image hits it too, and history
must learn the manifest, or a later `upstream` would name the wrong one as `before`. A hit reads `Newest` and adds an
entry when its fingerprint isn't the newest, or when `Used` is more than 7 days old, so that retention follows reuse.
That is one small read per hit, of the record's leading fields, beside the result record that `lookup` already reads,
so a fully cached build of 2,000 targets does 2,000 more small reads, and at most once a week per target a rewrite of
its record; a build that switches between branches writes once per target per switch. Skipping the read would let a hit
on a result made by another workspace, or by a branch history no longer holds, go unrecorded, and a later miss would be
compared with the wrong entries. On a miss, the scheduler reads the history once, before running, to attribute it, and
adds the entry once the target succeeds. Building an entry reads each added source's tree that the record doesn't
already hold by its digest, which `ReadTree` bounds, so a weekly refresh reads none, and a hit after a branch switch
reads only the trees that changed. History is best-effort: a failure to read or add is a span error and never fails the
target, and the target's next hit tries the add again.

### Attribution, nearest entry and the order of causes

`sched.attribute(current, history)` is a pure function, tested by table in `sched`:

1. `forced`, if `--rebuild` named the target, known before any lookup.
2. `lost`, if a result was found but couldn't be used, whether the checker or log refused it at lookup or a target that
   adds its files found them unusable and had it run again (a regeneration), or if any entry has the current key. A
   regeneration runs with the scheduler's own force, which is not `--rebuild`, so it is `lost`, never `forced`, and its
   miss replaces the hit the job first recorded for that target in the manifest.
3. `new`, if no entry has the current `ActionVersion`, or the workspace is unnamed, or the node has no `Target`.
4. Otherwise compare with the nearest entry among those of the current `ActionVersion` alone, so that an upgrade never
   compares actions encoded differently. The action difference is computed once, as a structure that keeps the target's
   own configuration apart from what suppliers gave it, and distance, cause and reported paths all derive from it. The
   distance counts one for each step that differs by position, for the kind, the command, the entrypoint, the output
   source directory and the output selection, for each path added, removed or changed in each source, for the base, and
   for each added task's files or epoch. Ties go to the most recent. The cause is then: `own-inputs` if the kind (a
   layer or a task), any step, command, entrypoint, output source or selection or source differs, or a different
   `Target` supplies the base or an `add`; otherwise `upstream` if the only difference is the base pull's manifest
   digest, the same pull `Target` and image reference; otherwise `dependency`, naming every supplier that differs, a
   moved base pull among them.

A pull whose image reference changed, as when its tag is edited in a BUILD file, is a supplier whose own declaration
changed, so its dependents' cause is `dependency`, naming the pull, as for a layer whose step changed; `upstream` is
kept for the same reference naming another image, by a moved tag or an updated pin, and nothing else changing. Switching
the base from one pull to another is a different `Target`, so `own-inputs`, whatever their references. A step count that
differs makes every step past the shorter one differ. The span gets `miss`, and for `own-inputs`, `added`, `removed` and
`changed` (each at most 100 paths, with a count of all), for `upstream`, `pull`, `before` and `after`, and for
`dependency`, the `Desc`s of the suppliers that changed. A joined target computes its own cause.

### The workspace reaches the scheduler

`sched.Options` gains `Workspace`, which `run.Local` and the coordinator set from `GraphRequest.Workspace`. This is
the use that `workspace-name` deferred.

## Risks / Trade-offs

- [Recording every build costs time and memory where `--trace`-less builds paid nothing] → Spans are already cheap and
  batched. `BenchmarkSchedulerCached` installs no recorder and has no workspace or targets, so a new benchmark runs
  the same graph as a job would, recorded, named and with history, reporting allocations; compare it before and after.
  `Enabled` checks no longer spare a build anything, so the benchmark counts arguments prepared only for a trace, such
  as formatted trees and paths, and any it shows costly are made cheaper or dropped.
- [Assembly holds a command's whole trace in memory] → Each document is capped, but a command of many large attempts
  could need gigabytes. Most attempts trace kilobytes; task 9.1's assembly benchmark measures it for many large attempt
  traces, and assembly can stream by track if it proves costly.
- [A trace blob per result grows the content store] → It is capped, and a prune clears it with everything else. Most
  attempts trace kilobytes.
- [A job past its recording limit refuses later targets' spans, so their misses' files aren't in its trace] → The
  manifest's entries still carry every target's outcome and cause, and the limit is twice a cap that builds trace far
  below.
- [History records grow with large globs] → Trees are deduplicated within a record, the record is capped as a whole,
  and the largest trees are kept by digest when the newest entry wouldn't fit.
- [One `flock` serializes every history add and prune on a cache directory] → An add is one small read and write, and
  only misses, hits whose fingerprint differs from the newest add's, and a weekly refresh add. Reads take no lock.
- [The size cap bounds each recorder, not an executor] → A coordinator running many jobs and attempts at once could hold
  up to twice the cap, 32 MiB, in each recorder. Most attempts and jobs trace kilobytes, and the number at once is
  bounded by the workers and the jobs held, so no shared budget is kept. A shared budget would let one large trace
  starve others.
- [Each attempt stores its trace before it publishes, so on a cold build a slow content store delays every dependent
  along a chain, by up to its 10-second bound per attempt, `--trace` or not] → The store is the executor's own, and the
  trace small. Task 9.1 benchmarks a cold dependency chain against a store that delays writes. If that shows a cost,
  overlapping the write with publishing is a follow-up, which must still keep a result from naming a trace that was
  never stored.
- [A build that left a shared attempt running shows a gap where the attempt was] → Its target says it left the attempt;
  every job that waits for the attempt's end gets the whole trace.
- [A lost worker's running step is missing from the attempt's trace] → As with `--trace` today. The try's ended spans,
  and the retry's, are kept.
- [Three Capabilities round trips per command to measure the clock, where only `--trace` paid them] → One sample when
  `--trace` isn't given. A stored trace's alignment is then rougher, which matters only for client spans against
  executor spans.
- [An older client's `--trace` loses executor spans against a new executor] → Accepted, silently: the older client has
  no way to learn why. Only a new client against an older executor warns, finding no manifest in the outcome. Versions
  are expected to move together, as graph versions already require.
- [A joiner's trace shows the shared attempt on a track apart from its target] → The track is named for the target
  and `(shared)`, so it reads as the work the target waited for.

## Migration Plan

No data migration: results without `Trace` or `Provenance.Started` decode and are reused, and show nothing of their
attempt. History starts empty, so a named workspace's first miss of each target is `new`. `ActionVersion` doesn't
change, since no key changes. Rolling back leaves `history/` and trace blobs unread. An older vamos ignores the new
fields, and its `Prune` clears the content.
