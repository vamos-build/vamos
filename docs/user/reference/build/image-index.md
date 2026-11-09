---
title: "index()"
description: "Bundling single-platform images into a multi-platform OCI image index."
---

```python
index(
    name = "release",
    layer = "app",
    platforms = ["linux/amd64", "linux/arm64"],
)
```

`index()` bundles multiple single-platform images into a multi-platform OCI image index.
`name` and `layer` are required.
When `layer` names a single pull or layer, `platforms` must be a nonempty list.
vamos builds that target once for each platform and preserves the listed order in the index.

To use distinct targets for each platform, pass a dictionary to `layer` and omit `platforms`:

```python
index(
    name = "release",
    layer = {
        "linux/amd64": "app-amd64",
        "linux/arm64": "app-arm64",
    },
)
```

Platforms must be unique.
A `dep()` in an index can pass user parameters but cannot specify `platform`,
because the platform is determined by the index.
An index cannot contain another index.
