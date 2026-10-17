// Package finish delivers executor results to client-selected destinations.
//
// # Invariants
//
// This phase uses results and cached content, never loader state or live project sources.
// Registry push placement is explicit: executor and client settings need not agree.
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
// Push, Save and LoadDocker own image delivery; archiveReference owns archive path adaptation.
// Export, install, swap and setMetadata in export.go own directory replacement and host metadata.
// Consult their contracts before changing filesystem operations,
// especially around symlinks, cancellation and replacement races.
package finish
