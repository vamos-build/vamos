---
title: "Output and troubleshooting"
description: "Progress states, confirmation messages, errors, and common problems."
---

Build output reports progress states for each target:
- `building`: Target is currently executing.
- `cached`: Target inputs have not changed; previous output is replayed.
- `rebuilding`: Target was forced to rerun via `--rebuild`.
- `building, shared with`: Target shares identical execution with another concurrent target or build.

In interactive terminals, `cached` is dimmed and `building` is colored green.
Delivery commands print a confirmation on success:

```text
Image built! <destination> <image-id>
Index built! <destination> <digest>
Files exported! <folder> <digest>
```

Errors display a `vamos:` prefix and exit with status 1.
Syntax and configuration errors in `BUILD` files report the file path, line, and column.

## Common problems

| Symptom | Cause and resolution |
| --- | --- |
| `Operation not permitted` during namespace creation | Host lacks container privileges; follow the [tutorial setup](../../tutorial.md#2-set-up-vamos) |
| `exec format error` in a build step | Foreign binary architecture; register QEMU emulators on the Docker host |
| An `add()` source is rejected | The source target must be a `glob()` or a `task()` declaring `source` and `include` |
| A glob matches no files | Check file paths relative to the `BUILD` file and review exclusions |
| Export refuses a nonempty folder | Destination directory already contains files; pass `--replace` or choose an empty directory |
| Registry tag points to an older image | The image is pinned; update the pin with [`vamos update`](update.md) and commit `vamos.lock` |
| Docker daemon cannot be reached | Verify the socket mount, `$DOCKER_HOST`, and Docker TLS environment variables |
| `can't reach the coordinator at ...` | Verify the URL in `--coordinator` and check network connectivity |
| `the coordinator ... runs graph versions ..., not this vamos's` | Version mismatch; upgrade vamos or the coordinator to matching versions |
| `waiting for a worker that runs ...; none is registered` | No worker matching the requested platform is registered; start a worker for that architecture |
| `its commands would run under emulation` | Target architecture differs from worker architecture; pass `--emulate` or provide native workers |
| `the connection to the coordinator was lost for ...` | Client disconnected longer than `--grace`; rerun the command to resume from cache |
| `the build expired` | Build went unwatched past the coordinator grace period; rerun the command |
