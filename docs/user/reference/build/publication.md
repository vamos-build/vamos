---
title: "publication()"
description: "Declaring releases: checks to pass and images to publish."
---

```python
publication(
    name = "release",
    checks = ["lint", "test"],
    images = {
        "ghcr.io/team/app": "app",
        "docker.io/team/app": "app",
        "ghcr.io/team/worker": "worker",
    },
)
```

| Argument | Description | Default |
| --- | --- | --- |
| `name` | Target name | Required |
| `images` | Dictionary mapping destination repositories to pulls, layers, or indexes | Required |
| `checks` | List of tasks that must succeed before publishing | `[]` |

A publication defines a release: what checks must succeed, and which images are published to which repositories.
[`vamos publish`](../cli/publish.md) executes all checks and builds all images in one operation.
If all checks and builds succeed, it publishes each image under every `--tag` specified on the command line.
Running `vamos build` on a publication verifies the checks and builds without pushing images.

`images` is keyed by destination repository without a tag or digest.
Each key must be a unique, lowercase repository name.
Keying by repository allows one target to be pushed to multiple registries (such as mirrors),
while ensuring each destination repository receives a single image.
Each check must be a `task()`, and each image must be a `pull()`, `layer()`, or `index()`.
Dependencies inside `publication()` can use `dep()` to pass parameters.

Publications accept `params` set with `--set`.
Their `checks` and `images` can be defined as functions of `ctx`:

```python
publication(
    name = "release",
    params = {"env": param(default = "staging", values = ["staging", "production"])},
    images = lambda ctx: {
        "registry.example/" + ctx.env + "/app": "app",
    },
)
```

A publication is a top-level target: no other target can reference it as a base,
an `add()` source, an index image, or a check.
Publications are not cached, so running `vamos publish` always attempts publication,
while their underlying checks and images reuse cached results as usual.
