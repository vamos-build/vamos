# Spec Delta

## Purpose

Lets a workspace keep tools' own caches, such as Go's build cache or a package manager's store, on the executor
between builds, so that an edit rebuilds incrementally, without what a cache holds ever changing what is built
or which results are reused.

## ADDED Requirements

### Requirement: The tool_cache rule declares a tool cache

A BUILD file SHALL declare a tool cache with `tool_cache(name = ..., path = ...)`, where `path` is the absolute
container path at which the cache is mounted unless a mount overrides it. `tool_cache` MAY also take `owner` and
`group`, and SHALL take no other argument. A mistake in a `tool_cache` call's arguments SHALL fail when its BUILD file
is evaluated, whether or not any target mounts the cache. A cache SHALL be referred to by label, from any package of the
workspace.

#### Scenario: Two packages share one cache

- **GIVEN** a workspace named `com.acme.shop` whose `tools` package declares
  `tool_cache(name = "go-build", path = "/cache/go-build")`
- **AND** a task `api` in package `api` and a task `cli` in package `cli`, each with
  `tool_caches = [mount("//tools:go-build")]`
- **WHEN** I build `//api:api` and then `//cli:cli`
- **THEN** both succeed and `cli`'s step sees what `api`'s step wrote to the cache

#### Scenario: A tool_cache rule needs a path

- **GIVEN** a BUILD file that declares `tool_cache(name = "go-build")`, and a task `app` that mounts no cache
- **WHEN** I build `app`
- **THEN** it fails naming the file, line and column of the call and the missing argument `path`

#### Scenario: A tool_cache rule names no variable

- **GIVEN** a BUILD file that declares `tool_cache(name = "go-build", path = "/cache/go-build", env = "GOCACHE")`
- **WHEN** I build any target of the package
- **THEN** it fails naming the file, line and column of the call and the argument `env`

### Requirement: A task or layer mounts caches for its run steps

