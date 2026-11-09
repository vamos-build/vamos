---
title: "vamos load"
description: "Loading an image into a local Docker daemon."
---

```sh
vamos load app app:dev
docker run --rm app:dev
```

Loads an image into a local Docker daemon with the specified repository name and tag (defaulting to `:latest`).
The image name must be lowercase without a digest.
vamos connects to `$DOCKER_HOST` or `/var/run/docker.sock` using Docker TLS settings
(`$DOCKER_CERT_PATH`, `$DOCKER_TLS_VERIFY`).

When loading an index target, vamos queries Docker for its platform
and builds only the image matching the Docker daemon's native architecture.
For single-image targets, the platform defaults to the host unless overridden by `--platform`.
