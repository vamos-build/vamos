---
title: "Dockerfile from Ubuntu"
description: "Install Vamos in a container image built from Ubuntu 24.04, such as a devcontainer or CI image."
---

Add these lines to a Dockerfile built from Ubuntu 24.04 or later:

```dockerfile
FROM ubuntu:24.04

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      golang-github-containers-common \
      netavark \
 && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL -o /tmp/vamos-install.sh https://github.com/vamos-build/vamos/releases/download/v0.1.0/install.sh \
 && sh /tmp/vamos-install.sh \
 && rm /tmp/vamos-install.sh

# Run build steps with chroot isolation, so no OCI runtime is needed.
ENV BUILDAH_ISOLATION=chroot
```

Combine the package installation with your existing `apt-get install` step.
Ubuntu names Fedora's `containers-common` package `golang-github-containers-common`.
If a package is missing, [install.sh](README.md#what-installsh-does) installs Vamos
but fails the build, printing the `apt-get` command that installs it.

Run the container privileged with a volume for `/var/lib/containers`;
for a devcontainer, [configure devcontainer.json](README.md#configure-a-devcontainer).
Then check the installation:

```sh
vamos --version
```
