---
title: "vamos push"
description: "Pushing an image or index to a container registry."
---

```sh
vamos push app ghcr.io/me/app:1.0
vamos push release ghcr.io/me/app:1.0 --push-from=client
```

Publishes an image or index to a destination container registry.
Image references without an explicit tag default to `:latest`.
Destination names must be lowercase image references without a digest.

- `--push-from=executor` (default): the build engine pushes using its own environment,
  credentials, and `registries.conf`.
- `--push-from=client`: pushes directly from the client process
  using the client's credentials and `--tls-verify` settings.

Pushes retry transient registry errors according to `containers.conf`.
After pushing, vamos reads the tag back from the destination registry (bypassing mirrors)
to verify that the tag points to the expected manifest digest.
