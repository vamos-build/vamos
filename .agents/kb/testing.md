# Validation and test conventions

Run from the repository root in the [development environment](development.md):

```sh
go build -o vamos .
go vet ./...
gofmt -l .                 # must print nothing
go test -short ./...       # skips image builds
go test ./...              # includes real buildah; needs root and network
```

For timeout planning, previous devcontainer runs took under one second for the short suite
and about 90 seconds for the full suite.
These are approximate timings, not deadlines: allow extra time for cold Go compilation, image pulls and emulation.

Run the full suite before finishing changes to building, committing or destinations.
Run [smoke-checks.md](smoke-checks.md) for changes to building, destinations, examples or the Dockerfile;
automated tests do not execute the built binary, example projects or release Dockerfile.
Changes to `install.sh` or the [release workflow](../../.github/workflows/release.yml) need
[smoke check 11](smoke-checks.md), which runs `shellcheck`, and `actionlint` for the workflow,
`go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7`, whose two SC2046 warnings on the merge job's
deliberate word splitting are expected; the workflow itself runs only in a release.
Changes to the Helm chart need `helm lint --strict remote/helm/vamos`, its rendering checked with
`helm template vamos remote/helm/vamos | kubeconform -strict -summary`, for the defaults and for any values the change
adds, and the chart's checks in [smoke-checks.md](smoke-checks.md#kubernetes) on kind.
Documentation-only changes need formatting, link checks,
Go doc inspection and the short suite rather than container smoke runs.

After changing an example, run its own checks using its README's setup.
For TypeScript UI run `pnpm check` and `pnpm build` in its directory,
and `./examples/typescript-ui/check.sh` from the repository root;
for TypeScript MCP this is `npm run check`;
for FastMCP activate `.venv`, then run `ruff check .`, `ruff format --check .`, `pyright` and `pytest`.
The other projects' guides are linked from [development.md](development.md).

