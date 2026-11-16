---
title: "vamos prune"
description: "Removing cached build results."
---

```sh
vamos prune
```

Removes all cached layer images and build artifacts, stored traces among them.
It retains pulled base images, recorded tag resolutions, and pins in `vamos.lock`.
Target history, which [explains why a target missed](../cache.md#why-a-target-missed), is kept
at least 90 days after its target was last built or reused, and a prune removes it once its target has been neither
built nor reused for 100 days.
A prune reports how many results, files and target histories it removed, and warns about any history it couldn't
remove, which the next prune tries again.
To refresh a pulled image, run [`vamos update <pull-name>`](update.md).
