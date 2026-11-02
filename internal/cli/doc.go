// Package cli coordinates loading, execution and delivery for vamos commands.
//
// # Invariants
//
// Argument and destination validation precede execution.
// Project reads use the filesystem supplied to NewRootCommand; output artifacts use the host filesystem.
// The CLI owns presentation through the invocation's writers, which Cobra supplies, including the events of jobs.
//
// # Lifecycle
//
// NewRootCommand builds the Cobra tree over an app, what every invocation shares, and each command's RunE
// makes an invocation, the state of one run with its options, writers and context, and calls its handler,
// a method of invocation that a test can call without the root command.
// Each command owns its executor, the local one or a coordinator through remote.Client when --coordinator names
// one, and optional traceSession; coordinator serves a coord.Coordinator through remote.Server, with the
// worker-facing API, or the local executor with --local; worker runs a worker.Worker against a coordinator, and the
// hidden attempt runs one attempt from standard input, as a worker's subprocess.
// UsesContainerStore tells main, before the command runs, whether to re-execute in a user namespace for container
// storage: a coordinator opens none, and neither do help and --version.
// --secret values are read only when a build runs, into api.GraphRequest.Secrets, and are refused with
// --coordinator before it is reached.
// The graph and source trees are stored before the executor is called;
// a build is a job, submitted and watched through api.Executor by watchJob, which also cancels it on Ctrl-C,
// and renders its failure against the graph; a gap in its events is warned about and makes the trace incomplete.
// A result is delivered only after execution succeeds; a trace may still be written when the command fails.
//
// # Changes
//
// NewRootCommand wires commands and parses flags into options; the handlers, build, push, publish, load, save,
// export, prune, graph, coordinator, worker and attempt, hold the phase boundary; Config says which container store the
// local executor builds in, so that tests give it their own, and which Registry pinning asks about tags:
// otherwise upstreamRegistry, which asks each image's own registry from the client, with its credentials and
// --tls-verify, never a mirror, so that scenarios can replace it and need no registry.
// buildImage delivers from a result's registry locations only over a coordinator; the local executor exports from
// its store, so that a local delivery needs no network; clientImage owns that choice.
// applyPins, in pins.go, does what --pins, or VAMOS_PINS, says with the pulls of a configured graph that the lockfile
// doesn't pin, as runLoaded's load.Request.Pin, before any glob is read, so that a glob reads the lockfile as recorded:
// record resolves them as update does and adds their pins, resolve leaves them to the executor, and require fails.
// update, in update.go, pins pulls without an executor: load.Pulls finds them, Registry lists each repository's tags
// once, resolveTag in tags.go chooses the newest tag each covers, and load.UpdateLock rewrites the lockfile with them;
// newerReleases chooses the newer releases and prerelease it reports. --dry-run asks load.WouldUpdateLock instead.
// A repository whose registry refuses to list its tags, as listingRefused tells, has its tags resolve to themselves,
// noted; any other failure to list them fails the update.
// publish loads a publication's graph and plan with load.LoadPublication, through runLoaded, then submits one
// api.PublishRequest, or publishes from the client with finish.Publish; checkTags checks --tag before loading.
// invocation.executor chooses the executor, and invocation.capabilities refuses one of another graph version.
// Output goes through invocation.printf, which returns a failure to write.
// Inspect destination for delivery and platform selection, invocation.execPlatform for the choice of execution
// platform among those the executor offers, eventPrinter for events, and traceSession for trace file ownership.
// Tests in this package exercise the real command path.
package cli
