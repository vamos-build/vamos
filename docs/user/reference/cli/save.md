---
title: "vamos save"
description: "Writing an image archive that docker load can import."
---

```sh
vamos save app app.tar --tag app:dev
docker load -i app.tar
docker run --rm app:dev
```

Exports a container image as a tarball archive compatible with `docker load`.
The `--tag <image>` flag assigns a repository name and tag within the archive.
The destination file is replaced atomically after the build completes.
Its parent directory must exist, and the destination path cannot be a directory.

When saving an index target, vamos builds and saves only the image matching the host platform.
To export a specific foreign platform, name the pull or layer target directly and pass `--platform`.
`write` is a deprecated alias for `save`.
