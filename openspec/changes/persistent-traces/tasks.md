# Tasks

## 1. Stored spans (`internal/tracing`)

- [x] 1.1 Add the stored span encoding with its version and `kind`, and `MaxTraceSize` (16 MiB). Make a recorder count
  each span's encoded size as it ends, keep at most twice the cap, and refuse and count spans past that. Add the trim
  that fits a trace into the bytes it is given by dropping spans latest-started first, and adds one `trace truncated`
  instant, with the count of spans dropped while recording and trimming, on the track of the earliest-started span lost,
  leaving room for it. Table-test round trips, the recording limit, that a trim drops children before their target
  span and keeps the first steps, the instant's track and count, and that a trimmed trace always fits. Verify with
  `go test ./internal/tracing`.
- [x] 1.2 Add `WithRecorderAt(ctx, r)`, which records ctx's later spans in `r` and keeps ctx's parent span and track.
  Test that spans started from it nest and lie as ctx's would. Verify with `go test ./internal/tracing`.
- [x] 1.3 Update `internal/tracing/doc.go` for stored traces, the cap, the trim and `WithRecorderAt`. Verify with
  `go doc ./internal/tracing` and the documentation checks in `.agents/kb/documentation.md`.

## 2. Results, provenance and target history (`internal/cache`)

- [x] 2.1 Add `ActionResult.Trace` and `Provenance.Started` and `Took`, validated, and documented as no part of any
  key. Show that a result without them still decodes. Verify with `go test ./internal/cache/...`.
- [x] 2.2 Add `History` (`Newest`, `Used`, `Entries`, `Trees`, encoded in that order), `HistoryEntry` and the trees they
  share, with `Validate`, `MaxHistorySize` and `HistoryRetention`, 100 days. Adding drops the oldest entries until the
  record fits, and keeps an oversized tree by digest alone. Unit-test the cap and the oversized tree. Verify with
  `go test ./internal/cache`.
- [x] 2.3 Add `RecordStore.History`, `Newest`, which decodes only the record's leading fields, `AddHistory` (take the
  entry's fingerprint as the newest, put the entry first unless it is too large to keep, drop the earlier entry with
  the key, keep at most `keep`, drop unused trees, budget the whole record, set `Used`), and `PruneHistory(ctx,
  before)`, which removes records whose `Used` is before it, keeps going past a record it fails to remove, and returns
  the count and the failures joined. Implement them in `Memory` under its mutex, and in `Directory` as
  `history/<name>`, published by rename, with adds and prunes under one history lock: an in-process mutex, then an
  exclusive `flock` on `history/lock`, taken without blocking and retried up to 10 seconds, mutex wait included, or
  the context's end. Make sure `ClearResults` leaves `history/`. Verify with `go test ./internal/cache/...`.
