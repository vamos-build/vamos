---
title: "vamos prune"
description: "Removing cached build results."
---

```sh
vamos prune
```

Removes all cached layer images and build artifacts.
It retains pulled base images, recorded tag resolutions, and pins in `vamos.lock`.
To refresh a pulled image, run [`vamos update <pull-name>`](update.md).
