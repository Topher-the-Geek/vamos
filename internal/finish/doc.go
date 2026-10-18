// Package finish delivers results, exported to the content store or located at a registry, to client-selected
// destinations.
//
// # Invariants
//
// This phase reads an Image, never loader state, live project sources, the executor's directory or its container
// store: an image at a registry, where a coordinator's workers leave what they build, is read from there by the
// reference the result's location names, with the client's credentials and settings; an index is assembled from its
// members' locations; and a local executor publishes an image's blobs with an export job, which finish reads through
// cache.ContentStore. A task's files are always in the content store.
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
// Image.open owns where an image is read from; Push, Save and LoadDocker own image delivery through copyTo,
// which opens the image for transfer.Image; FromLocations decides from a result whether the client reads it from a
// registry; archiveReference owns archive path adaptation.
// Export, install, swap and setMetadata in export.go own directory replacement and host metadata.
// Consult their contracts before changing filesystem operations,
// especially around symlinks, cancellation and replacement races.
package finish
