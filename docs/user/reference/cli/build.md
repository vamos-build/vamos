---
title: "vamos build"
description: "Building targets into the build cache, and forcing targets to rerun."
---

```sh
vamos build lint test app
vamos build //backend:test //ui:lint -C path/to/workspace
vamos build test --rebuild
```

`vamos build` compiles one or more targets into the build cache.
An index builds all its constituent platform images.
A [publication](../build/publication.md) builds all its checks and images without publishing, serving as a dry run.
Check tasks that produce no output are cached on success.

The `--rebuild` flag forces explicitly named targets to rerun while allowing unchanged dependencies to be reused.
Named pulls are not re-queried against registries during `--rebuild`;
use [`vamos update`](update.md) to refresh pulls.
See [refreshing results](../cache.md#refreshing-results).
