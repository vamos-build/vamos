---
title: "Shared flags"
description: "Flags accepted by every command that builds targets, including secrets, traces, and TLS."
---

Commands that build targets accept these shared flags:

| Flag | Description | Default |
| --- | --- | --- |
| `-C`, `--dir <dir>` | Base directory for unqualified target names and workspace root discovery | `.` |
| `--set <name=value>` | Override target parameter; repeat for multiple parameters | Parameter defaults |
| `--secret <id=ID,env=VAR>`, `--secret <id=ID,src=FILE>` | Supply credentials to `run()` steps declaring secret `ID` | None |
| `--platform <os/arch[/var]>` | Target platform for pulls, layers, or tasks | Host platform |
| `--exec-platform <os/arch[/var]>` | Execution platform for build steps and `EXEC_PLATFORM` | See [execution platform selection](../configuration.md#execution-platform-selection) |
| `--coordinator <URL>` | Delegate execution to a remote coordinator | `$VAMOS_COORDINATOR` |
| `--tls-verify <bool>` | Require HTTPS and verify certificates for registry connections | `registries.conf` |
| `--timestamp <time>` | Minimum timestamp for newly executed targets | Derived from inputs |
| `--pins <policy>` | Policy for unpinned pulls: `record`, `resolve`, or `require` | `$VAMOS_PINS` or `record` |
| `--color <when>` | Terminal status color mode: `auto`, `always`, or `never` | `auto` |

- `--platform` cannot be specified when naming an `index` target.
- With multiple build targets, `--set` applies to all named targets declaring that parameter.
  See [parameters](../configuration.md#parameters).
- `--timestamp` accepts Unix seconds or RFC 3339 timestamps (such as `2026-01-01T00:00:00Z`).
  It does not invalidate existing cached targets.

## Pin policies

`--pins` controls how build commands handle pulls not pinned in `vamos.lock`:

| Policy | Behavior |
| --- | --- |
| `record` | Resolves the tag from the client (like [`vamos update`](update.md)) and writes the pin to `vamos.lock` |
| `resolve` | Resolves the tag during execution and caches the result without modifying `vamos.lock` |
| `require` | Fails before building if any required pull is unpinned |

## Secrets

The `--secret` flag supplies credentials to `run()` steps that declare [secrets](../build/secrets.md):

```sh
vamos build //react:build --secret id=sentry,env=SENTRY_AUTH_TOKEN
vamos build //app:deps --secret id=npmrc,src=$HOME/.npmrc
```

- `env=VAR` reads an environment variable from the host process.
  The variable must be set in your shell environment.
- `src=FILE` reads a file from the host filesystem.

Secrets are read when the build begins.
If a step references a secret ID not provided on the command line,
the build halts with an error before any steps run.
Secret values stay in the client process; they never enter container images, build caches, or traces.
`--secret` cannot be combined with `--coordinator` because remote connections do not support client secret transport.

## Performance traces

Pass `--trace <file.json>` to write a Perfetto-format trace:

```sh
vamos build lint test app --trace build-trace.json
vamos load app app:dev --trace load-trace.json
```

Open the resulting file in [Perfetto](https://ui.perfetto.dev).
See [traces](../cache.md#traces).

## TLS verification

The `--tls-verify` flag controls TLS verification when the client contacts container registries:
- Pushing images from the client via `--push-from=client`.
- Reading images from a coordinator's cache repository or pull-through cache.
- Querying upstream registries during `vamos update` or pin recording.

When omitted, the client follows `/etc/containers/registries.conf`
(and any `insecure = true` settings reported by a coordinator for its cache registries).
Executors always use their own `/etc/containers/registries.conf`.
