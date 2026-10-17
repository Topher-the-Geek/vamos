// Package finish delivers exported results to client-selected destinations.
//
// # Invariants
//
// This phase uses api.Exported and the content store, never loader state, live project sources,
// the executor's directory or its container store: the executor publishes an image's blobs with an export job,
// and finish reads them through cache.ContentStore, which may be a store of the client's own.
// Registry push placement is explicit: the executor pushes as a job with its settings,
// and Push here is the client's, with the client's.
// Destination preflight improves errors but does not reserve a path.
//
// # Lifecycle
//
// File delivery separates preparation from installation and subsequent cleanup.
// Installation is the success boundary;
// cleanup of replaced files must not turn a completed export into a reported build failure.
// There is no transaction that rolls back executor cache writes when delivery fails.
//
// # Changes
//
// Push, Save and LoadDocker own image delivery through copyTo, which lays the blobs out for transfer.Image;
// archiveReference owns archive path adaptation.
// Export, install, swap and setMetadata in export.go own directory replacement and host metadata.
// Consult their contracts before changing filesystem operations,
// especially around symlinks, cancellation and replacement races.
package finish
