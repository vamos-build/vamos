# Storage and identity

This document describes storage, artifact identity, and caching in Vamos: the three storage layers,
tool caches beside them, action key generation, image naming, pin resolution, and cache pruning.

## The three storage layers

Build execution relies on three distinct storage layers:

The following diagram shows the three stores and the data exchanged with the loader, scheduler, runner,
and finish phases.

```mermaid
flowchart LR
    subgraph cachepkg["cache.Directory, beside the container store"]
        content[("Content store<br/>graph blobs, source and output trees,<br/>logs, exported image blobs")]
        records[("Record store<br/>action results, tag records,<br/>target history, generation")]
    end
    cstore[("Executor's container store<br/>images, named in the cache repository")]
    lock[/"vamos.lock in the project<br/>pins: tag → digest"/]

    load["load (client)"] -- "sources, graph" --> content
    lock -- "pins" --> load
    sched["sched.Sharing"] -- "SetResult, SetTag" --> records
    runner["Runner"] -- "trees, logs" --> content
    runner -- "images" --> cstore
    finish["finish (client)"] -- "reads" --> content
```

The three stores serve distinct roles:

- The **content store** is a content-addressed object store holding immutable blobs keyed by sha256 digest:
  serialized graph blobs, source file trees, task output trees, build logs, and exported image layer blobs.