- [x] 2.4 Extend `cachetest.Records`: concurrent adds to one name all survive, `keep` holds, keys and trees
  deduplicate, the cap evicts the oldest entries, a newest entry with two large trees keeps its key with its trees by
  digest, an entry too large even then adds no entry but becomes the newest fingerprint and moves `Used`, `Newest`
  follows adds, `ClearResults` keeps history, and `PruneHistory` removes only records whose `Used` is before its cutoff.
  In `Directory`, test two processes sharing the directory (via `testenv`'s re-exec) that both add, a prune that keeps
  going past a record it fails to remove, through an injected remover since the tests may run as root, and an add that
  gives up at its bound or the context's end while another process holds the lock. Verify with
  `go test -race ./internal/cache/...`.
- [x] 2.5 Add `ContentStore.Stat(ctx, digest)`, which gives the blob held under a digest or `ErrMissing`, to
  `Directory`, `Memory` and `remote.Client`, with `remote.Server`'s endpoint, in one change so that every package
  builds. Extend `cachetest.Content` to cover it, and that it never drops a blob, which `TestContractHTTP` runs through
  the remote store. Verify with `go build ./... && go test ./internal/cache/... ./internal/remote`.
- [x] 2.6 Update `internal/cache/doc.go`, the storage design (`docs/design/storage.md`: the record store's class
  diagram, `ActionResult.Trace`, the history record, its lock, and that a prune keeps it until it goes unused) and
  `.agents/kb/storage.md`'s tables. Verify with the documentation checks in `.agents/kb/documentation.md`.

## 3. Target identity in the graph (`internal/graph`, `internal/load`)

- [x] 3.1 Add `graph.Node.Target`: the canonical label, platform and every parameter's value. Document it as
  metadata that no key reads, and say why `graph.Version` stays at 4. Verify with `go test ./internal/graph`.
- [x] 3.2 Set `Target` from `confLabel` for pulls, layers and tasks in `load`. Sort the parameters by name. Test that
  one target gets the same `Target` whether its platform and parameters are given or defaulted, and whatever order its
  parameters are declared in, and different ones across configurations. Verify with `go test ./internal/load`.

## 4. Job manifests and outcomes (`internal/api`, `internal/remote`)

- [x] 4.1 Add `api.JobTrace` (own spans, and per node its outcome, cause, attempt ID and the attempt trace it took, or
  none if it left the attempt running, with a count of entries dropped at the cap) and `api.CommandTrace` (client
  spans, offset, manifests), and `Outcome.Trace`, and `api.Pruned.Histories` and `Warnings`, which `Outcome.Clone`
  copies deeply. Document `Request` `Trace` and `EventSpans` as no longer used. Round-trip them in `api_test.go` and
  through `apitest.JSON`, and test that a clone's warnings are its own. Have `JobTrace.Save` fit spans beside entries
  with 1.1's trim, and test entries and spans that fit apart but not together, and entries that alone pass the cap.
  Verify with `go test ./internal/api/...`.
- [x] 4.2 Add `api.Executor.Trace(ctx, id)`, served by `remote` and answered by `run.Local` for its held jobs. In
  `coord`, keep the manifest's blob in the journal's tombstone and answer from it for evicted jobs. Make `sched.Jobs`'
  memory of evicted jobs map each ID to its manifest's blob, restored by `RememberEvicted`. Test what needs no stored
  manifest: the answers for running, unknown and manifest-less jobs, and that the evicted map and tombstone carry a
  blob. 4.2's tests of a stored manifest by job ID are in 5.6, since jobs save manifests only from 5.1. Verify with
  `go test ./internal/api/... ./internal/remote ./internal/coord ./internal/run`.

## 5. Always-on recording and attempt traces (`internal/sched`, `internal/run`, `internal/coord`, `internal/worker`)

- [ ] 5.1 Replace `sched.ForCall` with `sched.ForJob`, which always records and returns a `JobTrace` that saves the
  manifest. Call it from every non-prune job of `run.Local` and the coordinator, saving before the outcome is returned,
  under `context.WithoutCancel` with a 10-second timeout. Stop emitting `EventSpans`. A manifest that can't be saved
  leaves the outcome naming none and the job's outcome unchanged. Test both, and that a cancelled job's outcome names a
  stored manifest. Verify with `go test ./internal/sched ./internal/run ./internal/coord`.
- [ ] 5.2 In `Sharing.attempt`, give every attempt, resolves included, its own recorder through `WithRecorderAt`, and a
  random 128-bit ID that its trace and its `cache write` span carry. Write its capped trace when the runner returns,
  under `context.WithoutCancel` of the attempt's context with a 10-second timeout, apart from `publish`'s context; set
  `Result.Trace` and `Provenance.Started`/`Took` before `SetResult`; and tell every job that waits on `done`, with
  `publish`'s `cache write` span, through a `Traced` interface. Tell a job that detaches while the attempt runs on the
  attempt's ID alone. Remove `tracing.Revocable`. In `share_test.go`, test that a failed attempt's trace is written,
  that starters and joiners get the same blob and the `cache write` span, that a starter that detaches gets the ID and
  no blob while the joiner gets the whole trace, that a lone cancelled job gets the final trace even from a content
  store that refuses a cancelled context, that a job attached while `SetResult` is held up gets the same blob, and that
  a trace write that fails still records the result. Verify with `go test -race ./internal/sched ./internal/tracing`.
- [ ] 5.3 Add `sched.Report.Ran`, the interval of the run that made the result, which `Took` and `Started` prefer to the
  attempt's own. Set it in the local runner and, in the pool, from the successful try's assignment to its report. Test
  in `coord_test.go` that waiting for a worker and a failed try aren't counted. Verify with
  `go test ./internal/sched ./internal/coord ./internal/run`.
- [ ] 5.4 Make `targetOutput` and resolves' outputs record each node's outcome, attempt ID and attempt trace in the
  job's `JobTrace`. Set the hit span's `made`, `platform`, `took`, `saved` and `attempt trace` arguments from
  `Provenance` without reading the blob. Keep `Trace` out of the checkers and `lookup`. Test in `scheduler_test.go`.
  Verify with `go test ./internal/sched`.
- [ ] 5.5 Make `pool.NextAssignment` always send a `Trace`, so workers always record. Check in `coord_test.go` that a
  worker's spans, retries included, land in the attempt's trace on the coordinator's clock. Verify with
  `go test ./internal/coord ./internal/worker`.
- [ ] 5.6 (Moved from 4.2 and 4.3, which need 5.1's manifests.) Test in `restart_test.go` that a released, evicted
  job's trace still answers by its ID after a restart that kept the state, and a released job's trace by ID on a
  coordinator without `Config.State`. Extend `apitest.Contract`: every graph, push, export and publish job's outcome
  names a manifest that the executor's content store holds, which `Trace` gives too, a prune's names none, two jobs
  that share an attempt name the same attempt trace, and a cold pull's manifest names its resolve's attempt trace.
  Verify with `go test ./internal/run ./internal/remote ./internal/coord`.
- [ ] 5.7 Update the doc.go of `sched`, `run`, `coord` and `worker`, the trace transport section and its sequence
  diagram in `docs/design/scheduling.md`, and the owners in `.agents/kb/concurrency-model.md`. Verify with the
  documentation checks.

## 6. Target history and miss attribution (`internal/sched`)

- [ ] 6.1 Add `sched.Options.Workspace`, set by `run.Local` and the coordinator from `GraphRequest.Workspace`, and
  `sched.HistoryName`. Build a `HistoryEntry` from the action and its suppliers: each one's `Target`, a pull's image
  reference and manifest digest too, and each source's tree the record doesn't already hold, read with `ReadTree`.
  Verify with `go test ./internal/sched`.
- [ ] 6.2 Implement `attribute` as the pure function design.md orders: `forced` (`--rebuild`), `lost` (unusable results
  and regenerations included), `new`, then the nearest entry of the current `ActionVersion` by distance, ties to the
  most recent, then `own-inputs`, `upstream`, `dependency`, all derived from one structured difference. Table-test every
  cause, the nearest entry, ties, entries of another version, steps that differ in count, an output directory change, a
  pull's edited tag (`dependency`) against its moved tag (`upstream`), a moved pull alongside a changed task
  (`dependency` naming both), a base switched to another pull (`own-inputs`), a source kept by digest alone, and files
  added, removed and changed, mode included. Count the kind (layer or task) as an own input, and test a layer that
  became a task, and a regeneration, `lost` and not `forced`, whose miss replaces the target's hit in the manifest.
  Verify with `go test ./internal/sched -run Attribute`.
- [ ] 6.3 In `runCached`, read history once on a miss and set `miss` and its arguments on the span and in the
  `JobTrace`, capping each file list at 100 with a count. Add the entry after success; on a hit, read `Newest`, and add
  only if the hit's fingerprint, key and suppliers, isn't the newest or `Used` is more than 7 days old. Treat a history
  failure as a span error. Test in `scheduler_test.go` that a fully cached build reads `Newest` once per target and
  adds nothing, that a hit on a record used over 7 days ago refreshes it, that a broken history store fails no target,
  that a failed target, a cancelled one, and a job that left a shared attempt which later succeeded for another job add
  nothing, a base switched to a layer making the same image, then changed, attributed as `dependency`, a pull's tag
  moved to another manifest of the same image, which a hit records, then to another image, whose `upstream` names the
  second manifest as `before`, and two workspaces with different histories sharing one attempt, which get their own
  causes and the same attempt trace. Verify with `go test ./internal/sched`.
- [ ] 6.4 Have both executors' prune jobs call `PruneHistory` with a cutoff of now less `cache.HistoryRetention` right
  after `ClearResults`, the coordinator's before its cache repository's tags. Fill `api.Pruned.Histories` and
  `Warnings` from it, and have the client print the count with the others and each warning. Test that both executors
  keep a recent record and one built long ago but reused recently, remove an unused one and count it, and that a
  history prune that fails partway still counts what it removed, lets the coordinator clear its tags, and warns in the
  outcome, which the client prints. Verify with `go test ./internal/run ./internal/coord ./internal/cli`.
- [ ] 6.5 Document target history, its retention and attribution in `internal/sched/doc.go` and in
  `docs/design/storage.md`'s identities. Verify with the documentation checks.

## 7. The client: command traces, `--trace` and `vamos trace` (`internal/cli`)

- [ ] 7.1 Make `inv.traced` always record. Measure the clock offset with one sample without `--trace` and three with it.
  Once a command's jobs end, and before it releases them, store the `CommandTrace` in the executor's content store and
  print `vamos: trace <digest>` on stderr, warning instead if it can't be stored. Store it, assemble `--trace`'s file
  and release the jobs under `context.WithoutCancel` of the command's context, bounding storing and releasing at 10
  seconds each and assembly at 60. Test that an interrupted build stores its trace and writes its `--trace` file with
  the executor's spans, that a prune on a coordinator is refused while the command trace is stored and its file
  assembled and runs once the jobs are released, and that each cleanup step against an executor that stops answering
  ends at its bound. A command that ran no job stores nothing. Warn about a clock offset that can't be measured,
  storing a zero offset marked unknown, rather than failing the command, and test it. Warn when a job's outcome names
  no manifest. Verify with `go test ./internal/cli`.
- [ ] 7.2 Assemble traces in `cli/trace.go`: manifests and the attempt traces they took with `ReadMany`, falling back to
  one read per blob when a batch meets a missing or corrupt one, with gaps for those. Place joined attempts on their own
  `(shared)` track with parents cleared, and shift client spans by the stored offset. Place an attempt that several of
  a command's jobs share once, by its ID, under the starter if it took the trace and otherwise on the first such job's
  `(shared)` track, with one `cache write` span; an attempt no job took is a gap saying the job left it running. Make
  `--trace` write from the stored command trace, from memory if storing failed, or the client spans alone if no job
  ran, before the jobs are released. Test mixed missing and present, and corrupt and present, batches, an attempt that
  one job of a command started and another joined, with and without the starter taking it, a join that starts
  mid-step, an attempt two nodes of one job share, placed once under the starter with each node's own outcome and
  cause, read by Perfetto, and that `--trace` writes every span when only storing the command trace fails, for a
  command that succeeds and one that fails, with the command's outcome unchanged. Verify the existing `TestTrace_*`
  scenarios pass unchanged with `go test ./internal/features -run TestTrace_`.
- [ ] 7.3 Add `vamos trace <digest|job> <file.json>`: take an argument holding a `:` as a digest, failing on one that
  doesn't parse, and read it through `Stat`, failing as no trace if its size passes `MaxTraceSize`, then by `kind`; read
  a job ID only with `--coordinator`, through `Executor.Trace`, with messages for running, expired and unknown jobs;
  fail, naming the digest and writing nothing, on a blob that isn't a stored trace or whose `kind` or version is
  unknown; check and write the output as `--trace` does. Test each message in `internal/cli`, the oversized blob's, a
  log's, malformed JSON's and an unknown kind's and version's included, and a content store that refuses the command
  trace, against a remote coordinator. Verify with `go test ./internal/cli`.
- [ ] 7.4 Add steps for a printed trace digest, `vamos trace` by digest and by job, and of a step's log, an attempt
  trace removed from the cache, and a step that checks a span lacks named arguments, for cached targets' causes and old
  results' provenance. Add `trace_test.go` rules and scenarios for every `build-traces` requirement, the secret one
  checking the stored command trace, manifest and attempt trace as well as the file, the coordinator ones in the remote
  steps. Verify with `go test ./internal/features -run TestTrace_`.
- [ ] 7.5 Update the user docs: `docs/user/reference/cache.md#traces` (always on, what is stored, prune),
  `flags.md`'s `--trace`, a new `docs/user/reference/cli/trace.md` with its row in the CLI README table, `output.md`
  (the trace line), and `prune.md` (traces go, history stays until unused). Update `internal/cli/doc.go`. Make the
  feature rules' doc comments quote them. Verify with the documentation checks.

## 8. Miss attribution scenarios (`internal/features`)

- [ ] 8.1 Add `misses_test.go` with a background for a named workspace, and a rule for each `cache-miss-attribution`
  requirement, with its scenarios, checked through `ThenTheTraceSpanSaysAtLeast`, and through 7.4's absence step where a
  scenario says no cause. Add a step for each new setup the scenarios need (moving a tag under `--pins=resolve`, two
  builds at once on a coordinator, and ageing a target's history by setting its record's `Used` back, for the
  retention scenarios). Document the causes in `docs/user/reference/cache.md` and have the rules quote it. Verify with
  `go test ./internal/features -run TestMisses_`.

## 9. Integration

- [ ] 9.1 Add a scheduler benchmark that runs `BenchmarkSchedulerCached`'s graph as a job would, recorded, in a named
  workspace with targets, so that history is read, reporting allocations. Run it and `BenchmarkSchedulerCached` before
  and after the change, and record any regression in the PR. Add a benchmark in `internal/cli` that assembles a command
  trace naming many attempt traces near the cap, reporting allocations and its peak heap, sampled from `runtime/metrics`
  while it runs and reported through `b.ReportMetric`, and record its result in the PR. Add a benchmark of a cold build
  of a dependency chain against a content store that delays writes, and record how much storing attempt traces adds to
  it. Verify by comparing `go test -bench . -benchmem ./internal/sched ./internal/cli` output.
- [ ] 9.2 Run `scripts/check` and then `scripts/check full`, and confirm both pass.
- [ ] 9.3 Run the build smoke checks in `.agents/kb/smoke-checks.md`, open a stored trace from `vamos trace` in
  Perfetto, and run the Kubernetes checks, so that a coordinator's job trace is read by job ID.
- [ ] 9.4 Run `openspec validate persistent-traces --strict` and confirm it passes.

## Workflow follow-up

- Archive the change with `/opsx:archive` once it is reviewed and merged, creating `openspec/specs/build-traces` and
  `openspec/specs/cache-miss-attribution`.
- `telemetry-export` builds on the manifests and miss causes added here.