`task` and `layer` SHALL take `tool_caches`, a list of `mount(label)` values, each naming a `tool_cache` rule and
optionally giving `path` to mount it somewhere other than the cache's own path. Each cache SHALL be mounted for every
`run()` step of the target and no `add()` step; loading refuses a target with caches and no `run()` step (see "Loading
refuses a cache path that conflicts"). vamos SHALL set no variable naming a mounted cache.

#### Scenario: A later build sees what an earlier one left

- **GIVEN** a workspace named `com.acme.shop` that declares `tool_cache(name = "tool", path = "/cache")`
- **AND** a task `probe` with `tool_caches = [mount("tool")]`, whose step lists `/cache` and then writes a file there
  named by a parameter
- **WHEN** I build `probe` with the parameter `one`, and then with `two`
- **THEN** the second build's step lists the file `one`

#### Scenario: A mount overrides the cache's path

- **GIVEN** a workspace named `com.acme.shop` that declares `tool_cache(name = "tool", path = "/a")`
- **AND** a task `writer` with `tool_caches = [mount("tool")]`, whose step writes `/a/marker`
- **AND** a task `reader` with `tool_caches = [mount("tool", path = "/b")]`, whose step reads `/b/marker`
  and checks that `/a` does not exist
- **WHEN** I build `writer` and then `reader`
- **THEN** both succeed

#### Scenario: A step is given no variable for its cache

- **GIVEN** a workspace named `com.acme.shop` whose task `probe` mounts a cache at `/cache` and reports its
  environment
- **WHEN** I build `probe`
- **THEN** no variable the step observed holds `/cache`

### Requirement: A tool cache is mounted, never built

A `tool_cache` rule SHALL be no target: every command that takes a target SHALL refuse its label, and a `base`,
`dep()`, `add()` or publication check that names it SHALL fail, each saying that a tool cache is mounted with
`mount()`.

#### Scenario: Building a tool cache

- **GIVEN** a workspace named `com.acme.shop` whose `tools` package declares
  `tool_cache(name = "go-build", path = "/cache/go-build")`
- **WHEN** I build `//tools:go-build`
- **THEN** it fails before anything runs, naming `//tools:go-build` and saying that a tool cache is mounted with
  `mount()`

#### Scenario: Updating a tool cache

- **GIVEN** a workspace named `com.acme.shop` whose `tools` package declares
  `tool_cache(name = "go-build", path = "/cache/go-build")`
- **WHEN** I run `vamos update //tools:go-build`
- **THEN** it fails naming `//tools:go-build` and saying that a tool cache is mounted with `mount()`

#### Scenario: A workspace-wide update passes over tool caches

- **GIVEN** a workspace named `com.acme.shop` that declares `tool_cache(name = "tool", path = "/cache")`, and whose
  `vamos.lock` pins a pull the BUILD files still use and one they no longer do
- **WHEN** I run `vamos update`
- **THEN** it succeeds, keeping the first pin, or bringing it up to date, and removing the second

#### Scenario: Depending on a tool cache

- **GIVEN** a workspace named `com.acme.shop` that declares `tool_cache(name = "tool", path = "/cache")`, and a task
  `app` whose step adds `dep("tool")`
- **WHEN** I build `app`
- **THEN** it fails as the BUILD file loads, naming `tool` and saying that a tool cache is mounted with `mount()`

### Requirement: What a cache holds is outside every key

A cache's contents, whether it holds anything, where the executor keeps it and the workspace's name SHALL NOT be part of
any action's key. The path each cache is mounted at, whether the cache's or the mount's, the cache's label, and its
owner and group SHALL be. Adding, removing or moving a target's mount SHALL change its key, so that the target runs
unless a result for that key is already cached.

#### Scenario: A changed cache does not cost a hit

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts a cache and has built
- **AND** another task, mounting the same cache, has since written to it
- **WHEN** I build `build` again
- **THEN** it succeeds and `build` was reused

#### Scenario: Moving a mount reruns the target

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts a cache at `/cache` and has built
- **WHEN** the BUILD file is changed to mount it at `/other` instead
- **AND** I build `build` again
- **THEN** it succeeds and `build` ran

#### Scenario: Overriding a cache's path with its own path keeps the hit

- **GIVEN** a workspace named `com.acme.shop` that declares `tool_cache(name = "tool", path = "/cache")`,
  whose task `build` has `tool_caches = [mount("tool")]` and has built
- **WHEN** the BUILD file is changed to `tool_caches = [mount("tool", path = "/cache")]`
- **AND** I build `build` again
- **THEN** it succeeds and `build` was reused

#### Scenario: Renaming the workspace reuses results

- **GIVEN** a workspace named `com.acme.first` whose task `build` mounts a cache and has built,
  and which no glob that selects `WORKSPACE` reaches
- **WHEN** its `WORKSPACE` is changed to name it `com.acme.second`
- **AND** I build `build` again
- **THEN** it succeeds and `build` was reused

### Requirement: An executor may give any cache empty

Every executor SHALL mount a directory at each of a target's cache paths, but MAY give any cache empty, and a build
SHALL succeed either way. A coordinator's workers SHALL give every cache empty. The local executor of a standalone
build, one not serving `vamos coordinator --local`, SHALL give each attempt it starts the workspace's cache as earlier
builds with the same owner, group and ID mappings left it since the last standalone prune, unless the build passes
`--no-tool-caches`.

#### Scenario: A remote build gives every cache empty

- **GIVEN** a coordinator with one worker, and a workspace named `com.acme.shop` whose task `probe` mounts a cache
  at `/cache`, lists it and writes a file there named by a parameter
- **WHEN** I build `probe` on the coordinator with the parameter `one`, and then with `two`
- **THEN** both succeed and neither build's step lists anything in `/cache`

### Requirement: A local coordinator gives every cache empty

The in-process executor of `vamos coordinator --local` SHALL give every cache empty, whatever workspace name a request
carries, and SHALL NOT refuse a request for its workspace name.

#### Scenario: A local coordinator keeps no cache between builds

- **GIVEN** `vamos coordinator --local`, and a workspace named `com.acme.shop` whose task `probe` mounts a cache at
  `/cache`, lists it and writes a file there named by a parameter
- **WHEN** I build `probe` on the coordinator with the parameter `one`, and then with `two`
- **THEN** both succeed and neither build's step lists anything in `/cache`

### Requirement: --no-tool-caches gives each target its own empty caches

With `--no-tool-caches`, each attempt the build starts SHALL be given its own empty caches, shared by its steps,
discarded when it ends and never seen by another target. A result, or a running attempt, that another build made with a
warm cache MAY still be reused.

#### Scenario: A clean rebuild reuses a dependency built warm

- **GIVEN** a workspace named `com.acme.shop` that declares `tool_cache(name = "tool", path = "/cache")`, and whose
  layer `deps` and task `app`, built on `deps`, each mount it; `deps`'s step writes `/cache/deps`, and `app`'s lists
  `/cache` and writes `/cache/app`; and that has built `app`
- **WHEN** I build `app` with `--no-tool-caches --rebuild`
- **THEN** `deps` was reused and `app` ran, and `app`'s step listed nothing in `/cache`

#### Scenario: A clean build gives every cache empty

- **GIVEN** a workspace named `com.acme.shop` whose task `probe` mounts a cache at `/cache`, lists it and writes
  a file there named by a parameter, and that has built `probe` with the parameter `one`
- **WHEN** I build `probe` with the parameter `two` and `--no-tool-caches`
- **THEN** the step lists nothing in `/cache`

#### Scenario: A clean build gives each target its own empty cache

- **GIVEN** a workspace named `com.acme.shop` whose layer `first` mounts a cache at `/cache` and writes `/cache/marker`,
  and whose task `second`, based on `first`, mounts the same cache at `/cache` and lists it
- **WHEN** I build `second` with `--no-tool-caches`
- **THEN** `first` and `second` ran, and `second`'s step lists nothing in `/cache`

#### Scenario: A clean build keeps the cache for later builds

- **GIVEN** a workspace named `com.acme.shop` whose task `probe` mounts a cache at `/cache`, lists it and writes
  a file there named by a parameter, and that has built `probe` with the parameter `one`
- **WHEN** I build `probe` with the parameter `two` and `--no-tool-caches`
- **AND** I build `probe` with the parameter `three`
- **THEN** the last build's step lists the file `one` and not `two`

#### Scenario: An attempt's steps share an empty cache

- **GIVEN** a workspace named `com.acme.shop` whose task `probe` mounts a cache at `/cache`, and whose first `run()`
  step writes `/cache/marker` and second step reads it
- **WHEN** I build `probe` with `--no-tool-caches`
- **THEN** it succeeds

### Requirement: A cache's contents never enter a layer's image

What a `run()` step writes through a cache's mount, at the location its path resolved to, SHALL stay in the cache and
SHALL NOT be part of the layer's image, whether the cache was given empty or not. The image SHALL hold an empty
directory at each cache path.

#### Scenario: A layer's image keeps no cache contents

- **GIVEN** a workspace named `com.acme.shop` whose layer `tools` mounts a cache at `/cache` and whose step writes
  `/cache/big` and `/opt/kept`
- **WHEN** I build `tools`
- **THEN** its image holds `/opt/kept` and an empty directory `/cache`

### Requirement: An executor resolves each cache path before the steps and holds it

The executor SHALL resolve each cache path in the container's root filesystem before an attempt's steps, without leaving
it, make an empty directory there before the first `run()` step if none is, and mount the cache there for every `run()`
step. Before each `run()` step and after the last step, it SHALL fail the attempt, naming the path, if the path resolves
elsewhere or through `/dev`, `/proc` or `/sys`, or its location is no longer an empty directory with the cache's owner
and group.

#### Scenario: A cache path through a symbolic link

- **GIVEN** a workspace named `com.acme.shop` whose layer `tools` mounts a cache at `/alias/cache`, from an image
  in which `/alias` is a symbolic link to `/real`, and whose step writes `/alias/cache/file`
- **WHEN** I build `tools`
- **THEN** it succeeds and its image holds an empty directory `/real/cache`

#### Scenario: A step that moves a cache path

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts a cache at `/alias/cache`, from an image
  in which `/alias` is a symbolic link to `/real`, and whose first of two `run()` steps points `/alias` at `/other`
- **WHEN** I build `build`
- **THEN** it fails naming `build` and `/alias/cache`, and its second `run()` step did not run

#### Scenario: A step that points a cache path through /dev

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts a cache at `/alias/cache`, from an image in
  which `/alias` is a symbolic link to `/real` and `/dev/hidden` is a symbolic link to `/real`, and whose first of two
  `run()` steps points `/alias` at `/dev/hidden`
- **WHEN** I build `build`
- **THEN** it fails naming `build` and `/alias/cache`, and its second `run()` step did not run

#### Scenario: A workdir() beneath a cache

- **GIVEN** a workspace named `com.acme.shop` whose layer `tools` mounts a cache at `/cache`, from an image in which
  `/alias` is a symbolic link to `/cache`, and whose steps are a `run()` and then, last, `workdir("/alias/sub")`
- **WHEN** I build `tools`
- **THEN** it fails naming `tools` and `/cache`

### Requirement: An executor refuses a cache path it cannot mount cleanly

The executor SHALL fail the attempt, naming the paths, before its steps when a cache's resolved location exists as
anything but an empty directory or equals, contains or lies beneath another's, and just before its first `run()` step
when the location is anything but absent or an empty directory that was there before the steps, or cannot be created.

#### Scenario: The image already holds files at a cache path

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts a cache at `/cache`, from an image
  that holds the file `/cache/old`
- **WHEN** I build `build`
- **THEN** it fails naming `build` and `/cache`, and no step ran

#### Scenario: Two cache paths that one symbolic link joins

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts one cache at `/alias/cache` and another
  at `/real/cache`, from an image in which `/alias` is a symbolic link to `/real`
- **WHEN** I build `build`
- **THEN** it fails naming `build`, `/alias/cache` and `/real/cache`, and no step ran

#### Scenario: A step that makes a directory at a cache path

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts a cache at `/src/.cache`, from an image whose
  environment sets `CACHE=/src/.cache`, with the steps `workdir("$CACHE")`, `workdir("/src")` and a `run()`
- **WHEN** I build `build`
- **THEN** it fails naming `build` and `/src/.cache`, and the `run()` step did not run

### Requirement: An executor refuses a cache path that buildah's own mounts overlap

The executor SHALL fail the attempt before its steps, naming the paths, when a cache's resolved location is at or
beneath `/dev`, `/proc` or `/sys` or is reached through one of them, or equals, lies beneath or contains where
`/etc/hosts`, `/etc/hostname`, `/etc/resolv.conf`, `/run/.containerenv`, `/run/secrets`,
`/etc/crypto-policies/back-ends`, `/etc/crypto-policies/config` or a volume the image declares resolves.

#### Scenario: A cache path that a symbolic link puts beneath /dev

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts a cache at `/alias/cache`, from an image in
  which `/alias` is a symbolic link to `/dev/shm`
- **WHEN** I build `build`
- **THEN** it fails naming `build` and `/alias/cache`, and no step ran

#### Scenario: A file every step is given that a symbolic link puts in a cache

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts a cache at `/cache`, from an image in which
  `/etc/hostname` is a symbolic link to `/cache/hostname`
- **WHEN** I build `build`
- **THEN** it fails naming `build`, `/etc/hostname` and `/cache`, and no step ran

#### Scenario: A volume the image declares beneath a cache

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts a cache at `/cache`, from an image that
  declares the volume `/cache/data`
- **WHEN** I build `build`
- **THEN** it fails naming `build`, `/cache` and `/cache/data`, and no step ran

### Requirement: An executor refuses a path that passes through a cache

Before the steps, before each `run()` step and after the last step, the executor SHALL fail the attempt, naming the
paths, when the resolution of a cache path, the working directory, a secret's file, a file buildah binds into a step or
a volume the image declares passes through a cache's location, its own included, before it reaches its end.

#### Scenario: A cache path reached through another cache

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts one cache at `/first` and another at
  `/alias`, from an image in which `/alias` is a symbolic link to `first/entry/../../second`
- **WHEN** I build `build`
- **THEN** it fails naming `build`, `/alias` and `/first`, and no step ran

#### Scenario: A step that re-links a cache path through another cache

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts one cache at `/first` and another at
  `/alias`, from an image in which `/alias` is a symbolic link to `/second`, and whose first of two `run()` steps
  points `/alias` at `first/entry/../../second`
- **WHEN** I build `build`
- **THEN** it fails naming `build`, `/alias` and `/first`, and its second `run()` step did not run

### Requirement: An executor refuses a symbolic link at /dev, /proc or /sys

The executor SHALL fail an attempt that mounts caches, naming the path, when `/dev`, `/proc` or `/sys` is a symbolic
link before its steps or before any `run()` step. An attempt that mounts no cache SHALL NOT fail for it.

#### Scenario: An add() that makes /dev a symbolic link

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts a cache at `/devices/cache`, and whose steps
  are an `add()` that replaces `/dev` with a symbolic link to `devices` and then a `run()`
- **WHEN** I build `build`
- **THEN** it fails naming `build` and `/dev`, and the `run()` step did not run

### Requirement: Tool caches need chroot isolation

The executor SHALL fail an attempt that mounts caches before its steps, naming the isolation, when it runs steps with
an isolation other than chroot, whose mounts the checks of a cache path are made for. An attempt that mounts no cache
SHALL NOT fail for it.

#### Scenario: A cache under OCI isolation

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts a cache, and a local executor given OCI
  isolation
- **WHEN** I build `build`
- **THEN** it fails naming `build` and `oci`, and no step ran

### Requirement: An executor refuses a working directory in a cache

The executor SHALL fail the attempt, naming the paths, when the working directory, as `workdir()` steps or else the
image left it, resolves, before a `run()` step or after the last step, to a location that equals or lies beneath a
cache's location.

#### Scenario: A last workdir() that a variable puts at a cache

- **GIVEN** a workspace named `com.acme.shop` whose layer `tools` mounts a cache at `/cache`, from an image whose
  environment sets `CACHE=/cache`, with a `run()` step and then, last, `workdir("$CACHE")`
- **WHEN** I build `tools`
- **THEN** it fails naming `tools` and `/cache` and saying that the working directory is in a cache, and no image is
  stored

#### Scenario: A workdir() that a variable puts at a cache

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts a cache at `/cache`, from an image whose
  environment sets `CACHE=/cache`, with the steps `workdir("$CACHE")` and a `run()` that writes `out`
- **WHEN** I build `build`
- **THEN** it fails naming `build` and `/cache` and saying that the working directory is in a cache, and the step did
  not run

### Requirement: An executor refuses a secret, add() or source that resolves into a cache

The executor SHALL fail the attempt, naming the paths, when, before a `run()` step, a secret's file, a file every step
is given or a volume the image declares resolves at, beneath or above a cache's location, or when an `add()`
destination, expanded with its step's environment and cleaned, or a task's `source`, resolved through symbolic links
before any `..`, resolves at or beneath one. An `add()` destination or `source` above a cache's location SHALL be
allowed.

#### Scenario: A secret's file inside a cache

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts a cache at `/cache`, from an image in
  which `/alias` is a symbolic link to `/cache`, and whose `run()` step is given a secret as the file `/alias/token`
- **WHEN** I build `build`
- **THEN** it fails naming `build`, `/alias/token` and `/cache`, and the step did not run

#### Scenario: A volume that an earlier step moves into a cache

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts a cache at `/cache`, from an image that
  declares the volume `/alias/data`, and whose first of two `run()` steps replaces `/alias` with a symbolic link to
  `/cache`
- **WHEN** I build `build`
- **THEN** it fails naming `build`, `/alias/data` and `/cache`, and its second `run()` step did not run

#### Scenario: A symbolic link that an earlier step makes into a cache

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts a cache at `/cache`, whose first `run()` step
  makes `/alias` a symbolic link to `/cache`, and whose later `add()` step adds a file to `/alias/file`
- **WHEN** I build `build`
- **THEN** it fails naming `build`, `/alias/file` and `/cache`

#### Scenario: An add() destination that a variable puts in a cache

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts a cache at `/go/pkg/mod`, from an image
  whose environment sets `GOPATH=/go`, and whose steps add a file to `$GOPATH/pkg/mod/extra` and then run a command
- **WHEN** I build `build`
- **THEN** it fails naming `build`, `/go/pkg/mod` and `/go/pkg/mod/extra`

### Requirement: A cache belongs to its workspace

Each cache SHALL be identified by the name of its workspace and its label, and kept apart for each owner and group, as
"A cache is kept apart for each owner and group" requires. No attempt SHALL be given the cache of a workspace named
differently from the one whose job started it, though a result, or a running attempt, is shared as any other, and a
cache whose label differs SHALL start empty.

#### Scenario: Two workspaces with the same label keep separate caches

- **GIVEN** a workspace named `com.acme.first` and one named `com.acme.second`, each declaring
  `tool_cache(name = "tool", path = "/cache")` and a task `probe` that mounts it, lists `/cache` and writes a file
  named by a parameter
- **WHEN** I build `probe` in the first with the parameter `one`, and then in the second with `two`
- **THEN** the second build's step lists nothing in `/cache`

### Requirement: An unnamed workspace cannot mount caches

A target that mounts a cache SHALL fail to load in a workspace whose `WORKSPACE` does not name it. The error SHALL name
the file, line and column of the target and say that the workspace needs a name. Declaring a `tool_cache` rule that no
loaded target mounts SHALL NOT fail.

#### Scenario: Mounting a cache in an unnamed workspace

- **GIVEN** a workspace whose `WORKSPACE` file is empty, and a task `build` that mounts a cache
- **WHEN** I build `build`
- **THEN** it fails before anything runs, naming `build`'s declaration and saying that the workspace needs a name
  in `WORKSPACE`

#### Scenario: An unmounted cache in an unnamed workspace

- **GIVEN** a workspace whose `WORKSPACE` file is empty, whose BUILD file declares
  `tool_cache(name = "tool", path = "/cache")`
  and a task `app` that mounts no cache
- **WHEN** I build `app`
- **THEN** it succeeds

### Requirement: Loading refuses a cache path it cannot mount

Loading SHALL refuse a cache's or a mount's path that is not absolute or not in clean form, is `/`, contains a variable,
is at or beneath `/dev`, `/proc` or `/sys`, or equals, lies beneath or contains `/etc/hosts`, `/etc/hostname`,
`/etc/resolv.conf`, `/run/.containerenv`, `/run/secrets` or `/etc/crypto-policies`, which buildah may bind into a
`run()` step, naming the path.

#### Scenario: A cache path beneath /dev

- **GIVEN** a workspace named `com.acme.shop` that declares `tool_cache(name = "tool", path = "/cache")`, and a task
  `build` with `tool_caches = [mount("tool", path = "/dev/shm/cache")]`
- **WHEN** I build `build`
- **THEN** it fails before anything runs, naming `build` and `/dev/shm/cache`

#### Scenario: A cache path that holds a file every step is given

- **GIVEN** a workspace named `com.acme.shop` that declares `tool_cache(name = "tool", path = "/run")`, mounted by
  `build`
- **WHEN** I build `build`
- **THEN** it fails before anything runs, naming `/run` and `/run/.containerenv`

#### Scenario: A cache path not in clean form

- **GIVEN** a workspace named `com.acme.shop` that declares `tool_cache(name = "tool", path = "/cache/")`
- **WHEN** I build any target of the package
- **THEN** it fails naming the call and `/cache/`

### Requirement: Loading refuses a cache path that conflicts

Loading SHALL refuse a mounted path that equals, contains or lies beneath another mounted path of the target or a
secret's file; an `add()` destination or a `workdir()` with no variable that, once cleaned, equals or lies beneath a
mounted path, or a `source` with no `..` component that, once cleaned, does; one cache mounted twice by a target; a
target that mounts caches and has no `run()` step; and a label naming anything but a `tool_cache` rule.

#### Scenario: A task's output inside a cache

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts a cache at `/build` and has
  `source = "/build/out"`
- **WHEN** I build `build`
- **THEN** it fails before anything runs, naming `build`, `/build` and `/build/out`

#### Scenario: Two mounts at one path

- **GIVEN** a workspace named `com.acme.shop` that declares `tool_cache(name = "go", path = "/cache")` and
  `tool_cache(name = "npm", path = "/cache")`, and a task `build` with `tool_caches = [mount("go"), mount("npm")]`
- **WHEN** I build `build`
- **THEN** it fails before anything runs, naming `build`, `go`, `npm` and `/cache`

#### Scenario: A workdir() in a cache

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts a cache at `/cache` and has the step
  `workdir("/cache/sub")`
- **WHEN** I build `build`
- **THEN** it fails before anything runs, naming `build`, `/cache` and `/cache/sub`

#### Scenario: An add() above a cache

- **GIVEN** a workspace named `com.acme.shop` whose layer `src` mounts a cache at `/src/.cache`, adds the package's
  files to `/src`, none of them beneath `.cache`, and runs a step that writes `/src/.cache/file`
- **WHEN** I build `src`
- **THEN** it succeeds and its image holds an empty directory `/src/.cache`

#### Scenario: A workdir() above a cache

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts a cache at `/app/.cache` and has the steps
  `workdir("/app")` and a `run()` that writes `/app/.cache/file`
- **WHEN** I build `build`
- **THEN** it succeeds

#### Scenario: A layer with caches and no run() step

- **GIVEN** a workspace named `com.acme.shop` whose layer `files` mounts a cache at `/cache` and has only `add()`
  steps
- **WHEN** I build `files`
- **THEN** it fails before anything runs, naming `files` and saying that caches are mounted only for `run()` steps

#### Scenario: One cache mounted twice

- **GIVEN** a workspace named `com.acme.shop` that declares `tool_cache(name = "tool", path = "/cache")`, and a task
  `build` with `tool_caches = [mount("tool"), mount("tool", path = "/other")]`
- **WHEN** I build `build`
- **THEN** it fails before anything runs, naming `build` and `tool`

#### Scenario: Nested caches

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts one cache at `/cache` and another at
  `/cache/go`
- **WHEN** I build `build`
- **THEN** it fails before anything runs, naming `build`, `/cache` and `/cache/go`

#### Scenario: A label that is no cache

- **GIVEN** a workspace named `com.acme.shop` whose task `build` has `tool_caches = [mount("tools")]`,
  where `tools` is a layer
- **WHEN** I build `build`
- **THEN** it fails before anything runs, naming `build` and `tools`

### Requirement: A standalone build refuses a request without a valid workspace name

A standalone local build whose graph mounts caches SHALL fail before anything runs when its request's workspace name
breaks the rules a name follows, or is empty and the request doesn't set `NoToolCaches`.

#### Scenario: An API request without a workspace name

- **GIVEN** a local executor, and a graph request with no workspace name whose graph has a task `build` that mounts a
  cache
- **WHEN** the request is sent without `NoToolCaches`
- **THEN** the job fails before anything runs, saying that the workspace needs a name

### Requirement: A cache's owner is fixed by its rule

A cache's directory SHALL be created owned by the `tool_cache` rule's `owner` and `group`, given as numeric IDs, and
by root when neither is given, whether the executor gives it empty or not; a later attempt given it warm SHALL find it
as earlier steps left it, owner included. A rule
SHALL give both or neither, each as decimal digits from 0 to 4294967294, and a name or any other value SHALL be refused,
when loading.

#### Scenario: A non-root step writes to its cache

- **GIVEN** a workspace named `com.acme.shop` that declares
  `tool_cache(name = "tool", path = "/cache", owner = "1000", group = "1000")`, and a task `probe`
  whose image runs as user 1000 and whose step writes to the cache at `/cache`
- **WHEN** I build `probe`
- **THEN** it succeeds

#### Scenario: An owner given by name

- **GIVEN** a workspace named `com.acme.shop` that declares
  `tool_cache(name = "tool", path = "/cache", owner = "node", group = "1000")`, mounted by `app`
- **WHEN** I build `app`
- **THEN** it fails naming the call and saying that `owner` must be a numeric ID

#### Scenario: An owner out of range

- **GIVEN** a workspace named `com.acme.shop` that declares
  `tool_cache(name = "tool", path = "/cache", owner = "4294967295", group = "0")`
- **WHEN** I build any target of the package
- **THEN** it fails naming the call and saying that `owner` must be from 0 to 4294967294

#### Scenario: Owners written differently are one owner

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts a cache declared with `owner = "1000"` and
  `group = "1000"`, and that has built `build`
- **WHEN** the BUILD file is changed to give the cache `owner = "01000"`
- **AND** I build `build` again
- **THEN** it succeeds and `build` was reused

#### Scenario: An owner without a group

- **GIVEN** a workspace named `com.acme.shop` that declares
  `tool_cache(name = "tool", path = "/cache", owner = "1000")`
- **WHEN** I build any target of the package
- **THEN** it fails naming the call and saying that `owner` and `group` are given together

#### Scenario: A non-root step writes to an empty cache

- **GIVEN** the workspace of the scenario "A non-root step writes to its cache"
- **WHEN** I build `probe` with `--no-tool-caches`
- **THEN** it succeeds

### Requirement: A cache's mount point takes its owner, and its parents the image's user

A cache's mount point in the container, whether the executor creates it or finds it empty, SHALL be owned by the
`tool_cache` rule's owner and group. Each missing parent directory the executor creates SHALL be owned by the user the
attempt's image runs as, as a `workdir()` would make it. A parent already there SHALL keep its owner. The executor
SHALL fail the attempt, naming the cache, when it cannot give a mount point the cache's owner and group.

#### Scenario: A cache's mount point belongs to its owner

- **GIVEN** a workspace named `com.acme.shop` that declares
  `tool_cache(name = "go-build", path = "/home/node/.cache/go-build", owner = "2000", group = "2000")`, and a layer
  `tools` that mounts it, from an image that runs as user 1000 and has `/home/node` but no `/home/node/.cache`,
  and whose step makes `/home/node/.cache/pip` and writes a file there
- **WHEN** I build `tools`
- **THEN** it succeeds, and its image holds an empty directory `/home/node/.cache/go-build` owned by 2000 and a
  directory `/home/node/.cache` owned by 1000

#### Scenario: A cache does not take a workdir() above it from its user

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts a cache that gives no owner at `/app/.cache`,
  from an image that runs as user 1000 and has no `/app`, with the steps a `run()`, then `workdir("/app")`, and then a
  `run()` that writes `/app/file`
- **WHEN** I build `build`
- **THEN** it succeeds

### Requirement: A cache is kept apart for each owner and group

An owner and group that have not used a cache, or have used it only under other ID mappings of the executor's
containers, SHALL find it empty, and the executor SHALL never change the owner of what another owner and group left in
it.

#### Scenario: Changing a cache's owner gives it empty

- **GIVEN** a workspace named `com.acme.shop` that declares `tool_cache(name = "tool", path = "/cache")`, and a task
  `probe` whose step lists `/cache` and makes a directory there named by a parameter, and that has built `probe`
  with the parameter `one`
- **WHEN** the BUILD file is changed to give the cache `owner = "1000"` and `group = "1000"`, and `probe`'s image
  to run as user 1000
- **AND** I build `probe` with the parameter `two`
- **THEN** it succeeds and the step lists nothing in `/cache`

#### Scenario: Changing a cache's owner back finds what it held

- **GIVEN** the build of the scenario "Changing a cache's owner gives it empty"
- **WHEN** the BUILD file is changed back to give the cache no owner, and `probe`'s image to run as root
- **AND** I build `probe` with the parameter `three`
- **THEN** it succeeds and the step lists `one` and not `two`

### Requirement: Pruning deletes caches

`vamos prune` on a standalone local executor SHALL delete every workspace's caches with the rest of its cache, and, when
the prune succeeds, report how many it removed from use; a failure to delete what it removed from use SHALL be a
warning. A cache SHALL NOT be deleted while a build that mounts it runs.

#### Scenario: A build after a prune starts with empty caches

- **GIVEN** a workspace named `com.acme.shop` whose task `probe` mounts a cache at `/cache`, lists it and writes
  a file there, and that has built `probe`
- **WHEN** I prune
- **AND** I build `probe` again
- **THEN** the step lists nothing in `/cache`

### Requirement: A local coordinator's prune leaves tool caches

`vamos prune` on `vamos coordinator --local` SHALL neither delete, move nor count the persistent caches of standalone
builds that share its store, nor delete what an earlier standalone prune left to delete.

#### Scenario: Pruning a local coordinator keeps a standalone build's cache

- **GIVEN** a workspace named `com.acme.shop` whose task `probe` mounts a cache at `/cache`, lists it and writes a file
  there named by a parameter, and that has built `probe` with the parameter `one` without a coordinator
- **WHEN** I prune `vamos coordinator --local` on the same store
- **AND** I build `probe` with the parameter `two` without a coordinator
- **THEN** the prune reports no tool caches removed, and the step lists the file `one`

### Requirement: Provenance records the caches an attempt mounted

A result's provenance SHALL record the label of each cache its attempt mounted and whether the executor gave it empty,
meaning that the attempt mounted an empty directory of its own rather than the workspace's cache, as under
`--no-tool-caches`, on a worker and under `vamos coordinator --local`. A workspace's cache
created for the attempt SHALL be recorded as not given empty. This record SHALL be outside the key, as the rest of
provenance is.

#### Scenario: A clean build's provenance

- **GIVEN** a workspace named `com.acme.shop` whose task `build` mounts `//tools:go-build`
- **WHEN** I build `build` with `--no-tool-caches`
- **THEN** its result's provenance records `//tools:go-build` as given empty
