---
title: "pull()"
description: "Pulling base images from registries, pinning, and tag resolution."
---

```python
pull(name = "node", image = "docker.io/library/node", tag = "24-alpine")
```

| Argument | Description | Default |
| --- | --- | --- |
| `name` | Target name | Required |
| `image` | Repository path without a tag or digest | Required |
| `tag` | Repository tag name | `"latest"` |

`pull()` retrieves an image matching the configured target platform.
If the tag points to a multi-platform index, vamos selects the matching platform image.
A single-platform image must match the configured platform; a mismatch fails the build.
Use fully qualified image names (such as `docker.io/library/node`) to ensure unambiguous registry resolution.
Place tags in the `tag` argument rather than embedding them in `image`.
Pulls do not take user parameters; they are configured only by target platform.

## Pinning and resolution

When the lockfile (`vamos.lock` at the workspace root) pins an image and tag,
`pull()` reads the manifest digest directly from the pin.
It selects the matching platform image from that digest without querying the registry.
If the pinned digest no longer exists in the registry,
the build fails and prompts you to run [`vamos update`](../cli/update.md).

When a pull is not pinned (for example, under [`--pins=resolve`](../cli/flags.md#pin-policies)),
vamos resolves the tag against the registry on first use and caches the result.
Subsequent builds reuse that cached resolution, even when `--rebuild` is specified,
until [`vamos update`](../cli/update.md) pins the pull.
A tag that names an index is resolved once:
subsequent builds for other platforms pull their image from that cached index digest.

If the registry does not contain the requested repository, or denies access,
vamos fails with an error suggesting to verify the name or log in with credentials.
If the registry is unreachable over the network, vamos reports the network failure.
