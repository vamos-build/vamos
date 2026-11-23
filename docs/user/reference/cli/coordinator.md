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

## Telemetry

The coordinator exports each job's trace over OpenTelemetry, as the standard `OTEL_*` environment variables
configure it when it starts; with none set, it exports nothing. A value it can't take fails its start, naming the
variable. A coordinator started with `--local` exports nothing, and reads none of them.

| Variable | Effect |
| --- | --- |
| `OTEL_TRACES_EXPORTER` | `otlp`, `console` or `none`; unset, `otlp` if an OTLP endpoint is set, and `none` otherwise |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | The collector, which turns trace export on |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `http/protobuf`, the default, or `grpc` |
| `OTEL_EXPORTER_OTLP_HEADERS` | Headers sent with every export, such as credentials |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | How each job's trace is sampled; `parentbased_always_on` if unset |
| `OTEL_SDK_DISABLED` | `true` turns every export off |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | The resource the traces are exported as |

It exports metrics of its capacity, its speed, its cache and what degrades quietly the same way, as these say:

| Variable | Effect |
| --- | --- |
| `OTEL_METRICS_EXPORTER` | `otlp`, `prometheus`, `console` or `none`, or several, comma-separated; unset, `otlp` if an OTLP endpoint is set, and `none` otherwise |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | The collector of metrics alone |
| `OTEL_METRIC_EXPORT_INTERVAL`, `OTEL_METRIC_EXPORT_TIMEOUT` | How often, in milliseconds, metrics are exported over OTLP, a minute by default, and how long an export may take |
| `OTEL_EXPORTER_PROMETHEUS_HOST`, `OTEL_EXPORTER_PROMETHEUS_PORT` | Where Prometheus scrapes `/metrics`, `localhost:9464` by default: an address of its own, never the API's |

An OTLP endpoint that isn't a signal's own, `OTEL_EXPORTER_OTLP_ENDPOINT`, turns metrics on too, unless
`OTEL_METRICS_EXPORTER` says otherwise. An address Prometheus can't be served on fails the start, naming it.
A histogram measured from a span of a kept trace carries that span as its exemplar, which a Prometheus scrape shows as
OpenMetrics. No metric is labeled with a target, a workspace, a job or a file.
An export that fails is reported in the coordinator's log, never to a client, and never changes a build.
A client building with `TRACEPARENT` set joins its jobs to that trace,
as [joining a CI trace](../remote.md#joining-a-ci-trace) describes.

See the [Remote execution reference](../remote.md) for architecture details, network setup, and deployment guides.
