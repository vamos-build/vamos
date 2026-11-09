---
title: "task()"
description: "Running build steps and collecting output files or check results."
---

```python
task(
    name = "compile",
    base = "tools",
    steps = [
        add("source", "/workspace"),
        workdir("/workspace"),
        run(["npm", "run", "build"]),
    ],
    source = "/workspace/dist",
    include = ["**/*.js"],
)
```

| Argument | Description | Default |
| --- | --- | --- |
| `name` | Target name | Required |
| `base` | A pull or layer reference, or `"scratch"` | Required |
| `steps` | List of steps executed in order | `[]` |
| `source` | Absolute container directory to collect output from | Omitted: no output fileset |
| `include` | Output patterns relative to `source` | Required when `source` is set |
| File selection options | Exclusions, permissions, and file limits | See [file selection](glob.md#file-selection) |

Declare `source` and `include` together to produce an output fileset.
Omit both to define a task that runs checks without keeping files.
File selection options require output to be declared.

Output files preserve their paths relative to `source`.
For example, `source = "/workspace/dist"` and `include = ["js/main.js"]` produces the fileset entry `js/main.js`.
Only selected files survive the task's temporary container; all other container changes are discarded.

A task inherits its base image's working directory and environment variables.
Its base can use another platform, such as `dep("tools", platform = EXEC_PLATFORM)` for native cross-compilation;
see [platforms](../configuration.md#platforms).
