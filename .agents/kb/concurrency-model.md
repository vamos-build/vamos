# Concurrency, callbacks and ownership

The design is explained, with diagrams, in [docs/design/scheduling.md](../../docs/design/scheduling.md):
the scheduler's walk and bound, shared attempts, jobs, demand and expiry, cancellation, trace transport
and the cache directory's lock.
Leases and fencing on workers are in [docs/design/remote-execution.md](../../docs/design/remote-execution.md).
This file says where the contracts live and which tests hold them.

| Subject | Owners | Tests |
| --- | --- | --- |
| Dispatch, bound and failure order | `walk` in [sched](../../internal/sched/doc.go), `Options.Bound` | [scheduler_test.go](../../internal/sched/scheduler_test.go) |
| Attempts shared by key, forced rebuilds, record writes | `sched.Sharing`, `sharing.publish` | `TestSharing*` in [share_test.go](../../internal/sched/share_test.go) |
| Coordinator attempts, leases, kills, prune waiting | `pool` and `pool.mu` in [coord](../../internal/coord/doc.go) | [coord_test.go](../../internal/coord/coord_test.go) |
| Worker fencing | [worker](../../internal/worker/doc.go) attempt.go | `TestWorkerFencesItself` |
| Demand, grace period, eviction | `sched.Jobs`, `sched.Withdrawn`, `Jobs.Sweep`, `sched.Demanded`, `sched.Permitted` | `TestJobsExpire`, `TestBuildExpires`, `TestExpiredPendingAttemptEnds` |
| Event retention and slow watchers | `sched.Job`, `defaultRetention`, `api.ErrFellBehind` | [events_test.go](../../internal/sched/events_test.go), `apitest.Contract` |
| Directory lock and prune exclusion | `cache.Directory`, `Directory.Prune`, lock.go | `TestDirectoryLock*` in [lock_test.go](../../internal/cache/lock_test.go) |
| Commit and discard of pending files | `cache.pendingFile`, `cache.LogRecorder` | [cache_test.go](../../internal/cache/cache_test.go) |

`walk` alone writes the scheduler's maps, between dispatches; what it dispatches reads only its request and the stores.
`pool.mu` guards workers, attempts and the pending queue, and nothing is held while a worker's events are relayed.
Parallel targets within one local job are still sequential; that is a separate decision from running jobs at once.

Concurrency contracts belong with their symbols, not in a parallel list of locks here.
The relevant owners are `sched.Events`, `spanBatch`, `blobTracker`, `cache.LogRecorder`,
`tracing.Recorder`, `tracing.Active`, `cli.eventPrinter` and `cli.traceSession`.
Their files are routed by the corresponding package `doc.go` files.
Changes to callbacks must inspect both caller and consumer: data may be borrowed, callbacks may arrive in parallel,
and invoking user code while holding an internal lock changes the deadlock and latency behavior.
Do not infer that event consumers are single-threaded from the target scheduler.

Each job must finish producing events before its final flush and terminal event.
Inspect `forCall` and `transfer.Image` when adding asynchronous work;
a context passed to a local method is not a remote trace protocol.
Tracing should do no trace-only computation when disabled; guard argument preparation with `tracing.Enabled`.
Track nesting, flow endpoints and timestamp encoding are owned by [tracing](../../internal/tracing/doc.go).
The trace feature's `ThenPerfettoReadsTheTraceAsVamosMeansIt`, in
[steps_trace_test.go](../../internal/features/steps_trace_test.go), verifies the importer's view,
beyond checking the JSON ourselves.
