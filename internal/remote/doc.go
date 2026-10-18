// Package remote carries api.Executor and cache.ContentStore over HTTP: Server serves them, and Client is the stub
// through which a client uses a coordinator as if it were in its process.
//
// # Invariants
//
// The package imports neither buildah, the container store nor Starlark: a client of a coordinator needs none of
// them, and TestImportRules holds it to that.
// Values cross as JSON, as api defines them, and blobs as raw request and response bodies, verified on read by
// both sides: a put verifies what it receives, and a get verifies what arrives, so a corrupt store or a corrupt
// transfer is told apart from a missing blob on either side.
// The stores' batched operations are single round trips: Missing, and ReadMany up to maxReadMany blobs.
// ClearContent isn't served: clearing a coordinator's store is a prune's, which excludes the builds that use it.
// Errors cross as data: the sentinels of api and cache travel as codes, so that errors.Is holds on the client,
// and any other error by its message alone.
// Every handler goes through Server.Middleware, the one seam where authentication will go; there is none yet,
// so a coordinator is for trusted networks alone.
//
// # Lifecycle
//
// A client asks capabilities first: they say which instance of the coordinator answered, how long it keeps a build
// for a client that has gone, and the graph versions it runs.
// Submit carries a submission ID the client chooses, so that a retry of a lost acknowledgement returns the job
// already accepted rather than starting another, for as long as the client may retry; a retry also names the
// instance it was first sent to, which another instance refuses, since it can't know whether the job started.
// Watch is a stream of events, one JSON line each, which the client resumes from the last position delivered when
// the connection drops, warning that it did, until the grace period has passed since the drop;
// the drop is over once a line arrives again, so that the next has the whole grace period. A coordinator that
// answers with another instance ID, or that doesn't know the job, has restarted, which ends the watch with a
// message that says so, and one that answers that the job expired, having held it unwatched for the grace period,
// says that instead.
// Cancel, Release, Result and Prune retry transport failures the same way; what they ask is idempotent.
// A blob being read resumes from where it was cut, and its drop is over once content arrives again.
//
// # Changes
//
// wire.go owns the paths and the error codes, which Server and Client must agree on; a code is added there and
// mapped in both.
// Server in server.go serves one executor and one content store; Client in client.go implements both interfaces.
// Prove a change with the contract suites: cachetest.Content through a Server, and apitest.Contract through
// Client and Server over a local executor, in run's tests.
package remote
