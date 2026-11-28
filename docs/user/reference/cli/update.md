---
title: "vamos update"
description: "Pinning pulls to manifest digests in vamos.lock."
---

```sh
vamos update            # update and pin all pulls in the workspace
vamos update node base  # update only the specified pull targets
```

Resolves pull targets against container registries and records pinned digests in `vamos.lock` at the workspace root.
Subsequent builds read digests from `vamos.lock` instead of resolving tags against registries;
see [`pull()`](../build/pull.md).

## Tag resolution

For each pull target, Vamos queries the remote registry for available repository tags.
It resolves the target's tag to the latest stable release matching the tag's version prefix,
preserving leading `v` prefixes and custom suffixes:

- Tag `1.3` resolves to the latest `1.3.x`.
- Tag `24-alpine` resolves to the latest `24.x.y-alpine`.
- Non-version tags (such as `latest`), prereleases, and explicit patch versions resolve to their own current digest.

Vamos fetches the manifest digest (for an image or multi-platform index) and records the pin in `vamos.lock`.
If a registry forbids listing tags, the specified tag resolves directly to its current digest.

## Workspace discovery

Running `vamos update` without arguments discovers all `BUILD` files across the workspace,
skipping hidden directories, linked directories and other workspaces, those with a `WORKSPACE` file of their own.
It checks the rule of each of their targets, and of each target those use as given, as building it would,
and of a target given a function of `ctx`, the targets it is given as they are:
a mistake there, or targets that name each other there in a cycle, fails the update.
It then configures every layer, task, index and publication as building it would, loading the packages it uses,
including those that only a function of `ctx`, or a label given to `add()` or `dep()`, reaches.
A pull of a package the walk found is pinned in the configuration that the command line gives it
and in each that the targets using it give it;
another is pinned in each that the targets using it give it, or else in the command line's.

It removes the pins that no pull uses, unless one may still be used:
by a pull given a function of `ctx`, which may pull another image in another configuration,
or by a target it couldn't configure, or a package it couldn't load, which may use a pull it didn't find.
Then it removes no pin, and says why it keeps those.
A function of `ctx` that names a target only in a configuration that the update doesn't configure,
such as only for another platform, names nothing the update follows,
so a pin that only that target's pulls use is removed.
Naming specific targets updates only the specified pulls.

`update` prints each pin added, changed, or removed:

```text
pinned docker.io/library/node:24-alpine to 24.11.1-alpine@sha256:9f3c...
updated ghcr.io/me/base:1.3 from 1.3.1@sha256:5d0e... to 1.3.2@sha256:1a2b...
removed ghcr.io/me/old:1, which no pull uses, from 1@sha256:77c4...
docker.io/library/node:24-alpine has a newer major release, 25-alpine
ghcr.io/me/base:1.3 has a newer minor release, 1.5, and a newer major release, 2.1
```

When it keeps the pins that no pull uses, it says why:

```text
kept ghcr.io/me/tool:2, which no pull uses here, since "//ui:app", which couldn't be configured as building it would, may use a pull that does
```

If a pull target fails to resolve, no changes are written to `vamos.lock`.

## Dry run and CI checks

Pass `--dry-run` to check whether `vamos.lock` is up to date without writing changes:

```sh
vamos update --dry-run
```

If any pins would change, `--dry-run` reports the differences and exits with status 1.
Use `--dry-run` in CI workflows alongside `VAMOS_PINS=require` to ensure all pulls are pinned and current.

The lockfile is formatted as sorted JSON:

```json
{
  "version": 1,
  "pulls": {
    "docker.io/library/node:24-alpine": {
      "tag": "24.11.1-alpine",
      "digest": "sha256:9f3c..."
    }
  }
}
```

Commit `vamos.lock` to source control so all developers and CI runners build identical images.
