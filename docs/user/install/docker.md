---
title: "Run with Docker"
description: "Run vamos in a container on a Docker host, without installing it."
---

vamos can run in a privileged container on a Docker host, without being installed there.
Build its image from the repository root:

```sh
docker build -t vamos .
```

Or, without a checkout, use the [published image](https://github.com/vamos-build/vamos/pkgs/container/vamos):

```sh
docker pull ghcr.io/vamos-build/vamos:0.1.0
docker tag ghcr.io/vamos-build/vamos:0.1.0 vamos
```

On the Docker host, define this shell function:

```sh
vamos() {
    docker run --rm --privileged \
        -v vamos-cache:/var/lib/containers \
        -v /var/run/docker.sock:/var/run/docker.sock \
        -v "$PWD:/project" \
        -w /project \
        vamos "$@"
}
```

This assumes a Docker daemon at `/var/run/docker.sock` and a host directory Docker can mount.
The named volume preserves the build cache between invocations.
Run the function from your project directory; paths passed to vamos refer to the container filesystem.

If you are already inside a devcontainer, install vamos in its image instead,
from [Ubuntu](dockerfile-ubuntu.md) or [Fedora](dockerfile-fedora.md),
because the paths in the devcontainer differ from the Docker host's.
