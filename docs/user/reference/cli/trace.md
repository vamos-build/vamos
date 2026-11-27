---
title: "vamos trace"
description: "Writing a stored trace of a command, a job or a run to a file."
---

```sh
vamos trace <digest> <file.json>
vamos trace <job> <file.json> --coordinator <url>
```

Writes a stored trace to `file.json`, which [Perfetto](https://ui.perfetto.dev) opens:
a command's, as `--trace` would have written it, a job's, or one run's of a target.

A digest is what a command printed, `vamos: trace sha256:<hex>`, or what a cached target's span names as its
`attempt trace`; it reads the trace from the executor's build cache until a [prune](prune.md) removes it.
Without `--coordinator`, that is the local build cache.

A job ID reads that job's trace from the coordinator that ran it, for as long as the coordinator remembers the job:
while its command runs, and afterwards among the last jobs it finished.
The command's printed digest reads the trace for longer.

`vamos trace` fails, writing nothing, for:

- a digest cut short, which isn't a valid digest;
- a job ID without `--coordinator`;
- a digest whose content isn't a trace, such as a log's;
- a trace the executor no longer holds;
- a job the coordinator is still running, or no longer remembers.

The file follows `--trace`'s rules: writing it replaces an existing file at the path.
See [traces](../cache.md#traces).
