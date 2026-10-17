// Package cache defines the stores that a build's content and records live in, and implements them locally.
//
// # Invariants
//
// ContentStore holds blobs by digest: graphs, source files, trees and logs.
// RecordStore holds the latest result of each action and what each tag resolved to; latest means the last write
// that completed.
// Directory implements both in a directory on disk, the one beside the executor's container store,
// and Memory implements both for tests; TestContentStore and TestRecordStore hold them to one contract.
// Every digest is validated before it names anything; a get verifies the bytes as they are read,
// and a corrupt blob is reported as ErrCorrupt, apart from ErrMissing and from a store that can't be reached.
// A blob that a read finds corrupt is dropped, so that the next put of its content, as PutFile makes, repairs it.
// Trees and records are decoded under a size bound and a structural check: ReadTree accepts only a tree that
// Checkout can write safely.
// Images remain in container storage and may be removed independently of their action records;
// before reusing a result, the scheduler in run has the runner check that its image, output tree and log still exist.
// This package imports no other vamos package and does not invoke buildah.
//
// # Lifecycle
//
// Directory writers prepare temporary content before publishing individual files.
// This is not a transaction over several records or a reclamation protocol.
// An open Directory holds its lock shared and Prune takes it exclusive, without waiting, so that a prune and
// any other use of the directory, in this process or another, exclude each other; see lock.go.
// Operations on the local filesystem, PutFile and Checkout, are functions over a store, not methods of it,
// as is LogRecorder, whose file the caller owns until Save or Discard.
// Checkout and ReplayLog stop between entries when their context ends.
//
// # Changes
//
// Blob, Tree, ActionResult and TagRecord define stored data, each with a Validate;
// Provenance in a result describes the executor and is no part of a key.
// See ContentStore and RecordStore for the contract, pendingFile for publication and cleanup,
// PutFile for source changes and size limits, and Checkout for path isolation and restoring modes.
// Inspect Directory.Prune, ClearContent and ClearResults together when changing pruning policy.
package cache
