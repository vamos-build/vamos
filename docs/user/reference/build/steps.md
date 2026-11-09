---
title: "Steps"
description: "The add, run, env, build_env, and workdir steps of tasks and layers."
---

Steps are declared in the `steps` list of a task or layer.
Declaring a step outside a `steps` list is an error.
Steps execute sequentially; later steps observe changes made by earlier steps.

## `add(source, dest, owner = "", group = "")`

Copies a fileset from a glob or task into an absolute container path.
`source` is a target reference; `dest` is a container directory path that can contain environment variables.
Paths within the fileset are preserved:

```python
# If "source" contains src/main.ts, this writes /app/src/main.ts.
add("source", "/app")
add("compile", "/app", owner = "node", group = "staff")
```

Later additions replace earlier files at matching paths.
Files are owned by root unless `owner` or `group` overrides ownership:
- `owner` and `group` must be strings, including numeric IDs (such as `owner = "1000"`).
- They must not contain colons (do not write `owner = "user:group"`).
- Specifying only `owner` sets the group to that user's primary group.
- Specifying only `group` leaves the owner as root.
- Newly created parent directories receive the specified ownership.
- Existing directories retain their modes and ownership unless explicitly matched by the fileset.
- Use `workdir()` before `add()` when a destination directory should maintain root ownership.

## `run(argv, secrets = [])`

Executes a command argument list in the container's current working directory.
`secrets` lists the [secrets](secrets.md) available to the command.
The command must exit with status 0.
Commands do not run inside a shell:

```python
run(["npm", "ci"])
run(["sh", "-c", "mkdir -p /out && printf 'hello\\n' > /out/message.txt"])
```

Invoke a shell explicitly to use pipes, file redirection, or shell variable expansion.
The base image must provide all executables referenced by `argv`, including any shell.

Commands run with chroot isolation and inherit the network configuration of the host process running vamos.
They use the host's network interfaces, IP addresses, and DNS configuration from `/etc/resolv.conf`.
Inside a devcontainer, commands can reach Docker Compose services by service name.
On remote workers, commands use the worker's network environment.
Steps receive a dedicated hostname and an `/etc/hosts` file resolving `host.containers.internal`.
Neither the temporary hostname nor `/etc/hosts` is preserved in the resulting image.

Steps do not inherit host mount configurations from `mounts.conf`.
Network access is not part of a target's cache key:
steps that fetch remote network assets are cached like any other step once completed.

## `env(**variables)` and `build_env(**variables)`

```python
env(NODE_ENV = "production", PATH = "/app/bin:$PATH")
build_env(npm_config_cache = "/tmp/npm")
env(**{"NAME-WITH-DASHES": "value"})
```

Both steps set string environment variables for subsequent steps within the target.
- `env()` also records the variables in the container image configuration,
  making them available to downstream targets and the final runtime container.
- `build_env()` applies only to remaining steps in the current target and does not persist in the image.

Variable names must be nonempty strings without `=`.
Values expand `$NAME` and `${NAME}` using the environment prior to that step,
where `NAME` starts with an ASCII letter or underscore followed by ASCII letters, digits, or underscores.
Unset variables expand to empty strings; `$$` produces a literal `$`.
Other variable syntax—including `${NAME:-default}`, numeric arguments like `$1`, or trailing `$` characters—is
an error during loading.
For shell-style parameter expansion, run a shell command through `run(["sh", "-c", ...])`.

## `workdir(path)`

Creates an absolute directory and sets it as the working directory for subsequent steps and the container image.
The directory is created with ownership matching the image's current user.
Paths expand environment variables, for example `workdir("$HOME/app")`.
Paths in `workdir()` and destinations in `add()` must be absolute after variable expansion.
