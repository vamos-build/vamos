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

For each pull target, vamos queries the remote registry for available repository tags.
It resolves the target's tag to the latest stable release matching the tag's version prefix,
preserving leading `v` prefixes and custom suffixes:
- Tag `1.3` resolves to the latest `1.3.x`.
- Tag `24-alpine` resolves to the latest `24.x.y-alpine`.
- Non-version tags (such as `latest`), prereleases, and explicit patch versions resolve to their own current digest.

vamos fetches the manifest digest (for an image or multi-platform index) and records the pin in `vamos.lock`.
If a registry forbids listing tags, the specified tag resolves directly to its current digest.

## Workspace discovery

Running `vamos update` without arguments discovers all `BUILD` files across the workspace,
configures each pull target, and removes pins for pulls no longer referenced in the project.
Naming specific targets updates only the specified pulls.

`update` prints each pin added, changed, or removed:

```text
pinned docker.io/library/node:24-alpine to 24.11.1-alpine@sha256:9f3c...
updated ghcr.io/me/base:1.3 from 1.3.1@sha256:5d0e... to 1.3.2@sha256:1a2b...
removed ghcr.io/me/old:1, which no pull uses, from 1@sha256:77c4...
docker.io/library/node:24-alpine has a newer major release, 25-alpine
ghcr.io/me/base:1.3 has a newer minor release, 1.5, and a newer major release, 2.1
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
