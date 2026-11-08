# Architecture and phase boundaries

The design is explained, with diagrams, across three design documents:
[docs/design/architecture.md](../../docs/design/architecture.md)
for the load/run/finish phases and execution topologies,
[docs/design/execution-protocol.md](../../docs/design/execution-protocol.md)
for the client/executor boundary and jobs,
and [docs/design/package-layering.md](../../docs/design/package-layering.md)
for package dependencies and new steps.
This file says what to check when changing them.

Keep the boundaries:

- No Starlark values, open project files or in-memory loader state may cross from load to run;
  the graph and the content store are the whole handoff.
- A value in [api](../../internal/api/doc.go) must survive JSON and name no host resource;
  `apitest.Contract` checks every `api.Executor`, and `apitest.JSON` proves values cross as JSON.
- Only [transfer](../../internal/transfer/doc.go) is shared by the client and executor sides.
- Dependency constraints are executable in [TestImportRules](../../internal/deps_test.go);
  consult that test before adding imports, rather than inferring rules from directory placement.
- Keep [main.go](../../main.go) limited to re-exec initialization, command execution and final error reporting;
  implementation belongs under `internal/`, and the independent example modules are BUILD clients.

Target identity before execution includes configuration.
See `confLabel` in [config.go](../../internal/load/config.go) for the loader's identity model
and [graph.doc.go](../../internal/graph/doc.go) for the serialized execution model.

For a new build step, change Starlark construction/lowering in [load/step.go](../../internal/load/step.go),
the encoded form and validation in [graph](../../internal/graph/doc.go),
the keyed form in [sched](../../internal/sched/doc.go), and execution in [run](../../internal/run/doc.go).
Raise `graph.Version` when graph meaning changes and `sched.ActionVersion` when what a key covers,
or how a target is built, changes; they are separate compatibility boundaries.

When a change alters a design decision, update the page in [docs/design](../../docs/design/README.md) that explains
it, its diagrams included, in the same change.
