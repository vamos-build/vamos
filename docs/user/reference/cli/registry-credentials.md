---
title: "Registry credentials"
description: "How vamos finds credentials for container registries, including inside Docker."
---

Registry operations use your local container registry credentials, including `~/.docker/config.json`.
Set `$REGISTRY_AUTH_FILE` to specify a custom authentication file path.

When running vamos inside Docker, mount your authentication file into the container:

```sh
-v "$HOME/.docker/config.json:/root/.docker/config.json:ro"
```

If your configuration relies on a credential helper, that helper binary must be installed inside the container.
Alternatively, mount a static auth file and point `REGISTRY_AUTH_FILE` to its path:

```sh
-v "$PWD/auth.json:/run/registry-auth.json:ro" -e REGISTRY_AUTH_FILE=/run/registry-auth.json
```
