# Design

## Context

See [proposal.md](proposal.md) for motivation and [the spec](specs/tool-caches/spec.md) for the behavior.

The pieces this design builds on:

- An action's key is the digest of its `sched.Action`, which holds every input that can change what a target makes
  ([storage.md](../../../docs/design/storage.md#action-keys)).
  Secrets already set the pattern for something a step is given that the key covers only in part:
  their ID and path are in the key, their value never is.
- A `sched.Request` carries the action plus execution metadata that the key excludes, and crosses to a worker as JSON.
  The workspace name reaches the scheduler as `sched.Options.Workspace`, which keys target history, but no
  `sched.Request` carries it, so no runner sees it.
- `run()` steps go through buildah with chroot isolation. Secrets become `RunOptions.RunMounts` in
  [step.go](../../../internal/run/step.go). A task's output is collected from the container's root filesystem,
  where a bind mount's contents can't be seen.
- `cache.Directory` lives beside buildah's storage (`run.CacheDir`). Builds hold a shared `flock` on it and prune
  holds an exclusive one ([scheduling.md](../../../docs/design/scheduling.md#cache-directory-locking)).
- `cache.Provenance` is a field of every result, outside the key.

## Goals / Non-Goals

**Goals:**

- Incremental builds on a persistent local executor, for any tool that keeps a cache directory.
- A BUILD file that mounts caches builds anywhere, with identical keys, warm or cold.
- One clear trust boundary: the user's assertion, written down in the reference, with nothing vamos pretends to verify.

**Non-Goals:**

- Caches on a coordinator's workers, or shared across workers, such as by worker affinity,
  a content-store-backed cache, or a `GOCACHEPROG` served by vamos. Each can come later without a BUILD file changing.
- Limits on how large a cache grows, eviction, and pruning one cache or one workspace's caches alone.
- Exclusive (`locked`) or per-attempt (`private`) sharing. Every mount is shared by concurrent attempts.
- Mounting a cache for one step of a target. Mounts are per target, by decision.
- `mount()` options beyond `path`. The list of `mount()` values is shaped to take them later.
- Detecting a tool whose cache does change what it makes.

## Decisions

### A cache is lent, never an input

The key covers how the container is shaped by the mount, not what the mount holds.
`sched.Action` for a layer or task gains `ToolCaches []Mount{Path, Label, Owner, Group}`, sorted by path, with the
effective path, the canonical label (`//pkg:name`) and the rule's numeric owner and group. The path is in the key
because a mount hides a path from the image:
a layer whose step writes `$GOCACHE` keeps those files without the mount and loses them with it.
The label is in the key so that the key always says which cache a target used,
the same way a secret's ID is there. Two targets differing only in which cache they mount are different actions.
The owner and group travel in the action because the runner, local or on a worker, needs them to prepare the
directory, and the action is what reaches it. A changed owner therefore reruns the targets that mount the cache,
which is what someone fixing a permission failure wants.

The workspace name and the location on disk are request metadata, never action fields,
so `workspace-identity`'s rule that the name changes no key holds as written.

Target history compares an action's own fields one by one, so `ToolCaches` joins them: a target whose mounts differ
from its nearest entry is `own-inputs`, as a changed step is, and the mounts count as one own difference in its
distance, as the command does. Otherwise a moved mount would differ in its key alone,
and attribution would report a dependency it can't name.

*Alternative*: leave mounts out of the key entirely. Rejected: adding a mount to a layer changes its image,
and the same key would then name two different images.

`ActionVersion` advances to 12. An action with no caches serializes as it does now apart from its version, but a worker
of version 11 would ignore `ToolCaches` and write a cache's contents into a layer's image under the same key.
`graph.Version` advances to 5 as well, because the graph's layers and tasks gain their mounts: a coordinator of graph
version 4 would decode a new client's graph, drop the mounts, and build actions without caches that its own workers,
agreeing with its older action version, would accept. `graph.Validate` checks each node's mounts as it checks the rest
of a node, since an executor decodes graphs that a client sent: paths absolute, clean, literal, not `/`, not at or
beneath `/dev`, `/proc` or `/sys` and not equal to or lying beneath or containing a file buildah binds into every step
(below), sorted and unique with none beneath another, a canonical label that no other mount of the node uses, and an
owner and group in range; and a node that mounts caches has a `run()` step.

### The tool_cache rule holds a default path; a mount may override it

```python
tool_cache(name = "go-build", path = "/cache/go-build")               # path required; owner, group optional

task(
    name = "lint",
    tool_caches = [
        mount("//tools:go-build"),                                # at /cache/go-build
        mount("//tools:go-mod", path = "/root/go/pkg/mod"),       # overrides the cache's path
    ],
    ...
)
```

Most targets mount a cache where the rule says, so the common case names only the label.
A target whose image keeps the tool's directory elsewhere overrides the path on its own mount.
The rule names no variable and no tool. A step that needs one sets it with `build_env`.

`tool_caches` is a list of `mount()` values, not a dict from path to label. A `mount()` value has room for options
later, such as sharing or read-only, and a dict would have to change shape to take them. The cost is that a dict made
paths unique by construction, and a list doesn't, so loading checks for duplicates.

`load` resolves each mount to its effective path, the override or else the rule's, and its canonical label.
`graph.Layer` and `graph.Task` gain those mounts, sorted by path. The key sees only the effective path,
so `mount("tool", path = "/cache")` and `mount("tool")` for a cache whose path is `/cache` are one action, and
changing the rule's path reruns every target that mounts it without an override.
The `tool_cache` rule is a declaration that `load` resolves labels against, not a graph node:
nothing is built for it and nothing depends on it. Every command that takes a target refuses its label, and so does a
`base`, `dep()`, `add()` or publication check that names it, each saying that a tool cache is mounted with `mount()`.
A label that has no graph node must not reach the graph: `vamos build`, which builds what a publication publishes
rather than refusing it, needs its own refusal, as it has for a glob.

Paths, the rule's and the mount's, are literal: no `$VAR` expansion, unlike `add()` destinations. They must also be in
clean form, as `path.Clean` leaves them, so that `/cache/` or `/x/../cache` can't miss a hit, slip past the checks
against `workdir()`, `add()` and `source`, or make two mounts of one directory look different. A path at or beneath
`/dev`, `/proc` or `/sys` is refused: buildah's chroot isolation mounts those itself and silently skips any mount given
there, so the step would see the host's directory and never the cache. A path that equals, lies beneath or contains one
of the files buildah binds into every `run()` step, `/etc/hosts`, `/etc/hostname`, `/etc/resolv.conf` and
`/run/.containerenv`, and on a FIPS host `/run/secrets` and `/etc/crypto-policies`, is refused too: buildah would bind
the file over the cache, fail to make its file where the cache made a directory, or make a mount point for it inside the
cache, leaving a placeholder in what the cache holds. The FIPS paths are mounted only on a FIPS host: `/run/secrets`
when the image links `/etc/system-fips`, and the two leaves `/etc/crypto-policies/back-ends` and
`/etc/crypto-policies/config`, each bound apart, when the image has `/usr/share/crypto-policies/back-ends/FIPS`. A BUILD
file builds anywhere, so loading refuses them everywhere, and the runner resolves `/run/secrets` and each of the two
leaves, not their parent, before each `run()` step. These checks follow buildah's chroot isolation, which vamos's image
sets; an attempt that mounts caches under any other isolation, whose runtime mounts differ, fails before its steps
naming it. The runner refuses the same overlaps between resolved locations, resolving the bound files too, since a
symlink can put one in a cache, as Ubuntu's `/etc/resolv.conf`, a link into `/run/systemd`, does. An attempt that mounts
caches fails when `/dev`, `/proc` or `/sys` is a symlink, as the image left it or a step made it: buildah follows it
from the host's side when it mounts there, which the runner's resolution inside the root filesystem can't match. The
runner also refuses a cache that equals, lies beneath or contains the resolved location of a volume the image declares,
whose directory buildah makes in the root filesystem before it binds the caches; it cleans a volume's path before
following its symlinks, as buildah does, so `/alias/../cache/data` is `/cache/data` whatever `/alias` points at. It
checks the system roots, the bound files and the volumes again before each `run()` step, since an earlier step can
change the links that lead to them. Loading and `graph.Validate` check a literal path with one function of `graph`,
which `load` calls, and which calls `graph.CheckPath` first, so a NUL byte is refused as in any other path. Load-time
conflict checks need the final path, and a path that a base image's variables decide could pass them and still collide
when the step runs. Loading can't settle every conflict, though: an `add()` destination may hold a variable that only
the image defines, and a symlink in the image can make two different paths one. Loading checks what it can see: literal
`add()` destinations and `workdir()` paths, compared textually once `path.Clean` has cleaned them, as the runner cleans
a step's paths before it uses them, so that `workdir("/cache//sub")` or `add(".", "/x/../cache/f")` is caught. A path
with a variable is left to the runner, since cleaning `/x/$SUB/../cache` before `$SUB` is known would settle its `..`
wrongly. A task's `source` is not a step's path: the runner resolves it through symlinks before applying a `..`, so
loading compares it only when it has no `..` component, and leaves one such as `/work/../build` to the runner. The
runner checks the rest (see [Mounting](#mounting-a-bind-for-each-cache-for-each-run-step)).

A target may mount each cache once. Mounting one directory at two paths within a single target has no clear use,
and refusing it now leaves room to give it a meaning later.

*Alternative*: no path on the rule, every mount names its path. Rejected on review: it repeats the path at every use,
and a path on the rule sets a convention that targets keep to unless they have a reason not to.

*Alternative*: per-step `run(..., tool_caches = ...)`, like secrets. Rejected by decision: a tool cache belongs to the
target's environment, the same as the `build_env` that names it, and repeating it on every step adds nothing.

### Ownership is numeric and belongs to the rule

All of a cache's mounts share one directory, so its owner can't depend on the target.
A user name would resolve through each image's `/etc/passwd` and could mean different IDs in different images,
so `owner` and `group` are numeric strings only: decimal digits, read as base ten, from 0 to 4294967294.
That leaves out a sign, and 4294967295, which `chown` takes as "leave unchanged". Loading parses them to integers
so that `"1000"` and `"01000"`, or no owner and group and `"0"` for both, name one owner, one directory and one key.
A rule gives both or neither, since either one alone would leave the other to a default that decides the directory,
the key and whether a non-root step can write.

The owner and group are part of where a cache lives (see [Storage](#storage-beside-the-cache-directory-under-its-lock)),
so a cache whose rule changes either one is a different directory, created empty with the new owner and group.
No cache directory on the executor is ever chowned or replaced. Changing the owner of the old directory alone would
leave what earlier steps made beneath it owned by the old owner, so a step could fail with a warm cache and pass with
an empty one, and changing the owner of everything inside would cost time in proportion to the cache's size.
Replacing the directory would remove it from beneath any concurrent attempt that has it mounted, and two worktrees
whose BUILD files disagree about the owner would empty each other's cache on every build.
With one directory per owner, each keeps its own, and `vamos prune` reclaims the one no BUILD file uses any more.

*Alternative*: owner on each mount. Rejected: two targets could then disagree about the owner, and since each owner
and group has its own directory, they would silently stop sharing the cache.

### Storage beside the cache directory, under its lock

```
<run.CacheDir>/                         cache.Directory today, flock shared by builds, exclusive by prune
  tool-caches/
    <hex sha256 of the workspace name>/
      <hex sha256 of the canonical label>/
        label                           the label, for diagnostics
        <owner>-<group>-<mapping>/      the rule's numeric owner and group, 0-0 when it gives neither, and a digest
                                        of the attempt's UID and GID mappings, or host with none
          data/                         what is mounted
```

Hashing the label keeps `/` and `:` out of directory names. Hashing the workspace name keeps the name itself out of
each mount's source path, which a step can read in `/proc/self/mountinfo`, as `workspace-identity` requires. The digest
is still there to read, as the executor's own paths are, so a step that writes its mount table out makes something that
differs between workspaces and executors. A reproducible step reads no mount table, and a cache's contents and location
are outside the key in any case: the user's assertion covers a step that reads them. The `workspace-identity` delta
says so. Because the directory sits inside
`cache.Directory`, the existing lock keeps a prune from deleting a cache while a build has it mounted. It also persists
in the `/var/lib/containers` volume that users already keep.

A cache is created on first use in two steps, each built under a temporary name beginning with `.tmp-` beside its final
path and then renamed into place: first the label's directory with its `label` file, then the
`<owner>-<group>-<mapping>/` directory with its `data/`, set to mode 0755 after it is made, whatever the umask, and
already owned by the rule's owner and group mapped to host IDs through the attempt's ID mappings, those of the builder
it created, which a container can take from its image rather than the store, so that a step sees them as the rule's. The
directory is named by the rule's IDs and those mappings both, so an attempt whose mappings differ finds a new, empty
directory rather than one whose files a step sees owned by IDs it no longer maps, and two owners that one store's
mappings and another's put on the same host IDs keep apart, as "A cache is kept apart for each owner and group"
requires. The old mapping's directory stays until a prune, which counts it. Buildah's chroot isolation, in
`runUsingChroot` of buildah v1.46.0, makes its bind mounts before it creates the builder's user namespace, so the
root-owned directories above `data/`, `<run.CacheDir>` among them, which `cache.Directory` makes 0700, need no wider
mode; task 3.1's build with a remapped builder is to confirm it. A published directory is never empty, holding the
`label` file or `data/`, so a plain `rename` onto it fails, with `ENOTEMPTY` or `EEXIST`, and never replaces it; no flag
the filesystem might not support is needed. The workspace's directory above them is made with `MkdirAll`, which a
concurrent creator can't fail, and is never counted as a cache. Only a prune removes anything here, and it first renames
all of `tool-caches/` aside under the exclusive lock (see [Prune](#prune)), so no build ever sees a directory vanish and
even a prune killed partway never leaves an empty one behind. When two attempts create one at once, the rename that
fails with `ENOTEMPTY` or `EEXIST` loses; any other error, such as `ENOTDIR` for a stray file at the target, fails the
attempt naming the path. The loser removes its own and uses the winner's. So no attempt can mount a cache whose owner is
not yet set, and adding a second owner beside a label that already has one renames only the new owner's directory. A
`.tmp-` directory, or a label's directory with no owner's directory yet, that an interrupted attempt leaves behind is
never mounted or counted, and the next standalone prune removes it.

*Alternative*: buildah's own `type=cache` mounts. Rejected: they live under `/var/tmp/buildah-cache-*` with buildah's
lifetime and keys, outside vamos's lock and prune.

### Mounting: a bind for each cache, for each run step

The runner adds a read-write bind mount for each of the action's caches to every `run()` step's options, as a
`specs.Mount` of type `bind` in `RunOptions.Mounts`, not as a `--mount` string in `RunOptions.RunMounts` beside its
secrets: buildah turns a read-write bind given there into an overlay that it discards after the step, so nothing a step
wrote would stay in the cache. The mount points are empty directories it creates just before the first `run()` step,
where nothing is there yet (below), so that the image holds an empty directory at each cache path whether the cache was
warm or cold (see the spec). `add()` steps get no mount.

Each cache path is resolved before any of the attempt's steps, against the root filesystem the attempt starts from, and
again, as below, just before its first `run()` step. `securejoin` resolves it inside that filesystem, as the runner
already does for outputs, so a symlink is followed but can't lead outside it. A cache path through a symlink, such as
one beneath `/var/run` or a usr-merged `/lib`, is fine: the image that decides where it leads is in the key already.
Loading refuses a target that mounts caches and has no `run()` step, since caches are lent only to `run()` steps, so
every such attempt has a first step and a last. Every check compares resolved locations, so that two paths one symlink
makes the same are caught:

- **Before the steps**, the attempt fails, naming the paths, if `/dev`, `/proc` or `/sys` is a symlink, or if a cache's
  resolved location is at or beneath one of them, or its resolution passes through one of them, which buildah hides
  while a step runs, equals, lies beneath or contains where a file buildah binds into every step or a volume the image
  declares resolves, exists as anything but an empty directory, or equals, contains or lies beneath another cache's
  resolved location. A mount would hide what the image holds there from every `run()` step, and the image would still
  hold it afterwards, so neither "an empty directory at each cache path" nor "succeeds warm or cold" could hold.
  Clearing the path instead would silently delete what an earlier layer put there. Just before the first `run()` step
  the runner prepares the mount points in one order. It first applies the check of the working directory below, so that
  a `workdir()` a variable put at a cache is reported as such. It then resolves each cache path again and, before
  creating anything, fails the attempt if it now resolves elsewhere than before the steps or through `/dev`, `/proc` or
  `/sys`, as an `add()` that made a parent a symlink can make it, or if its location is anything but absent or the empty
  directory that was there before the steps, which it knows by the device and inode it recorded then, such as one an
  earlier layer mounted the same cache at, which it keeps: a directory a step made there, as an `add()` above the cache
  can, is refused, as task 3.2 tests. It then creates each missing mount point, so that it is present and empty for
  every later check, and in the image whatever step comes last, and sets each mount point's owner and group. Only then
  does the check that a location is an empty directory with the cache's owner and group apply, before that step and
  every later one. An `add()` or `workdir()` before that step makes a parent as it would without the cache. The mount
  point, new or kept, is owned by the rule's owner and group, root's when the rule gives none, so that a target built on
  the image that runs as that user can write there. Every mount point, created or kept, is given mode 0755. Missing
  parent directories are created the way `workdir()` creates them, owned by the user the attempt's image runs as; the
  last component, the mount point itself, is created apart from them with the rule's owner. A parent then belongs to
  whoever would have owned it without the cache: adding a cache at `/app/.cache` doesn't make `/app` root's and break a
  later `workdir("/app")` for a non-root user, a parent that two caches share doesn't depend on which comes first, and a
  symlink on the way whose target doesn't exist, which is followed inside the root filesystem, can't hand a system
  directory such as `/var/cache` to the rule's owner. A mount point it can't create, such as one beneath a regular file,
  fails the attempt naming the path, and so does an owner the executor can't set, such as an ID outside a user
  namespace's mapped range, naming the cache.
- **Before the steps, before each `run()` step and after the last step**, the attempt fails, naming the paths, if the
  resolution of a cache path, the working directory, a secret's file, a file buildah binds into a step or a volume the
  image declares passes through a cache's location, its own included, before it reaches its end, as `/alias` linked to
  `first/entry/../../second` does with a cache at `/first`: what a warm cache holds could lead it elsewhere while a step
  runs, and put a cache's writes into the image or a bind into a cache, though its end is where it was. An earlier step
  can make such a link, so the check is repeated.
- **Before each `run()` step, and once after all the steps**, the attempt fails if a cache path no longer resolves to
  its location, or its resolution now passes through `/dev`, `/proc` or `/sys`, since a step made or changed a symlink
  on the way, or if the location is no longer an empty directory owned by the cache's owner and group, since a step such
  as `workdir()` made something there while nothing was mounted, or an `add()` above it copied a directory there with
  another owner. The owner and group are compared as the container sees them, through the attempt's ID mappings, as they
  were set, so a store whose user namespace maps container IDs to other host IDs compares like with like. It also fails
  if the working directory, as `workdir()` steps or else the image left it, resolves to a location that equals or lies
  beneath a cache's location, since a variable or symlink can put a `workdir()` there that loading couldn't see, and
  relative writes would then go into the cache rather than the image. Before a `run()` step it also fails if `/dev`,
  `/proc` or `/sys` is a symlink, or if a secret's file, a file buildah binds into every step, or a volume the image
  declares resolves to a location that equals, lies beneath or contains a cache's location: an earlier step can change
  the links that lead to one. Otherwise every mount is made at its cache's location, which is the mount's destination,
  so buildah follows no symlink of its own. This keeps the image's empty directory at each cache path through the
  attempt's end, whichever step comes last. The check after the steps runs before a layer's image is committed or a
  task's output collected, so a failure stores nothing.
- **Before each `add()` step**, the attempt fails if its destination, expanded with the environment the step sees,
  cleaned as the step uses it, and then resolved, equals or lies beneath a cache's location.
- **When collecting a task's output**, the attempt fails if the resolved `source` equals or lies beneath a cache's
  location.

An `add()` destination, `workdir()` or `source` above a cache is allowed, as in `add(".", "/src")` with a cache at
`/src/.cache`. An `add()` there that does put files at the cache's location is caught by the emptiness
check before the next `run()` step or after the steps, and a `source` above a cache collects only its empty
directory.

Loading has already refused the literal conflicts it can settle, including a literal `workdir()` that equals or lies
beneath a cache path, so these checks catch what variables, symlinks and directory-making steps hide, and a task's
`source` with a `..` component, which loading leaves to them.

Resolving once is stricter than following a path wherever each step finds it: a step that installs a package which
turns `/lib` into a symlink, as `usrmerge` does, fails every later step of a target with a cache beneath `/lib`.
That is deliberate. A cache that moved between steps would leave mount points behind in the image, and checks of
later steps would compare against a location the cache was no longer mounted at. The error names the cache path
and says that a step moved it, so the user can choose a path that the target's steps leave alone.

Concurrent attempts mounting one cache share it. Go's, pnpm's and uv's caches tolerate this.
Sharing an attempt across jobs (`sched.Sharing`) is unaffected: the attempt runs once, with the cache of the workspace
whose job started it. Because a cache never changes a result, any job may join it.

### Where the workspace name and `--no-tool-caches` travel

`sched.Request` gains `Workspace string` and `NoToolCaches bool`, both execution metadata outside the key. The scheduler
fills them from its options: `Workspace` from `sched.Options.Workspace`, which already keys target history, and
`NoToolCaches` from a new `Options.NoToolCaches`. `api.GraphRequest` gains `NoToolCaches` beside its existing
`Workspace`. The local executor, outside coordinator mode, checks `Workspace`, whatever `NoToolCaches` says, when its
job reads the graph, where it sets it in the scheduler's options, not when the request is accepted. If the graph mounts
caches, the job fails before anything runs when the name breaks the rules a workspace name follows, or when it is empty
and `NoToolCaches` is not set, since only then would a persistent cache be chosen by it: the request comes from a
client, not from `load`, and an API or older client can send an empty name. A coordinator neither checks the name nor
chooses any cache by it, `vamos coordinator --local` included: the coordinator command creates its in-process local
executor with a coordinator-mode option of `run`, under which it neither checks a request's workspace name nor chooses a
persistent cache, and gives every cache empty, as a worker does, since its clients each supply a name of their own,
though it keys target history and telemetry with a name it is given, so a request to one that omits it builds a graph
that mounts caches with empty caches, as any request to it does; the CLI still refuses to load such a target in an
unnamed workspace, whichever executor it uses. The runner hashes the name into a path, so no name can place a directory
outside `tool-caches/`, and checks it again there with the same rules, only when it works out a persistent cache's
directory, so that an empty or malformed name from any route never names a shared cache, while a worker's or a
`--no-tool-caches` attempt, which uses no name, never checks one. The rules a name follows move from `load` to `graph`,
which `load`, `sched` and `run` may all import, as [package-layering.md](../../../docs/design/package-layering.md)
requires, so the job's check and `load`'s are one.

The local runner resolves each mount to the workspace's directory, or, when `NoToolCaches` is set, to a fresh
temporary directory that the attempt alone uses. A worker's runner always takes a temporary directory, whatever the
request says, so nothing persists from one attempt on a worker to the next. An attempt's temporary directory is shared
by all of its `run()` steps, as a persistent cache would be, is created with the rule's owner and group, as a
persistent one is, and is never shared with another target of the same build.

Temporary directories are made in a `tool-caches/` directory of the attempt's buildah container's own directory
(`Store.ContainerDirectory`), beside the files buildah keeps there, on the store's volume. They are made once the
builder exists, before the mount points are resolved, one per cache, set to mode 0755 and the rule's owner and group as
a persistent cache's directory is. They are not made in the default temporary directory, as a step's secrets directory
is: a cache can grow as large as everything a build compiles, and a worker's `/tmp` is on the container's own overlay
filesystem, outside the store volume that the Helm chart sizes and that the container store must use for the same
reason. The runner deletes them itself when the attempt ends, whatever ended it, before it removes the container:
removing the container deletes its directory inside `DeleteContainer`, which holds the store's locks throughout, so a
cache left to it would stall the worker's other attempts for as long as the delete takes. What a killed or crashed
attempt leaves goes with its container: on a worker through `run.RemoveAttempt`, which deletes the container directory's
`tool-caches/` first, outside the store's locks, for the same reason. The worker's order is unchanged: it removes the
attempt's container before it reports. Deleting a large temporary cache that a crashed subprocess left can outlast the
lease, and the coordinator then counts the worker lost rather than taking its report, retrying the attempt while its
retry budget allows, and otherwise failing it as lost rather than with the subprocess's own failure; that is accepted,
since it needs a crash whose temporary caches are large, and the runner deletes an attempt's temporary caches itself,
while the lease is still renewed, whenever its subprocess ends normally. The worker already calls `run.RemoveAttempt`
after every attempt and, for each attempt a crash left, when it starts. If that delete fails, `run.RemoveAttempt` still
removes the container, whose removal finishes the delete under the store's locks, and returns the failure, wrapped to
say that it was the temporary caches and not the container, with any other. That stall is accepted: vamos runs as root,
which deletes even read-only trees such as Go's module cache, so only a failure such as a process that survived its kill
causes it. Locally, a crash leaks them with the container it already leaks. `DeleteContainer` deletes the container's
layer, then its directories, then its record, so a removal that fails partway, as one racing a process that a failed
kill left writing there can, keeps the record without its layer and perhaps without buildah's state. `removeAttempt`
must then take a missing layer as already unmounted, which it doesn't today, so that the worker's next start finishes
the removal.

*Alternative*: a directory of temporary caches beside `cache.Directory`, deleted by a prune and by a worker for each
attempt. Rejected: it took its own discovery of a crashed run's attempts, a rule for an attempt whose kill failed, a
check of the coordinator's attempt ID before it became a path, and care where a worker and local builds share a store,
all of which the container's lifetime already settles. Locking each temporary cache by its attempt and sweeping
unlocked ones was rejected before that for the same reason.

*Alternative*: one temporary directory per cache for the whole build, shared by its targets. Rejected: a target that
only works because an earlier target of the same build warmed the cache would pass, so the flag would no longer show
that each target builds from nothing, and workers, which a build's targets are spread across, couldn't match it.

`--no-tool-caches` only affects attempts this job starts. A hit, or joining another job's attempt, can still reuse
a result that an attempt made with a warm cache. That's correct under the assertion.
Combined with `--rebuild`, it reruns the named targets with empty caches, but `--rebuild` still reuses their
unchanged dependencies, which may have been made with warm caches. No flag combination reruns a whole graph cold,
and this change adds none: under the assertion it would make nothing different.

*Alternative*: have a clean build refuse to join attempts. Rejected: it would add a Sharing rule to guard against
something the assertion already rules out.

### The unnamed-workspace refusal happens in load

`load` already knows the workspace's name. Configuring a target with non-empty `tool_caches` in an unnamed workspace
fails at that target's declaration. The check is lazy, like all loading: an unused `tool_cache` rule, or a target that
is never built, costs nothing. A mistake in a `tool_cache` call's own arguments is not lazy: like any builtin's, it
fails when the BUILD file is evaluated, whether or not anything mounts the cache.

*Alternative*: give an unnamed workspace's targets temporary empty caches, as `--no-tool-caches` does. Rejected:
their caches would never warm, and the user would see slow builds with nothing saying why.

This is the one way the name affects what can be built, so the change modifies `workspace-identity`'s requirement
that the name never affects what is built: a named and an unnamed workspace still share results for any target
both can build, but only a named one can build a target that mounts caches.

### Provenance

`cache.Provenance` gains `ToolCaches []ToolCacheUse{Label, Empty}`. It's diagnostic, outside the key, and recorded by
the attempt that made the result. `Empty` says that the attempt mounted a temporary directory of its own, under
`--no-tool-caches`, on a worker or in coordinator mode; a workspace's persistent cache is recorded as not empty even on
its first use, since what it held is not what a result's reader needs to know, but whether a warm cache could have been
there.

### Prune

`Local.prune`, in the `first` callback that `cache.Directory.Prune` runs under the exclusive lock, beside
`removeCachedImages`, renames `tool-caches/` aside to `<run.CacheDir>/tool-caches.trash-<random>` and then counts the
owner directories of what it renamed, so that a prune refused for the lock renames nothing. A failed rename, other than
for a `tool-caches/` that doesn't exist, fails the prune, as a failed removal of an image does, and counts none; a
failure to count is a warning, the count is what it read, and the renamed tree is still deleted; a `tool-caches/` that
doesn't exist, on an executor that never mounted a cache, counts as none. Once `Prune` has taken its lock back shared,
as a build holds it, it deletes the renamed tree, which nothing can reach, along with any `tool-caches.trash-*` that an
earlier prune, killed partway, left behind. If `Prune` couldn't take the lock back, the prune job logs that the build
cache lost its lock and ends the process as soon as it finds the lock lost, before it reports: its runner, the
scheduler's sharing and a local coordinator's HTTP server all hold the directory, and none can be given a new one or
stopped from using it safely, so a CLI build fails and `vamos coordinator --local` stops, and the next start opens the
directory again. The prune leaves the trash for the next standalone prune, since another prune may be renaming its own,
and skips pruning histories, which a prune refused for the lock skips too. `cache.Directory.Prune` returns
`cache.ErrRelock` whenever it leaves the directory without its shared lock, after a refused upgrade or after the prune,
wrapping the lock's own error without `%w` so that it never matches `cache.ErrLocked`, which says a prune was refused
and removed nothing; `Local.prune` checks for `cache.ErrRelock` first. A failure to delete is a warning in
`api.Pruned.Warnings`, as a failed history removal is, and the next standalone prune tries again. `api.Pruned` gains
`ToolCaches`, the number of owner directories, `<owner>-<group>-<mapping>/`, that it removed from use, whether or not
deleting them then succeeded, one for each workspace, label, owner, group and mapping used; `.tmp-` leftovers and label
directories are not counted. A coordinator's prune is unchanged, since its workers keep no persistent caches, and so is
the prune of `vamos coordinator --local`: in coordinator mode `Local.prune` leaves `tool-caches/`, which holds the
persistent caches of standalone builds that may share its store, and only an attempt's temporary caches, which go with
its container, are its own. It deletes no trash and counts no tool caches either: the rename, the deletion of trash a
killed standalone prune left, and the count all belong to a standalone executor's prune.

## Risks / Trade-offs

- [A step that changes its cache's owner or mode, such as `chown -R 0:0 /cache`] → Later builds whose steps run as the
  rule's owner fail with a warm cache and pass with an empty one, breaking the assertion the user made. The reference
  says a step must leave its cache usable by the rule's owner; `vamos prune` repairs the cache, while `--no-tool-caches`
  gives the attempts a build starts empty caches, which gets a target that failed, and so stored no result, past it, and
  `--no-tool-caches --rebuild` reruns a named target cold that has one.

- [A tool whose cache trusts a file's size and modification time, as `go test`'s cached results do for the files a
  test reads] → vamos gives every file it adds the same epoch time, so an edit that keeps a file's size looks
  unchanged to such a tool, and a warm cache can return a stale result, breaking the assertion. The reference names
  this; `go test -count=1` keeps the build cache and turns result caching off, as task 6.1 has the root `BUILD`'s
  `test` do.
- [A tool whose cache does change what it makes] → The user asserted otherwise. The reference says so plainly,
  `--no-tool-caches --rebuild` reruns the named targets cold, `vamos prune` clears everything,
  and provenance shows which caches made a result.
- [An image that already holds files at a cache path] → The attempt fails naming the path, and the user picks
  another path with a mount override. Nothing is hidden or deleted silently.
- [Paths checked as the attempt runs, not at load] → A conflict that a variable or symlink hides fails the attempt
  when the runner reaches it, possibly after earlier steps have run.
  Loading still catches every literal conflict up front.
- [A tool whose cache misses a change it doesn't track, as Go's build cache misses changes to the C libraries and
  headers that cgo uses] → A target whose base installed other libraries reruns, as its key says, but the tool can
  return what it made against the old ones, breaking the assertion. The reference names this; a step can keep such a
  tool's cache in a directory beneath the mount named by what it doesn't track, as task 6.1 has the root `BUILD` name
  Go's cache directory by the image's installed packages. That assumes the packages' files are as installed: a step that
  edits them directly calls for `vamos prune` to clear the caches, or `--no-tool-caches --rebuild` to rerun the named
  targets cold once, since `--no-tool-caches` alone still reuses a result already stored.
- [A layer that mounts a cache leaves an empty directory at its path, and a target built on it can't mount a cache at
  a path above that, which is no longer empty] → The attempt fails naming the path, and the user mounts the cache
  beside it rather than above it. The reference names this.
- [A `run()` step that makes a cache's parent itself, as `mkdir /app` or `git clone <url> /app` does with a cache at
  `/app/.cache`] → The parent is there before the first `run()` step, so the step fails. The reference says to make
  the parent in an earlier step or mount the cache elsewhere.
- [A tool that deletes and remakes a directory holding a cache, as `npm ci` does `node_modules`] → It empties the cache
  and fails with `EBUSY` at the mount point on every build. The reference says to mount a cache outside such a
  directory.
- [A step that changes a symlink on a cache path and restores it within one step] → No check between steps can see it;
  the guarantee that a cache's contents stay out of the image covers writes that reach the mount.
- [On a FIPS host, an image whose `/usr/share/crypto-policies/back-ends/FIPS` links to where a cache's mount point
  will be] → Buildah resolves that probe through the image's symlinks, so creating the mount point makes it add its
  FIPS binds, which the runner then checks as it checks any bind. The reference names this.
- [Caches grow without bound] → `vamos prune` clears them; size limits are a later change.
- [A tool that can't share its cache between concurrent attempts] → Shared is the only mode for now; `locked` can
  be added as a `tool_cache` rule argument without changing what is already written.
- [Remote builds get no speedup] → The trade-off of starting local. The "may give empty" rule keeps the door open
  for per-worker caches, without a BUILD file or key changing.
- [The word "cache" already means the build cache] → The docs call these tool caches, and the rule, the argument and
  the flag never appear without that context.
- [ActionVersion 12 invalidates every cached result once] → It's a one-time rebuild, as every version change is,
  and each target's next miss is `new`, since history compares only entries of one `ActionVersion`.

## Migration Plan

None for users: no BUILD file changes meaning. The version bump rebuilds once.
To roll back, run `vamos prune` before reverting, or afterwards delete `tool-caches/` from `<run.CacheDir>` by
hand: a vamos without this change neither uses nor prunes them.

## Open Questions

None.
