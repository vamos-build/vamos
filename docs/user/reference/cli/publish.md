---
title: "vamos publish"
description: "Running a publication's checks and publishing its images under release tags."
---

```sh
vamos publish release --tag 1.4.2 --tag 1.4 --tag latest
vamos publish release --tag v1 --set env=production --push-from=client
```

Executes a [publication](../build/publication.md) release.
All checks and images are built in a single invocation.
If every check succeeds, each image or index is pushed to its assigned repositories under every tag specified.
`--tag` is required and can be repeated.
Publishing fails before building if tags are missing, invalid, duplicate,
or if the total number of tag destinations exceeds 1,000.
Naming a target that is not a publication fails immediately.
Like [`vamos push`](push.md), the executor pushes by default;
pass `--push-from=client` to push using the client's credentials and `--tls-verify` settings.

Publishing proceeds in two distinct phases:
1. **Upload by digest**: Every image is uploaded to each destination repository by manifest digest without tags.
   If an upload fails, existing repository tags remain untouched.
2. **Tag assignment**: Once all uploads succeed, tags are written and verified by reading them back.
   If an error occurs during tagging, all remaining tags are still attempted.
   The command reports which tags succeeded and which failed; rerunning the command finishes untagged destinations
   without re-uploading existing layer blobs.

On completion, vamos prints a summary line for each repository and tag:

```text
Image built! ghcr.io/team/app:1.4.2 sha256:abcd...
Index built! ghcr.io/team/app:latest sha256:1234...
```
