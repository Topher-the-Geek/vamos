// Package run executes cached graphs and owns their container images.
//
// # Invariants
//
// Execution is independent of the project filesystem and Starlark.
// Base images enter through puller; target execution uses stored image IDs or scratch.
// Graph compatibility and action-cache compatibility are separate concerns.
//
// # Lifecycle
//
// Local opens storage lazily, resolves pulls, then walks targets in dependency order.
// A target reuses a checked result or builds a temporary container, captures its log and result,
// and publishes the action record.
// Task containers are discarded; layer images outlive the call in storage.
// Delivery follows successful execution.
//
// # Changes
//
// Executor and Events define the client boundary.
// See targetRunner in runner.go for result reuse, target cleanup and file transfer, and action.go for cache keys.
// pull.go resolves tags and supplies platform contexts.
// step.go applies graph steps.
// tracedCopy and blobTracker in copy.go collect progress and wait for it to finish.
// Inspect exportImage and exportIndex before changing transports or signature policy.
package run
