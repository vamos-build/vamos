---
title: "vamos coordinator"
description: "Running a coordinator that schedules builds across workers."
---

```sh
vamos coordinator --listen :8080 --cache-repository registry:5000/vamos-cache --pull-through docker.io=hub-proxy:5000
```

Runs a centralized build coordinator that distributes target execution across registered workers.
Clients connect using `--coordinator http://host:8080` (or `VAMOS_COORDINATOR`)
and upload workspace source files to the coordinator's `--cache-dir` (`/var/lib/vamos` by default).
The coordinator schedules concurrent targets across workers
and shares identical target builds across concurrent client requests.

| Flag | Description | Default |
| --- | --- | --- |
| `--listen <addr>` | Address and port to bind | `:8080` |
| `--cache-dir <dir>` | Directory for coordinator state, logs, and captured files | `/var/lib/vamos` |
| `--cache-repository <repo>` | OCI registry repository where workers push built layers | None |
| `--pull-through <reg=cache>` | Mapping from upstream registry to pull-through proxy cache | `[]` |
| `--bound <n>` | Maximum concurrent targets scheduled per build | `4` |
| `--max-builds <n>` | Maximum concurrent builds accepted | `64` |
| `--grace <duration>` | Time to keep disconnected builds running | `5m` |
| `--window <duration>` | Retention window for finished build results | `--grace` |
| `--lease <duration>` | Heartbeat expiration timeout for worker attempts | `30s` |
| `--retries <n>` | Maximum worker retry attempts for lost tasks | `2` |
| `--offer <platform>` | Platform the cluster supports beyond registered workers | `[]` |
| `--offer-emulated <platform>` | Platform supported via worker QEMU emulation | `[]` |
| `--default-exec-platform <platform>` | Default execution platform for clients (must also be passed to `--offer`) | None |

See the [Remote execution reference](../remote.md) for architecture details, network setup, and deployment guides.
