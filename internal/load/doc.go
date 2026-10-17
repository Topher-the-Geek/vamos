// Package load turns reachable Starlark targets into an execution graph.
//
// # Invariants
//
// Evaluation, configuration and source capture happen on the client.
// Package loading is demand-driven, including labels discovered while configuring;
// there is no workspace-wide validation pass.
// BUILD mistakes retain source positions here rather than becoming executor graph-validation errors.
//
// # Lifecycle
//
// Declaration, configuration, lowering and source capture are separate stages.
// Each honours the load's context: cancelWith stops a Starlark thread when it ends, and the load reports
// the interruption as the context's error, not the BUILD file's.
// Their state is private to one Load call.
// The returned graph and cached sources are the complete handoff; an executor must never need evaluation state.
//
// # Changes
//
// Start with Load and Request.
// In buildfile.go, declaration and evalContext own deferred evaluation and builtin scope;
// loadError preserves nested failures.
// In config.go, confLabel, workspace.edge, workspace.template and configurer own target identity
// and dependency expansion; lowering constructs graph nodes.
// workspace.readGlobs captures sources after configuration.
// Step values and their lowering live in step.go; changes must also reach graph and run.
package load
