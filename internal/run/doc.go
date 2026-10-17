// Package run executes cached graphs and owns their container images.
//
// # Invariants
//
// Execution is independent of the project filesystem and Starlark.
// Base images enter through resolve actions; target execution uses image IDs, the digests of configs, or scratch.
// Registries are reached as the executor's own environment says, through SystemContext; no client setting reaches it.
// Every image is located through its name in the cache repository, so the container store's own IDs stay within run;
// what a name names is checked to be the image, by its config's digest and layers, before it is used.
// Graph compatibility and action-cache compatibility are separate concerns.
// The scheduler never opens container storage, and the runner never reads the graph:
// an actionRequest carries everything an action needs, and the two share no state but the stores.
//
// # Lifecycle
//
// Jobs run at once; sharing in share.go runs each action once for every job that needs it, and Close cancels
// what still runs and waits for it.
// Local implements api.Executor: Submit refuses a graph it can't run, then starts it as a job in a goroutine,
// whose retained events watchers read from any position and whose terminal event carries its outcome.
// A failure is data, a failure in scheduler.go, naming the node, action and step and carrying the attempt's log;
// the scheduler describes it against its graph for its own errors, and the client against its own.
// A job opens storage lazily and hands the graph to a scheduler, which resolves pulls first,
// then walks targets in dependency order, computing each one's key and looking its result up.
// A result is reused once the runner confirms its artifacts are present; otherwise the runner executes the action:
// it builds a temporary container, captures its log, publishes the artifact and then the result.
// Task containers are discarded; layer images outlive the job in storage.
// A task whose cached files a target finds corrupt runs again, and the target is tried again.
// Delivery follows successful execution.
//
// # Changes
//
// api.Executor is the client boundary; jobs.go holds jobs and their events, and Events is the scheduler's callback,
// which the runner reaches as actionEvents.
// submitPush and submitExport in executor.go make pushes and exports jobs: a push copies from the container store
// to a registry, and an export publishes an image's blobs to the content store for the client to deliver;
// a task's export needs the content store alone. Submit refuses results whose image IDs aren't image IDs.
// scheduler.go owns dependency order, keys, epochs, index assembly and tag pinning; runner is its interface
// to execution, and scheduler_test.go drives it with a fake.
// localRunner in runner.go executes actions with buildah: result checks, target cleanup and file transfer;
// action.go owns the keyed payload and its version.
// pull.go resolves tags and supplies platform contexts.
// step.go applies action steps.
// Copies go through transfer.Image, which traces them; inspect export, pushImage and layoutIndex
// before changing transports or signature policy.
package run
