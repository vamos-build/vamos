---
title: "vamos export"
description: "Writing a task's output fileset into a local directory."
---

```sh
vamos export codegen src/generated
vamos export codegen src/generated --replace
```

Extracts files from a task that declares `source` and `include`.
Files are written relative to the task's `source` directory.
For example, if `source = "/out"` and the task generates `/out/dist/bundle.js`,
the file is written to `src/generated/dist/bundle.js`.

The destination folder is replaced as a whole, rather than merged with existing contents.
Exporting into a nonempty directory requires `--replace`.
Always specify a dedicated directory for generated outputs.

When running as root, exported files receive the ownership of the destination folder's parent directory.
Otherwise, files belong to the user executing vamos.
Exported files retain the task's timestamp, permission modes (including setuid/setgid bits within `mode_mask`),
and symbolic links.
