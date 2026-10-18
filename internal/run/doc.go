// Package run executes actions with buildah and owns their container images: it is the local executor,
// and the runner that a worker's attempt executes with.
//
// # Invariants
//
// Execution is independent of the project filesystem and Starlark.
// Base images enter through resolve actions; target execution uses image IDs, the digests of configs, or scratch.
// Registries are reached as the executor's own environment says, through SystemContext; no client setting reaches it.
// Every image is located through its name in the cache repository, so the container store's own IDs stay within run;
// what a name names is checked to be the image, by its config's digest and layers, before it is used.
// A result names its image by ID and lists where its bytes are as cache.Locations, the store's name here,
// a pulled base's registry reference by digest too, at the pull-through cache when one mirrors its registry,
// and a built layer's reference in the cache repository once the runner has pushed it there, which it does before
// reporting, verifying the config digest, so that a result never names an artifact that isn't where it says.
// A runner whose store lacks an image fetches it from a registry location.
// Graph compatibility and action-cache compatibility are separate concerns.
// Scheduling belongs to sched: localRunner implements sched.Runner and sched.Checker, reads no graph and no record,
// and returns a sched.Report, which sched.Sharing records once the artifact is published.
//
// # Lifecycle
//
// Local implements api.Executor: Submit refuses a graph it can't run, then starts it as a job in sched.Jobs,
// whose retained events watchers read from any position and whose terminal event carries its outcome.
// A job opens storage lazily and hands the graph to a sched.Scheduler, one target at a time, which resolves pulls
// first, then walks targets in dependency order, computing each one's key and looking its result up.
// A pull whose tag record holds a complete instance for its platform resolves without the runner.
// A result is reused once the checker confirms its artifacts are in the container store; otherwise the runner
// executes the action: it builds a temporary container, captures its log, publishes the artifact and reports it.
// Task containers are discarded; layer images outlive the job in storage.
// Where emulation needs permission, a target whose commands would run emulated fails before running without it;
// the local executor doesn't require it, pending the decision that sched.Options.OptIn records.
// Delivery follows successful execution.
//
// # Changes
//
// submitPush and submitExport in executor.go make pushes and exports jobs: a push copies from the container store
// to a registry, and an export publishes an image's blobs to the content store for the client to deliver;
// a task's export needs the content store alone. Submit refuses results whose image IDs aren't image IDs.
// localRunner in runner.go executes actions with buildah: result checks, base materialization from a location,
// the push of a built layer to the cache repository, target cleanup and file transfer.
// attempt.go is the worker's subprocess: Attempt reads an AttemptInput, runs the action or push with a localRunner
// whose content store is the coordinator's over HTTP, and prints AttemptLines, the report last; RemoveAttempt and
// LeftoverAttempts clean up the containers named for attempts, vamos-attempt-<id>, after a kill or a crash.
// pull.go resolves tags, completes tag instances, maps a registry to its pull-through cache in mirror, and supplies
// platform contexts.
// step.go applies action steps.
// Copies go through transfer.Image, which traces them; inspect export, pushImage and layoutIndex
// before changing transports or signature policy.
package run
