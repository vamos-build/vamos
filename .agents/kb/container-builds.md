# Container build and delivery lifecycle

The design is explained, with diagrams, in [docs/design/container-builds.md](../../docs/design/container-builds.md):
the build lifecycle, result kinds, the execution and run platforms, emulation and binfmt_misc, and delivery.
This file says where to change it and which tests guard it.

Source bytes used by execution must be the cached bytes used to identify its inputs:
never optimize an `add()` by reading the live project again.
Regression coverage is the scenario of every target that adds a glob getting its files as they were first read,
in `TestGlob_ReadBeforeExecution` in [glob_test.go](../../internal/features/glob_test.go),
and that of a file edited while the build reads it, cached under the key of what was read,
in `TestCache_ChangedInputsRunAgain` in [cache_test.go](../../internal/features/cache_test.go).

## Platforms

- Propagation and destination selection: `workspace.edge` and `workspace.imageRoot` in [load](../../internal/load/doc.go).
- Choice of execution platform: `invocation.execPlatform` in [cli](../../internal/cli/doc.go).
- Where an action runs: `Scheduler.runPlatform`, `sched.Options.Native`, `sched.Options.Offered`.
- Emulation permission: `sched.Scheduler.emulationRefused`, `sched.Options.OptIn`, `api.GraphRequest.Emulate`.
- Platform checks, library contexts and diagnostics: `setPlatform`, `forPlatform` and `emulatorHint`
  in [run](../../internal/run/doc.go);
  the buildah error-reporting limitation is documented beside `emulatorHint` in [runner.go](../../internal/run/runner.go).
- Emulators a worker offers: `emulatedPlatforms` in [worker](../../internal/worker/doc.go);
  which worker gets emulated work: `pool.suits` in [coord](../../internal/coord/doc.go).

An empty or unmounted `/proc/sys/fs/binfmt_misc`, as in the devcontainer, does not mean that nothing emulates,
and mounting it fixes no `exec format error`:
to learn whether a platform is emulated, run one of its programs, as the scenarios behind `GivenThisMachineEmulates` do.

## Delivery

For file destinations, inspect `Save`, `Export`, `install`
and `swap` in [finish](../../internal/finish/doc.go) before changing preflight or installation.
Publications share `transfer.Upload` and `transfer.Tag` between the executor's job, `run.Local`'s or a coordinator's
on its workers, and the client's `finish.Publish`.

Transport details belong beside their owners:
`Image.open`, `copyTo` and `LoadDocker` in [finish.go](../../internal/finish/finish.go), and `Publish` in
[publish.go](../../internal/finish/publish.go),
which deliver an image from the registry its result locates it at, an index assembled from its members' locations,
or an export in the content store, through an OCI layout;
`export` in [executor.go](../../internal/run/executor.go)
and `sendResult` in [publish.go](../../internal/run/publish.go), which publish an image's blobs, or push or upload them;
`Image`, `Push`, `Upload`, `Tag`, `Policy`, `Layout` and `LayoutIndex`
in [transfer](../../internal/transfer/doc.go),
which both sides copy with; and the registry settings,
`run.SystemContext` for the executor's environment and `invocation.systemContext` in
[cli.go](../../internal/cli/cli.go) for the client's, with `--tls-verify`.
Construct image references with transport packages;
preserve the system signature policy
and the distinction between an unset TLS override and explicit `--tls-verify=false`.

## Host and container paths

For host/container path isolation, start with `cache.Checkout`,
`fileset.Walk`, `targetState.addSource` and `collectOutput`.
Their symbol comments own link handling and directory metadata rules.
The end-to-end guards are the scenario of a task's source that is a link, in
[task_test.go](../../internal/features/task_test.go),
the rule that existing directories keep their modes and owners, in [add_test.go](../../internal/features/add_test.go),
the rule that output which can't be selected fails, in [task_test.go](../../internal/features/task_test.go),
and [export_test.go](../../internal/finish/export_test.go).
