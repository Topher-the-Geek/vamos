// Package cache stores build plans, files and reusable execution results.
//
// # Invariants
//
// The content store and mutable action/tag records have different identities and lifetimes.
// Images remain in container storage and may be removed independently of their action records.
// Before reusing a result, run.targetRunner.checkResult checks
// that its image, output tree and log still exist; otherwise it rebuilds.
// This package imports no other vamos package and does not invoke buildah.
//
// # Lifecycle
//
// Writers prepare temporary content before publishing individual files.
// This is not a transaction over several records or a reclamation protocol.
// Callers own returned file handles, log finalization and checkout directory cleanup.
//
// # Changes
//
// Blob, Tree and ActionResult define stored data.
// See pendingFile for publication and cleanup, PutFile for source changes and size limits,
// and Checkout for path isolation and restoring modes.
// LogRecorder streams output to the cache.
// Inspect Clear and Tag together when changing pruning policy.
package cache