- The **record store** is a key-value store mapping action keys to execution results,
  image tags to resolved manifest records,
  and each configured target of a named workspace to its [target history](#target-history).
- The **container store** is the executor's underlying buildah storage holding container layer filesystems and images.

The content and record stores are defined as Go interfaces in [cache](../../internal/cache/doc.go):

- `cache.Directory` provides on-disk storage for production builds.
- `cache.Memory` provides an in-memory implementation for automated tests.
- `remote.Client` provides an HTTP proxy client forwarding storage requests to a remote coordinator.

`run.CacheDir` places the on-disk directory store alongside buildah's storage,
typically at `/var/lib/containers/vamos`.
Persisting the `/var/lib/containers` volume preserves both container layers and build metadata.
Client processes obtain their content store handle directly from the executor they connect to.

The following class diagram shows the `cache.ContentStore` and `cache.RecordStore` interfaces,
their implementations, and data structures.

```mermaid
classDiagram
    direction LR
    class ContentStore {
        <<interface>>
        +Has(b Blob)
        +Missing(blobs)
        +Put(r, limit) Blob
        +Import(b, r)
        +Get(b) ReadCloser
        +ReadMany(blobs, limit)
        +Stat(digest) Blob
        +ClearContent() int
    }
    class RecordStore {
        <<interface>>
        +Result(key) ActionResult
        +SetResult(key, r)
        +Tag(name) TagRecord
        +SetTag(name, r)
        +ClearResults() int
        +Generation() uint64
        +History(name) History
        +Newest(name) HistoryHead
        +AddHistory(name, entry, trees, keep)
        +PruneHistory(before) int
    }
    class Directory["cache.Directory"]
    class Memory["cache.Memory"]
    class Client["remote.Client"]
    ContentStore <|.. Directory
    RecordStore <|.. Directory
    ContentStore <|.. Memory
    RecordStore <|.. Memory
    ContentStore <|.. Client

    class Blob {
        Digest
        Size
    }
    class ActionResult {
        Image (image ID)
        Platform
        Index
        Epoch
    }
    class Location {
        where the image bytes are
    }
    class Provenance {
        Platform, Isolation
        Capabilities, Version
        Emulated
        Started, Took
    }
    class History {
        Newest, a fingerprint
        Used
        Trees, files by tree digest
    }
    class HistoryEntry {
        Key, ActionVersion
        Recorded, Action
    }
    class Supplier {
        Target
        Image, Manifest of a pull
        Tree of a source
    }
    class TagRecord {
        Digest
        MIMEType
        Manifest
    }
    class TagInstance {
        Image ID, Platform
        Epoch, Locations
    }
    ActionResult "1" o-- "*" Location : Locations
    ActionResult o-- "0..1" Blob : Output tree
    ActionResult o-- "0..1" Blob : Log
    ActionResult o-- "0..1" Blob : Trace
    ActionResult o-- "0..1" Provenance
    History "1" *-- "*" HistoryEntry : most recent first
    HistoryEntry *-- "*" Supplier : Base, Adds
    TagRecord "1" *-- "*" TagInstance : by platform
    TagInstance o-- "*" Location
    RecordStore ..> ActionResult : by action key
    RecordStore ..> TagRecord : by tag or pin name
    RecordStore ..> History : by workspace and target
```

Record metadata alone cannot prove that referenced image layers or file trees remain present on disk.
Content is therefore verified as it is read:

| Corrupted resource | System response |
| --- | --- |
| Task output files in content store | The task re-executes to regenerate output files |
| Captured workspace source files | The build fails; only the client can re-read local files |
| Cached execution log | The result is marked as a cache miss and re-executes |

When a read operation detects a corrupted blob, the store deletes the corrupted file immediately,
allowing subsequent write operations to restore valid content.

`ActionResult.Trace` names the stored trace of the attempt that made the result,
and `Provenance.Started` and `Took` when that run began and how long it took.
Like `Provenance`, neither is part of the action's key,
and unlike the log, the trace is never checked before reuse:
a result whose trace is gone is reused all the same, and only a trace of it shows the gap.
`ContentStore.Stat` gives a blob's size from its digest alone, without verifying it,
so that a trace named by a bare digest can be read; every other read needs the size.

### Target history

The record store keeps a history for each configured target of a named workspace,
which miss attribution compares a build with.
A `cache.History` record is named by the digest of the workspace's name and the target,
and holds the last actions its builds used, most recent first, each key once,
with what supplied each input, and the files of their sources' trees, shared between entries by tree digest.
It names no content blob, so that it survives a prune, as tag records do.

Results and tags are replaced whole, but history is added to:
`AddHistory` reads, extends and publishes a record under a lock, so that concurrent builds of a target
each add their entry and none drops another's.
`cache.Directory` keeps each record as `history/<hex>`, published by a rename, so reads take no lock;
adds and `PruneHistory` take an in-process lock and then an exclusive `flock` on `history/lock`,
without blocking, retried for up to 10 seconds in all or until the context ends.
A lock an add can't have fails only that add, which the scheduler treats as a span error.
`cache.Memory` does the same under its mutex.
`ClearResults` leaves `history/` alone.

A record is capped at `cache.MaxHistorySize`, 16 MiB, budgeted over the whole record:
an add drops the oldest entries until the record fits,
keeps the newest entry's largest trees by digest alone if it doesn't fit with them,
and adds no entry at all for an action too large even then,
though it still takes that action's fingerprint as the newest and moves `Used`.
The fields are encoded `Newest`, `Used`, then the rest, so that `Newest` reads them without decoding the entries.

The scheduler keeps history for each layer and task of a named workspace whose node names its `Target`,
and attributes each miss from it, as `sched.attribute` orders the causes:
`forced` for a target `--rebuild` named, `lost` where a result was found but couldn't be used,
or history holds the action's key, `new` where history holds no entry of the current `sched.ActionVersion`,
and otherwise the nearest such entry, the most recent among the nearest, decides between `own-inputs`,
`upstream`, a moved base pull alone, and `dependency`, what a supplier gave alone.
A miss reads the history once and adds its entry once the target succeeds;
a hit reads only the history's head, and adds its entry when its fingerprint isn't the newest,
or the record wasn't used for a week, so that `Used` follows reuse within a week.

## Tool caches

A [tool cache](../user/reference/build/tool-cache.md) is a directory that a standalone local executor keeps between
builds and bind-mounts into the `run()` steps of the targets that mount it, such as Go's build cache.
It is storage beside the three stores, never one of them: nothing in it is content-addressed or recorded,
and no key, record or image names what it holds.

**Trust model.** Declaring a cache is the user's assertion that what it holds changes only how long a build takes,
never what it makes, as a `run()` that fetches a pinned URL asserts that the URL is stable.
Vamos verifies nothing of it.
So the key covers how a mount shapes the container, its path, the cache's label, and its owner and group,
and never what the cache holds, whether it holds anything, where the executor keeps it or the workspace's name,
which travel in `sched.Request` as execution metadata.
Because a cache never changes a result, any executor may give any cache empty, and a build succeeds either way:
a coordinator's workers, `vamos coordinator --local` and `--no-tool-caches` all do.

**Layout.** Caches live in `tool-caches/` inside `cache.Directory`, beneath `run.CacheDir`:

```text
tool-caches/
  <sha256 of the workspace name>/
    <sha256 of the canonical label>/
      label                         the label, for diagnostics
      <owner>-<group>-<mapping>/    the rule's numeric IDs and a digest of the builder's ID mappings, or host
        data/                       what is mounted, owned by the rule's owner and group on the host
```

Digests keep `/` and `:` out of directory names, and the workspace's name out of the mount table a step can read.
Each owner, group and ID mapping has a directory of its own, so no directory is ever chowned,
and a builder that maps IDs otherwise finds an empty one rather than files it sees owned by IDs it no longer maps.
A directory is made under a `.tmp-` name beside its place and renamed into it,
so a rename that finds another attempt's in place lost a race, and no attempt sees one half made;
one that a killed process left is never mounted, and stays until the next standalone prune.

The following diagram shows which directory an attempt mounts at each cache path.

```mermaid
flowchart TD
    start{"runner keeps caches?<br/>localRunner.toolCacheRoot"}
    clean{"request asks for<br/>empty caches?<br/>sched.Request.NoToolCaches"}
    name{"workspace name valid?<br/>graph.CheckWorkspaceName"}
    temp["an empty directory of the attempt's own,<br/>in its container's directory,<br/>deleted as the attempt ends"]
    keep["the workspace's cache,<br/>tool-caches/&lt;workspace&gt;/&lt;label&gt;/&lt;owner&gt;-&lt;group&gt;-&lt;mapping&gt;/data"]
    fail["the attempt fails"]
    start -- "no: a worker,<br/>a local coordinator" --> temp
    start -- yes --> clean
    clean -- yes --> temp
    clean -- no --> name
    name -- no --> fail
    name -- yes --> keep
```

**Lock and lifetime.** Builds hold `cache.Directory`'s shared lock while they run,
so a prune, which takes it exclusively, never deletes a cache that a build has mounted.
A standalone prune renames all of `tool-caches/` aside under the exclusive lock and deletes it once it holds the lock
shared again; a local coordinator's prune leaves it, since it holds standalone builds' caches.
An attempt's own empty caches live in its buildah container's directory, on the store's volume,
and the runner deletes them before it removes the container, so that their deletion holds no store lock.
Concurrent attempts share one cache; caches grow until a prune.

The runner checks each cache path where it resolves in the container, before the steps, before each `run()` step and
after the last: see [run](../../internal/run/doc.go) and the tool cache reference's paths.

## Identities

The system maintains distinct identifiers across the build lifecycle:

| Identifier | Subsystem owner | Purpose |
| --- | --- | --- |
| Configured target | `load.confLabel` | Distinguishing targets across configuration variants |
| Graph blob digest | `graph.Encode`, `api.GraphRequest.Graph` | Content address of an immutable build plan |
| Action key | `sched.Action`, `Scheduler.inputs` | Identifying identical target execution inputs for reuse |
| Target identity | `graph.Node.Target`, `load` | A configured target's label, platform and every parameter's value, alike in every invocation |
| Target history name | `sched.HistoryName` | Digest of the workspace's name and the target identity, naming its history record |
| History fingerprint | `cache.HistoryEntry.Fingerprint` | An action's key and its suppliers' identities, which a hit compares with the newest add |
| File tree digest | `cache.Tree` | Addressing file hierarchies independently of file timestamps |
| Image ID / manifest digest | `api.Result`, `cache.TagRecord` | Addressing OCI image configs and registry manifests |

### Action keys

An action key is the sha256 digest of its serialized `sched.Action`.
It incorporates every input that influences the execution outcome, and excludes non-deterministic metadata.
Because action identity is independent of target names or graph position,
renaming a target or moving it within a BUILD file preserves cache hits.

The following diagram contrasts attributes included in an action key with execution metadata
and secret values excluded from it.

```mermaid
flowchart LR
    subgraph inputs["In the key"]
        kind["kind: layer, task, resolve"]
        base["base image ID or scratch"]
        plat["image platform<br/>(layers)"]
        steps["steps, including<br/>source trees and task epochs"]
        cfg["command, entrypoint,<br/>task output selection"]
        sec["secret IDs and how<br/>each is given"]
        tc["tool caches: path, label,<br/>owner and group"]
        ver["ActionVersion"]
    end
    subgraph outside["Outside the key"]
        names["target names, positions"]
        prov["provenance, emulation"]
        runp["platform it runs on"]
        locs["image locations"]
        ts["--timestamp"]
        secv["secret values"]
        tcv["what a tool cache holds,<br/>workspace name"]
    end
    inputs --> key(("action key"))
    outside -. "never" .-x key
```

Secret management follows strict isolation rules:

- An action key incorporates the secret identifier and its mount path or environment variable binding,
  never the secret's actual plaintext value or the host path where `--secret` read it.
- This design ensures that rotating a secret value does not invalidate cached results when commands did not change.
- Plaintext secret values travel exclusively in memory through `api.GraphRequest.Secrets` and `sched.Request.Secrets`.
  They are never serialized to JSON, written to disk records, or transmitted over network sockets,
  and passing `--secret` is rejected when using `--coordinator`.
- During execution, the runner writes each secret value to a temporary file accessible only to its process,
  mounts it into the buildah container, and unlinks the file immediately after the step finishes.

Action results also record a `cache.Provenance` structure detailing the host platform, isolation mode,
capabilities, version, and whether commands ran under emulation.
Provenance is recorded for diagnostics and inspection, but is excluded from action keys to avoid cache fragmentation.

### Image identity and locations

An image's ID is the sha256 digest of its OCI configuration object.
Pushes and pulls preserve this ID, keeping image identity portable across registries and container stores.

The `run` package tracks all images—whether built locally or pulled from registries—using an internal reference
in the cache repository formatted as `<cache-repository>:<image ID>`.
This abstraction keeps the local container store's internal IDs confined to `run`.
Because external operations on container storage could reassign tags, `run` validates every image before reuse:
it confirms that the tag references an image matching the expected ID,
and that its stored layers match the layer digests in its configuration.
If this check fails, the cached result is treated as a cache miss.
The layer validation also rejects images from partial pulls where `insecure_allow_unpredictable_image_contents`
in `storage.conf` allowed unverified layers whose configurations do not match file contents.

An action result records its image ID along with explicit storage locations:

The following diagram shows the four storage location formats recorded in an `ActionResult`.

```mermaid
flowchart LR
    result["ActionResult<br/>Image: sha256:config…"] --> l1["the container store's name,<br/>locally"]
    result --> l2["a pulled base's registry<br/>reference by digest"]
    result --> l3["…at the pull-through cache, where<br/>the coordinator maps its registry to one"]
    result --> l4["a layer built on a worker:<br/>cache repository tagged by image ID"]
```

Storage locations adapt to deployment topologies:

- When running on a worker, the runner pushes built layers to the shared cache registry before reporting completion.
- If a runner subsequently requires an image not present in its local storage,
  it pulls the image from one of the registry locations listed in the action result.
- The client reads directly from registry locations only during remote coordinator builds;
  in local builds, delivery reads exported blobs from the local content store.

## Lockfile pins and repository tags

The project lockfile, `vamos.lock`, read by [load](../../internal/load/doc.go),
is the committed source of truth for base image pulls.
It records the precise manifest digest associated with each repository tag.
The record store tracks two distinct pull records:

| Record type | Lookup key | Stored content | Cache validity |
| --- | --- | --- | --- |
| Pin record | `sched.PinName`: repository name and pinned digest | The image manifest and platform instances | Immutable: a digest reference identifies one manifest permanently |
| Tag record | Repository tag name | Result of resolving an unpinned tag under `--pins=resolve` | Mutable: varies across machines, networks, and time |

A pinned pull reads and writes only its pin record; moving a remote registry tag has no effect on pinned builds.

The following diagram shows the decision process for resolving a pull node from the lockfile
or local record store.

```mermaid
flowchart TD
    pull(["pull node"]) --> pinned{"Pinned in<br/>vamos.lock?"}
    pinned -- "yes" --> pinrec["read the pin's record"]
    pinned -- "no" --> tagrec["read the tag's record"]
    pinrec --> complete{"Complete instance<br/>for the platform, and the<br/>checker accepts it?"}
    tagrec --> complete
    complete -- "yes" --> served["served from the record,<br/>runner asked nothing"]
    complete -- "no" --> puller["runner's puller, given the record,<br/>pulls and validates"]
    puller --> write["Sharing writes the record"]
```

Each platform instance in `cache.TagInstance` records the image ID, platform, timestamp epoch,
and storage locations.
If a cached instance is complete and accepted by the backend `Checker`, `Scheduler.resolve` returns the result
without invoking network operations:

- The local executor's checker requires the image layer to exist in local container storage.
  If the layer was deleted, the pull re-executes.
- The coordinator's checker verifies that an image ID and platform are recorded, without querying registries:
  workers fetch missing base layers on demand through the pull-through cache proxy.
- Within an individual build job, `Scheduler.resolve` pins the tag's multi-platform index manifest;
  all platform images resolve from this single index even if upstream registry tags update concurrently.
- Before accepting a resolved image, the runner validates that the local image matches the manifest digest
  and targets the requested architecture.
  Verifying local images against the recorded manifest digest prevents redundant registry queries.

Cache invalidation is decoupled from tag resolution:
passing `--rebuild` forces re-execution of target roots, but does not re-resolve base image pulls.
To update pinned digests against upstream registries, users run `vamos update`.

## Reproducible timestamps

File trees stored in the content store omit file modification times to ensure reproducible digests.
However, action results preserve an execution epoch timestamp applied to output files.
Base images provide an epoch derived from their image metadata,
and task steps incorporate their epoch into action keys via `sched.Step.Epoch`.
The `--timestamp` flag applies only to newly executed targets;
it does not invalidate existing cache entries, so cached results retain their original epochs.

## Pruning

Pruning purges action results, content blobs, and cached layer images.
It retains resolved tag records and pulled base images (which temporarily lose their cache repository tag
until a subsequent resolve re-establishes the reference).
It keeps target history too, until it goes unused:
`PruneHistory` removes the records whose `Used` is older than `cache.HistoryRetention`, 100 days,
and goes on past a record it fails to remove, which stays for the next prune.
`Used` moves on every add, and a reuse adds at least weekly,
so the history of a target built or reused in the last 90 days survives a prune.

In local builds, pruning acquires an exclusive kernel file lock on `cache.Directory`,
as described in [Scheduling and concurrency](scheduling.md#cache-directory-locking).
Because a remote coordinator cannot hold file locks across distributed clients,
it coordinates pruning through **generation fencing**:

- `RecordStore.Generation` maintains a monotonically increasing integer counter incremented with each prune.
- The coordinator advertises `Generation() + 1` in its capabilities.
  A value of 0 indicates that generation checks are skipped.

The following sequence diagram shows how a coordinator coordinates cache pruning across the content store,
record store, and cache registry while fencing concurrent client submissions.

```mermaid
sequenceDiagram
    participant C as Client
    participant Co as Coordinator
    participant Cs as Content store
    participant Rs as Record store
    participant Reg as Cache registry

    C->>Co: Capabilities
    Co-->>C: Generation g
    C->>Cs: capture sources
    C->>Co: Submit(graph, Generation g)
    Note over Co: checked once admitted,<br/>when no prune can start

    Note over Co: Prune, refused while any job is open<br/>or any attempt may be live
    Co->>Cs: ClearContent
    Co->>Rs: ClearResults, generation advances
    Co->>Reg: delete every tag of the cache repository
    C->>Co: Submit(graph, Generation g)
    Co-->>C: refused: run the command again
```

The coordinator executes pruning operations in strict order:

1. **Blobs are cleared before records**:
   Clearing records advances the generation counter.
   A client submitting under the new generation uploads source blobs to a content store that is no longer being cleared.
2. **Registry tags are deleted last**:
   Deleting tags in the shared cache repository occurs after local records are cleared.
   If tag deletion fails midway, the remaining images are unreferenced orphans that cause no cache collisions.
3. **Pulls and proxy caches are preserved**:
   Tag records and pull-through caches remain intact, matching local pruning behavior.

Any report submitted by an attempt that started under an earlier store generation is rejected.
