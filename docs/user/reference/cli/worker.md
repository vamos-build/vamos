---
title: "vamos worker"
description: "Running a build worker managed by a coordinator."
---

```sh
vamos worker --coordinator http://coordinator:8080 --capacity 2
```

Runs a worker daemon that executes target build attempts assigned by a coordinator.
Each target attempt runs inside an isolated cgroup and chroot environment.

| Flag | Description | Default |
| --- | --- | --- |
| `--coordinator <URL>` | URL of the coordinator to register with | Required |
| `--capacity <n>` | Maximum concurrent target attempts to execute | `1` |

Workers require root privileges, cgroup v2, a dedicated cgroup namespace, and chroot isolation.
See the [Remote execution reference](../remote.md#worker-requirements) for complete worker requirements.
