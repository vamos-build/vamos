---
title: "Dockerfile from Fedora"
description: "Install Vamos in a container image built from Fedora, such as a devcontainer or CI image."
---

Add these lines to a Dockerfile built from Fedora:

```dockerfile
FROM fedora:45

RUN dnf -y install \
      ca-certificates \
      containers-common \
      curl \
      netavark \
 && dnf clean all

RUN curl -fsSL -o /tmp/vamos-install.sh https://github.com/vamos-build/vamos/releases/download/v0.1.0/install.sh \
 && sh /tmp/vamos-install.sh \
 && rm /tmp/vamos-install.sh

# Run build steps with chroot isolation, so no OCI runtime is needed.
ENV BUILDAH_ISOLATION=chroot
```

Combine the package installation with your existing `dnf install` step.
If a package is missing, [install.sh](README.md#what-installsh-does) installs Vamos
but fails the build, printing the `dnf` command that installs it.

Instead of running `install.sh`, you can copy the binary from the published image,
which is built on Fedora:

```dockerfile
COPY --from=ghcr.io/vamos-build/vamos:0.1.0 /usr/local/bin/vamos /usr/local/bin/vamos
```

Run the container privileged with a volume for `/var/lib/containers`;
for a devcontainer, [configure devcontainer.json](README.md#configure-a-devcontainer).
Then check the installation:

```sh
vamos --version
```
