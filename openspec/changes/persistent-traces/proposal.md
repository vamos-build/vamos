# Proposal

## Why

A build's trace exists only if `--trace` was given before the build started, and only on that client's disk,
so the question "why was that build slow?" can't be answered after the fact. A cached hit says nothing about what
it saved, and a miss says nothing about why it missed: in particular, a glob that picks up files its target doesn't
need shows up only as misses that look like any other.

## What Changes

- Traces are always captured, by the client, by the local executor and by a coordinator and its workers alike.
  A command's trace is one thing that holds all of its work: the client's, and each job's it ran.
  `--trace` decides only whether the client retrieves the trace and writes the Perfetto file;
  a build without it still has one.
- The spans of the attempt that made a result are stored as a content blob, named by a new `ActionResult.Trace`,
  beside `Log`, in vamos's own span format and capped in size as logs are.
  Like `Provenance`, it is no part of the key.
  Every build that joins a shared attempt refers to the same blob.
- **A missing trace blob never invalidates a result**: unlike a log, it isn't checked before reuse;
  a build whose result has lost its trace gets a gap in its trace, never a rebuild.
- Each build has a trace manifest, a content blob holding the job's own spans (scheduling, checks, transfers,
  pushes) and, for each target, whether it hit, missed or joined an attempt, with a reference to the attempt's
  trace blob; a hit refers to the attempt that originally made its result, so the trace shows when, where and how
  long that took, and the time the cache saved.
- Each command stores a command trace, a content blob holding the client's spans and naming each of its jobs'
  manifests, and prints its digest on standard error. `vamos trace <digest|job> <file.json>` writes a stored trace:
  a digest reads it until a prune, and a job ID reads that job's for as long as the coordinator remembers the job.
- Each miss carries a cause: `new` (no earlier build of the target in this configuration, its parameters and
  platform), `forced` (`--rebuild`), `lost` (the same key as before, but its result is gone or unusable, as after a
  prune or when its files are found corrupt), `upstream` (a pulled tag now names another image), `dependency` (only
  because the base image, or the files and their epoch, a target it depends on supplies changed, whether that target
  hit or missed) or `own-inputs` (a step, command or source of its own changed). An `own-inputs` miss caused by a
  source names the files added, removed and changed, which is what reveals a glob that is too broad.
- To find causes, an executor keeps target history: a new kind of record, keyed by workspace name and target
  (label, parameters and platform), holding the last 8 actions the target used, written on hits and misses alike.
  A miss is compared with the nearest of those 8, so that builds of several branches don't make each other's
  misses look larger than they are.
  Each entry holds inline, as a tag record holds its manifest, what attribution compares: the action's key, its
  steps and commands, the base image, or the files and their epoch, each target it depends on supplied, the
  digests of pulled images, and each source's complete tree entries (path, type, mode, link target and content
  digest).
  It refers to no content blob, so target history survives a prune, as tag records do: after a prune, a miss whose
  action is unchanged is `lost`, and one whose action changed is still attributed to what changed.
- An entry is added to target history atomically: concurrent builds of one target, on one executor, each add their
  own entry and none drops another's.
  A workspace without a name keeps no history, and its misses are `new`, `forced`, or `lost` where the result
  found can't be used.

## Capabilities

### New Capabilities

- `build-traces`: always-on capture, storage of attempt traces with results, of each job's manifest and of each
  command's trace, the digest a command prints, retrieval with `--trace` and `vamos trace`, the rule that a missing
  trace never costs a hit, and the time a hit saved.
- `cache-miss-attribution`: target history, the causes a miss can have, and how a source's changed files are found.

### Modified Capabilities

None.

## Impact

- `internal/cache`: `ActionResult.Trace`; the target history record kind in `RecordStore`, with an atomic append
  rather than the whole-record replacement results and tags use, implemented by `Directory` and `Memory`,
  the only record stores, and held by `cachetest.Records`; a prune clears results, and keeps the history of every
  target built or reused in the last 90 days.
- `internal/tracing`: a stored, versioned span format and its size cap; conversion to Chrome JSON on retrieval.
- `internal/sched` and `internal/run`: recording spans whether or not a trace was asked for; writing the attempt's
  trace blob with its result; building the manifest; comparing actions to attribute misses.
- `internal/coord`: the job record names its manifest; `internal/worker` and attempts send spans unconditionally.
- `internal/graph`: each node names its configured target, which keys its history.
- `internal/api` and `internal/remote`: a job's outcome names its manifest; `internal/cli`: the client always
  records its spans and stores the command trace, `--trace` retrieves rather than enables, and `vamos trace`.
- Cost to watch: recording spans for every build, and storing a trace blob with every result, where today a build
  without `--trace` allocates nothing for tracing.
- Documentation: the trace transport section of `docs/design/scheduling.md`, `docs/design/storage.md`,
  the `--trace` reference in the user docs, and the doc.go of each package above.
- Depends on `workspace-name`, whose name keys target history.
