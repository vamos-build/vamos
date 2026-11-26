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

Each attempt reports what its processes used, read from its cgroup as it ends: its CPU time, and its peak memory
where the memory controller is delegated to the worker's cgroup. The coordinator records both on the attempt's trace
and in its metrics.

## Telemetry

A worker exports its own metrics over OpenTelemetry, as the standard `OTEL_*` environment variables configure it when
it starts; with none set, it exports nothing. It exports no traces: its coordinator exports its attempts' spans.

| Variable | Effect |
| --- | --- |
| `OTEL_METRICS_EXPORTER` | `otlp`, `console` or `none`; unset, `otlp` if an OTLP endpoint is set, and `none` otherwise. `prometheus` fails the start, since workers export over OTLP |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | The collector, which turns metric export on |
| `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_EXPORTER_OTLP_HEADERS` | As for the coordinator |
| `OTEL_METRIC_EXPORT_INTERVAL`, `OTEL_METRIC_EXPORT_TIMEOUT` | How often, in milliseconds, metrics are exported, a minute by default, and how long an export may take |
| `OTEL_SDK_DISABLED` | `true` turns every export off |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | The resource the metrics are exported as; its instance is the host's name |

`OTEL_TRACES_EXPORTER`, set to an exporter other than `none`, is ignored, with a warning, unless
`OTEL_SDK_DISABLED=true` turns everything off. The worker counts `vamos.worker.fenced`, running attempts
it killed as their lease couldn't be renewed; `vamos.worker.events.dropped`, batches of an attempt's output and spans
the coordinator didn't take, but for those of an attempt it no longer counts; and `vamos.worker.cgroups.stuck`,
attempts' cgroups a kill couldn't empty in time, once each. Each is exported from zero.

Workers require root privileges, cgroup v2, a dedicated cgroup namespace, and chroot isolation.
See the [Remote execution reference](../remote.md#worker-requirements) for complete worker requirements.
