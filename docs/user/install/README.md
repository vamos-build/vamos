---
title: "Install Vamos"
description: "Requirements for running Vamos, and how to install it in a container image or on a host."
---

Vamos runs on Linux and uses Buildah, so it needs root and container privileges.
Each [release](https://github.com/vamos-build/vamos/releases) publishes the binary for `linux/amd64` and `linux/arm64`,
a `SHA256SUMS` file, and an `install.sh` script that installs the binary.
The binary requires glibc 2.34 or later.

To follow the [fundamentals](../fundamentals.md), use the repository's devcontainer or [run Vamos with Docker](docker.md).

## Choose a setup

| Setup | Use it when |
| --- | --- |
| [Run with Docker](docker.md) | You want to try Vamos on a Docker host without installing it |
| [Dockerfile from Ubuntu](dockerfile-ubuntu.md) | Your devcontainer or CI image is built `FROM ubuntu` |
| [Dockerfile from Fedora](dockerfile-fedora.md) | Your devcontainer or CI image is built `FROM fedora` |
| [Install on Ubuntu](ubuntu.md) | You build directly on an Ubuntu host or VM |
| [Install on Fedora](fedora.md) | You build directly on a Fedora host or VM |

The [published image](https://github.com/vamos-build/vamos/pkgs/container/vamos) has the same binary,
at `/usr/local/bin/vamos`, if you prefer to `COPY --from=ghcr.io/vamos-build/vamos:0.1.0` it into your own image.
It is built on Fedora and has the runtime packages already.

## What install.sh does

`install.sh` downloads the binary for the host platform, verifies it against `SHA256SUMS`,
and installs it as `/usr/local/bin/vamos`.
Run as a user other than root, it installs through `sudo`.
It requires `curl` (or `wget`) and `ca-certificates` to download the release.

It does not install packages itself:
it checks for the packages Vamos needs at run time and prints the `apt-get` or `dnf` command that installs any missing.
If packages are missing, the script installs the binary and exits with status 1,
which stops a Docker build before the image is finished.
Pass `--allow-missing` or set `VAMOS_ALLOW_MISSING=1` to skip this check.

| Option | Environment variable | Effect |
| --- | --- | --- |
| `--version <version>` | `VAMOS_VERSION` | Install that release, such as `0.1.0`, or `latest`; each release's copy defaults to itself |
| `--dir <directory>` | `VAMOS_INSTALL_DIR` | Install into that directory instead of `/usr/local/bin` |
| `--allow-missing` | `VAMOS_ALLOW_MISSING=1` | Succeed even when runtime packages are missing |

## Configure a devcontainer

When Vamos runs in a devcontainer, run the container privileged,
mount a dedicated volume for container storage,
and mount the Docker socket if you use `vamos load`.
In `.devcontainer/devcontainer.json`:

```json
"runArgs": ["--privileged"],
"mounts": [
  "source=vamos-storage,target=/var/lib/containers,type=volume",
  "source=/var/run/docker.sock,target=/var/run/docker.sock,type=bind"
]
```

The named volume retains the build cache when the devcontainer is rebuilt.
Run Vamos as root, or with `sudo` if your devcontainer user is non-root.

## Build for other architectures

To build images for other architectures, register QEMU emulators with the kernel that runs the build.
Docker Desktop registers emulators automatically.
On a Linux Docker host, register emulators after each boot:

```sh
docker run --privileged --rm tonistiigi/binfmt --install all
```

Without Docker, install your distribution's `qemu-user-static` package, as the host install pages show.

## Troubleshooting

Each environment requirement corresponds to a specific failure symptom:

| Requirement | Purpose | Error without it |
| --- | --- | --- |
| `ca-certificates` | Trust certificate authorities for registry TLS | `x509: certificate signed by unknown authority` |
| `containers-common` (`golang-github-containers-common` on Ubuntu) | Provide `/etc/containers/policy.json` and `registries.conf` | `no policy.json file found` |
| `netavark` | Configure network interfaces for `run` steps | `could not find "netavark"` |
| `BUILDAH_ISOLATION=chroot` | Execute build steps without an OCI runtime | `did not get container create message from subprocess` |
| Volume for `/var/lib/containers` in a container | Provide overlay storage compatible with nested containers | `Failed to built-in GetDriver graph btrfs`, followed by slow `vfs` storage |
| Emulators registered | Execute steps for foreign architectures | `exec format error` |
