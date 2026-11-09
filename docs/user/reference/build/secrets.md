---
title: "Secrets"
description: "Supplying credentials to run steps without storing them in images or caches."
---

Steps that require credentials (such as publishing tokens or private package credentials) access them through secrets:

```python
run(
    ["sh", "-c", "sentry-cli sourcemaps upload dist"],
    secrets = [secret("sentry", env = "SENTRY_AUTH_TOKEN")],
)
run(
    ["sh", "-c", "npm ci"],
    secrets = [secret("npmrc", file = "/root/.npmrc")],
)
```

`secret(id, env = ...)` sets the specified environment variable for that step alone.
`secret(id, file = ...)` mounts a temporary read-only file owned by root at that path for that step alone.
Each secret must provide either `env` or `file`, not both.
A step cannot define multiple secrets with the same environment variable or destination path.
Secret IDs start with an ASCII letter or digit, followed by letters, digits, `_`, `.`, or `-`.

Secret values are supplied on the command line via `--secret`; see [the CLI reference](../cli/flags.md#secrets).
If a step references a secret that was not provided on the command line,
the build halts with an error as the `BUILD` file loads, before any steps execute.

Secret values never enter container images, build caches, performance traces, or the build graph.
A step's cache key includes the secret ID and target variable or file path, but excludes the secret's value.
Changing a secret's value does not invalidate the cache for that step.
To force a step to rerun when a release changes,
incorporate an explicit cache input such as `build_env(VERSION = ctx.version)`.
Steps must not print or echo secret values into logs, because build logs are retained in the cache.
