// Package api defines what crosses between a client and an executor: requests, events, results and failures,
// and the Executor interface that carries them.
//
// # Invariants
//
// Every value here is plain data that survives JSON encoding, so that a remote transport can carry it:
// no value holds a path on either host, a file descriptor, an open file or a library handle.
// Paths inside a container, a tree's entry paths and source positions in diagnostics are data and may cross.
// The one exception to JSON is GraphRequest.Secrets, which is never encoded, so that no transport carries a secret:
// only an executor in the client's process, handed the request directly, gets them.
// Content is named by digest, as cache.Blob names it, and an image by its ID, with where its bytes are
// as cache.Locations beside it; a registry image by its reference.
// A context, an event stream or a store is a carrier, which a transport supplies, and never appears inside a value.
// Values cross by copy, as JSON would carry them: an executor keeps no part of a request it is handed,
// and every event, outcome and capability it returns is the caller's own, which the Clone methods make.
// The package imports neither buildah nor the container store; TestImportRules holds it to that,
// and TestNoHandles in this package holds the values to plain data.
//
// # Lifecycle
//
// Submit accepts or refuses a job, a graph run, a push or an export, before any work starts.
// A job's events form one sequence from position zero to its terminal event, which carries its Outcome;
// Watch delivers them, and Result returns the outcome once there is one.
// An executor retains a bounded number of a job's events: a watch from an expired position gets a gap event first,
// and a watcher that falls behind is dropped with ErrFellBehind, to watch again from where it was.
// A Failure is data, which the client renders against its graph with Describe; the executor names no target.
// Cancel is the only way a job is cancelled: the context of a call governs that call alone.
// A build is held open while a watch of it is open, and for the executor's grace period after the last watch
// ended; a client that delivers for longer renews the hold by watching from past the terminal event, which delivers
// nothing, and Release ends the hold when the client is done. A build held by no one has its demand withdrawn:
// what runs finishes and is recorded, and the job fails saying it expired. A job released or long finished may be
// evicted, after which its calls answer ErrExpired, for as long as the executor remembers the eviction, rather than
// ErrNoJob, which means the executor never had it.
// Capabilities report the cache generation, which a prune advances; a client reads it before capturing sources and
// sends it with the graph, so that a submit after a prune is refused rather than building on what is gone.
//
// # Changes
//
// Executor is the boundary; run.Local implements it, and apitest.JSON wraps one to prove its values cross as JSON.
// Pruned.Tags and the generation are a coordinator's: the local executor reports neither.
// Add a field here only if it survives JSON and names no host resource; apitest.Contract tests every implementation.
// A field that holds a slice, map or pointer must be copied by its type's Clone method.
package api
