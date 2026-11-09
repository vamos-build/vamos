---
title: "glob()"
description: "Selecting workspace files as input filesets, and the file selection options shared with task outputs."
---

```python
glob(
    name = "source",
    include = ["src/**/*.ts", "package.json"],
    exclude = ["**/*.test.ts"],
)
```

`glob()` selects workspace files relative to the directory containing its `BUILD` file.
The `name` argument and a nonempty `include` list are required.
Other selection options and defaults are listed under [file selection](#file-selection).
Globs supply input filesets to `add()`; they cannot be requested directly by `vamos build` or `vamos export`.

## File selection

`glob()` and output-producing `task()` targets configure file selection with these arguments:

| Argument | Description | Default |
| --- | --- | --- |
| `include` | Nonempty list of file paths or glob patterns | Required |
| `exclude` | List of paths or patterns to omit | `[]` |
| `mode_mask` | Permission bits retained for files and directories | `0o755` |
| `allow_no_files` | Allow a selection that matches zero files or links | `False` |
| `max_file_count` | Maximum number of matched files and links | `10000` |
| `max_file_size` | Maximum size in bytes for an individual file | `100 * 1024 * 1024` (100 MB) |
| `max_total_size` | Maximum total size in bytes across all selected files | `1024 * 1024 * 1024` (1 GB) |

Limits must be positive integers; increase them explicitly when handling larger datasets.
`mode_mask` accepts octal values between `0` and `0o7777`.
It masks existing permissions rather than overriding them with fixed bits.

### Patterns and paths

Patterns are evaluated relative to the `BUILD` file for globs, or relative to `source` for task outputs.
Use `/` as the path separator.
Patterns cannot be empty or resolve outside the selection root after resolving `.` and `..` segments.
Use `.` to select an entire directory tree.

| Pattern | Matches |
| --- | --- |
| `main.go` | The specific file at the root |
| `*.go` | Go files directly beneath the selection root |
| `src/**/*.go` | Go files at any depth beneath `src/`, including `src/` directly |
| `src` | The `src/` directory and all its contents recursively |
| `.` | The entire selection root recursively |
| `**/node_modules` in `exclude` | All matching directories and their contents |

- `*` matches characters within a single directory component, including hidden names beginning with `.`.
- `?` matches a single character.
- `[a-z]` matches character ranges.
- `**` matches zero or more directories and must occupy a full path segment (use `src/**/*.go`, not `src/**.go`).
- Directory exclusions prevent traversal into matching directories.
- Empty directories are omitted; selected files supply necessary parent directory entries.
- Paths remain relative to the selection root: selecting `src/main.go` preserves the `src/` prefix.

### Reading files and links

vamos traverses only directories that can match an included pattern.
In every include pattern, all path components before the first wildcard must exist and must not be symbolic links,
even when `allow_no_files = True`.
For patterns without wildcards (such as `main.go` or `link`), this requirement applies to the entire path:
specifying a literal symlink in `include` fails the selection.

Symbolic links matched by wildcard patterns or contained inside an included directory are copied as symlinks
and are never followed.
A task's `source` directory can traverse symlinks resolved within the task container.

Globs are evaluated once before execution starts, ensuring input files remain consistent throughout the build.
Globs that are not referenced by requested targets are not read.
Task outputs are collected immediately after the task's steps complete.
