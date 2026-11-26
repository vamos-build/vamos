---
title: "Install on Ubuntu"
description: "Install Vamos directly on an Ubuntu 24.04 or later host."
---

These steps install Vamos on an Ubuntu 24.04 or later host or virtual machine.
Earlier releases have a glibc older than 2.34, which the binary requires.

Install the packages Vamos needs at run time:

```sh
sudo apt-get update
sudo apt-get install -y --no-install-recommends \
    ca-certificates curl golang-github-containers-common netavark
```

Install the latest release:

```sh
curl -fsSL https://github.com/vamos-build/vamos/releases/latest/download/install.sh | sh
```

[install.sh](README.md#what-installsh-does) installs Vamos as `/usr/local/bin/vamos`,
through `sudo` if you are not root.
Run build steps with chroot isolation, so no OCI runtime is needed,
by adding this line to your shell profile:

```sh
export BUILDAH_ISOLATION=chroot
```

Vamos needs root, so run it with `sudo`, keeping that variable:

```sh
sudo --preserve-env=BUILDAH_ISOLATION vamos --version
```

The build cache is kept in `/var/lib/containers`.
`vamos load` also needs Docker, with its daemon at `/var/run/docker.sock`.

To build images for other architectures, install the QEMU emulators,
which register themselves with the kernel at boot:

```sh
sudo apt-get install -y qemu-user-static
```