What layers and tasks take in and give out is specified as [behavior specifications](#behavior-specifications).
Most other behavior tests drive the real CLI using `vamos` in [helpers_test.go](../../internal/cli/helpers_test.go).
Use `.succeeds(t)` and `.failsWith(t, usage, wants...)`; `usage` is true only for command-line mistakes.
Inspect resulting commands, files and tags.
Do not mock the core: push uses an in-process registry,
load uses a Unix-socket fake daemon and a real daemon when available.
Complex cache, graph and selection cases can also exercise their packages directly
on real implementations of the filesystem interface.
Every implementation of the stores runs `cachetest.Content` and `cachetest.Records`, and every executor
`apitest.Contract`: the directory and memory stores from the cache package's external tests, `run.Local` directly,
through JSON and through `remote.Client` and `remote.Server` in `TestContractHTTP`, which passes `Fixture.Buffered`
since a transport buffers events.
The remote package's own tests script an executor and inject dropped connections through the server's middleware.

Use `project(t, map[string]string{...})` for projects under `/project` in memory.
Use `diskProject` for symlinks or tests that write the project.
Do not add `testdata/` directories.
Workspace cases need `WORKSPACE` plus each package's BUILD file.
BUILD snippets start with `pullBusybox`; image commands usually name `app`.
Tests that build call `requireBuildah(t)`.
Every `TestMain` that uses buildah is `testenv.Main(m)`, which calls `buildah.InitReexec()` first
and `testenv.RemoveSuiteStores` after the tests; packages whose tests run a worker pass it `worker.ChildFromEnv`.
Table cases use keyed struct literals, one field per line.

On failure, check output absence with `assertNoFiles` and image cleanup with `assertNoNewImages`.
Successful dependencies remain cached even if a later target fails.
For path and metadata changes inspect [export_test.go](../../internal/finish/export_test.go)
and the end-to-end cases routed by [container-builds.md](container-builds.md).

Tests never touch the developer's container store or build cache.
`testenv.SuiteStores` makes a store and cache for the test binary, beside the default store, removed after the run;
`testenv.PrivateStores` makes a test its own, for one that destroys what they hold, as `TestPrune` does.
In `internal/cli`, `vamos` runs against the suite's stores unless the test called `usePrivateStores`.
Each run of the suite pulls `busybox:1.36` once into its fresh store, which Docker Hub counts against its pull limit,
so tests that need only a base to add files to push one to an in-process registry instead.
Use `newNonce` for cases that need fresh actions within a run; `cached` versus `building` is observable.
Tests that need buildah call `requireBuildah(t)`; those that change owners call `testenv.RequireOwnership`.
Both skip where the capability is missing, by probing it, unless `VAMOS_REQUIRE_INTEGRATION` is set,
which makes them fail instead, for a host that must run the whole suite.
The buildah probe needs root and makes and mounts a container from scratch in the suite's store;
the ownership probe needs root, as exports do, and a filesystem that takes `chown`.
Test stores drop the default store's additional image and layer stores,
and are removed only once nothing under them is mounted.
`-short` still skips image builds.
The executor takes its registry settings from its environment, so a test whose executor reaches a registry
over plain HTTP marks it insecure with `testenv.InsecureRegistry`, or `insecureRegistry` in `internal/cli`,
which write a `registries.conf` and call `sysregistriesv2.InvalidateCache`,
because containers/image caches registry configuration.
Trace importer coverage skips without `trace_processor_shell`.

## Behavior specifications

[features](../../features/) specifies the behavior of layers and tasks in Gherkin, one `.feature` file for each,
and godog runs them through `TestFeatures` in [features_test.go](../../features/features_test.go).
Scenarios run the command line in process, through `cli.NewRootCommand`, on an in-memory project and the suite's stores.
They observe only what a user can:
the command's output and error, the image it saved, the folder it exported and the images left in the store.
Observe a layer's image with `I save` and a task's files with `I export`.
Add a behavior as a scenario under the `Rule` it illustrates;
a rule states one sentence of the contract, in the words of the [BUILD reference](../../docs/reference/build.md).
Use a Scenario Outline when cases differ only in short values, titled `<case>`, a column that names each row:
each row runs as a subtest named by the title with the row's values.
Keep in Go tests the cases that need a registry, owners, paths that must not exist on the host
or long tables of expansions, such as `TestSteps` and `TestTaskOutputLinks`,
and the mistakes that loading a BUILD file catches.

Steps are methods of `world` in [steps_test.go](../../features/steps_test.go),
the state of one scenario, which `InitializeScenario` makes for each.
Reuse a step before adding one, and phrase a new one as what a user does or sees, not as how it is checked.
Steps return errors, rather than failing a `*testing.T`, so that godog reports the step and its line.
A scenario's BUILD file is a doc string, appended to the pull of `Given a project that pulls busybox`;
delimit it with ```` ``` ```` when it holds Starlark's `"""`.
Each non-blank line of `it fails with`'s doc string must appear in the error.
Table cells write a newline as `\n`,
and `{{nonce}}` becomes a value new to each scenario, for actions that must run rather than come from the cache.
Tag a feature or scenario `@buildah` when it builds:
`TestFeatures` leaves those scenarios out with `-short` or where buildah can't build,
and fails if `VAMOS_REQUIRE_INTEGRATION` is set.
It reports itself skipped when that leaves no scenario to run, and fails when `-godog.tags` matches none.

godog's options are flags prefixed `godog.`:
`go test ./features -godog.tags=@task` runs the task scenarios,
though not `-godog.concurrency` above 1, since scenarios share the suite's container store,
and godog prints each step as it runs, in its default `pretty` format;
`-godog.no-colors` drops the colors and `-godog.format=progress` prints a character a step.
`-run 'TestFeatures/<scenario>'` selects one scenario, though godog's summary then counts the others as undefined.
The devcontainer's `alexkrechik.cucumberautocomplete` extension links steps to their definitions,
which the workspace's `cucumberautocomplete.steps` setting locates.
