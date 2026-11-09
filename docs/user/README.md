---
title: "vamos user guide"
description: "Introduction to vamos, core concepts, and reading order for the documentation."
---

vamos builds container images from Starlark `BUILD` files using container isolation.
It combines multi-stage build pipelines, fine-grained caching, and reproducible outputs.

Build steps run inside disposable containers using your project's native tools, such as pnpm, Go, or uv.
Each target specifies its base image, inputs, and build steps.
Tasks collect only the files you select, keeping compilers and intermediate tools out of the finished image.
vamos caches the result of every target and reuses it when its inputs have not changed.

These two targets from the TypeScript UI example build a website with pnpm,
then serve it from nginx without Node.js in the final image:

```python
task(
    name = "build",
    base = "packages",
    steps = [
        add("source-files", "/workspace"),
        run(["pnpm", "build"]),
    ],
    source = "/workspace/dist",
    include = ["."],
)

layer(
    name = "frontend",
    base = "nginx",
    steps = [add("build", "/usr/share/nginx/html")],
)
```

The `build` task compiles the website inside a Node.js container
and collects only the static files from `/workspace/dist`.
The `frontend` layer places those files into an nginx image.
The final image contains only nginx and the static assets;
Node.js and build tools remain in the build cache.

## Where to start

Start with the [tutorial](tutorial.md).
It introduces images and filesets, walks through setting up vamos,
and builds the [TypeScript UI example](https://github.com/vamos-build/vamos/blob/main/examples/typescript-ui/README.md)
into a running container.

## Documentation index

Use these references when writing your own project:

| Reference | Contents |
| --- | --- |
| [Tutorial](tutorial.md) | Hands-on walkthrough building an application from source into a running container |
| [BUILD language](reference/build/README.md) | Target declarations (`pull`, `glob`, `task`, `layer`, `index`, `publication`), build steps, secrets, and file selection |
| [CLI](reference/cli/README.md) | Commands for building, publishing, loading into Docker, saving archives, and exporting files |
| [Configuration](reference/configuration.md) | Multi-package workspaces, target parameters, cross-platform builds, and shared `.star` files |
| [Build cache and traces](reference/cache.md) | Cache keys, rebuilding, dependency pinning, storage pruning, and performance traces |
| [Remote execution](reference/remote.md) | Distributing builds across a coordinator and worker pool on a shared network |

See the [examples directory](https://github.com/vamos-build/vamos/blob/main/examples/README.md)
for complete projects demonstrating Go services, TypeScript frontends, and cross-compilation.
