# Storage, identity and reuse

The design is explained, with diagrams, in [docs/design/storage.md](../../docs/design/storage.md):
the three stores, the identities, action keys, image naming and locations, pins and tags, epochs and pruning.
This file says what to review when changing it and which tests hold it.

## Where to look

| Change | Review together |
| --- | --- |
| Execution semantics or what a key covers | [action.go](../../internal/sched/action.go), `Scheduler.inputs` and `Scheduler.lookup` in [scheduler.go](../../internal/sched/scheduler.go), `localRunner.Check` in [runner.go](../../internal/run/runner.go); raise `sched.ActionVersion` |
| Image naming | `CacheRepository`; `imageIdentity`, `locateImage`, `cacheName` and `nameImage` in [pull.go](../../internal/run/pull.go) |
| Tag resolution and freshness | `cache.TagRecord`, `cache.TagInstance`, `puller` in [pull.go](../../internal/run/pull.go), `Scheduler.resolve` |
| Reproducible timestamps | `Scheduler.epoch`, `commitLayer` and `sched.Step.Epoch` |
| Pruning policy | `Local.prune` in run, `Directory.Prune`, the stores' `ClearContent` and `ClearResults`, `pool.Report`, `coord.Registry.ClearRepository` |
| Record writes | `sched.Sharing`; the runner returns a `sched.Report` and the puller the tag record to write |

Only run and cache name the cache directory, `run.CacheDir`; a client gets its content store from its executor.
`graph.Version` addresses a different compatibility boundary from `sched.ActionVersion`.
Do not let a secret value reach anything encoded: values travel only in `api.GraphRequest.Secrets`
and `sched.Request.Secrets`.

## Tests that hold the design

| Rule | Tests |
| --- | --- |
| Unusable cached content: corrupt task files rerun, corrupt globs fail, corrupt logs miss | `TestCache_ResultsThatCantBeUsedAreBuiltAgain` |
| Image IDs survive a push and pull | `TestImageIdentityRoundTrip` |
| Names are checked against image IDs and layers, and store IDs differ from image IDs | `TestCache_CachedLayersAreFoundByTheirImageIDs` |
| A missing image is fetched from a registry location | `TestMaterializeFromRegistry` |
| Secrets in steps, image, trace and reuse | [secrets_test.go](../../internal/features/secrets_test.go), and `TestRequestSecretsNotEncoded` for the encoding |
| Timestamps: from bases and tasks, task epochs as inputs, `--timestamp` as a minimum | `TestCache_TimestampsComeFromBasesAndTasks`, `TestCache_AddedTasksTimestampIsAnInput`, `TestCache_TimestampIsAMinimumForTargetsThatRun` in [cache_test.go](../../internal/features/cache_test.go) |
| Cached resolves ask the runner nothing; legacy instances decode | `TestSchedulerCachedResolve`, `TestTagRecordLegacyInstances` |
| Nothing reaches a registry once the record and images are cached | `TestPull_ResolvedTagsAreReused` |
| Generation fencing, prune order, cache repository cleared | `TestGenerationFencing`, `TestPruneOrder`, `TestPruneClearsRepository` |

Pruning excludes every other use of the directory through its lock; see [concurrency-model.md](concurrency-model.md).
Shared-cache test isolation rules are in [testing.md](testing.md).
