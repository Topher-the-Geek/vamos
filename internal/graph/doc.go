// Package graph defines the serialized contract between loading and execution.
//
// # Invariants
//
// Graphs contain data, never Starlark values, filesystem handles or buildah state.
// Node references encode dependency order.
// Consumers must validate decoded data before indexing references;
// source positions are diagnostic data, not evaluator state.
// Loading owns user-facing validation before a graph reaches execution.
//
// # Changes
//
// Review Version when changing graph meaning, and Encode, Decode and Graph.Validate when changing its representation.
// Step additions also require loader lowering, executor behavior and action-key coverage.
// vars.go owns the variable syntax shared by loading and execution.
// Cache identity is owned by run, not this package.
package graph
