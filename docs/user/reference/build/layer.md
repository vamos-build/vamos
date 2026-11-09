---
title: "layer()"
description: "Building container images from a base image and steps."
---

```python
layer(
    name = "app",
    base = "runtime",
    steps = [add("compile", "/app"), workdir("/app")],
    command = ["node", "main.js"],
)
```

| Argument | Description | Default |
| --- | --- | --- |
| `name` | Target name | Required |
| `base` | A pull or layer reference, or `"scratch"` | Required |
| `steps` | List of steps executed in order | `[]` |
| `command` | Default container command as an argument list | Inherited from base |
| `entrypoint` | Container entrypoint as an argument list | Inherited from base |

A layer retains its base image's files and configuration, then applies its own steps.
`command` configures the image's default runtime command; it does not execute during the build.
As in Docker, `command` provides default arguments to the entrypoint.
An omitted or empty `command` preserves the inherited command.

`entrypoint` sets the default executable for the image:

```python
layer(
    name = "server",
    base = "jre",
    steps = [add("jar", "/app")],
    entrypoint = ["java", "-jar", "/app/server.jar"],
)
```

Passing runtime arguments (`docker run server --port 8080`) appends them to the entrypoint.
Specifying `entrypoint` clears any inherited base command.
Specify `command` alongside `entrypoint` to provide default arguments.
Setting `entrypoint = []` removes any inherited entrypoint.
A layer's base image must have the same platform as the layer.
