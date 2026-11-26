---
title: "Install on Fedora"
description: "Install vamos directly on a Fedora host."
---

These steps install vamos on a Fedora host or virtual machine.

Install the packages vamos needs at run time:

```sh
sudo dnf install -y ca-certificates containers-common curl netavark
```

Install the latest release:

```sh
curl -fsSL https://github.com/vamos-build/vamos/releases/latest/download/install.sh | sh
```

[install.sh](README.md#what-installsh-does) installs vamos as `/usr/local/bin/vamos`,
through `sudo` if you are not root.
Run build steps with chroot isolation, so no OCI runtime is needed,
by adding this line to your shell profile:

```sh
export BUILDAH_ISOLATION=chroot
```

vamos needs root, so run it with `sudo`, keeping that variable:

```sh
sudo --preserve-env=BUILDAH_ISOLATION vamos --version
```

The build cache is kept in `/var/lib/containers`.
`vamos load` also needs Docker, with its daemon at `/var/run/docker.sock`.

To build images for other architectures, install the QEMU emulators,
which register themselves with the kernel at boot:

```sh
sudo dnf install -y qemu-user-static
```
