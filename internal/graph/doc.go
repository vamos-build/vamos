// Package graph defines the serialized contract between loading and execution.
//
// # Invariants
//
// Graphs contain data, never Starlark values, filesystem handles or buildah state.
// Node references encode dependency order.
// Consumers must validate decoded data before indexing references;
// source positions are diagnostic data, not evaluator state.
// A pull's Digest is its pin from the lockfile, which the graph carries in place of the lockfile itself:
// set, the executor uses that manifest rather than asking what the tag names.
// Loading owns user-facing validation before a graph reaches execution.
// A node's Target names its configured target for target history alone; no key reads it.
//
// # Changes
//
// Review Version when changing graph meaning, and Encode, Decode and Graph.Validate when changing its representation;
// metadata that no key reads, and that a graph builds the same without, needs no new Version.
// Step additions also require loader lowering, executor behavior and action-key coverage.
// vars.go owns the variable syntax shared by loading and execution, toolcache.go the checks of a tool cache's path,
// owner and group, and workspace.go the rules of a workspace's name, which an executor checks again before choosing a
// workspace's tool caches by it.
// Cache identity is owned by run, not this package.
package graph
