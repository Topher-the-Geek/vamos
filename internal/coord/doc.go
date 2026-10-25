// Package coord is the coordinator: it schedules builds across a pool of workers, shares actions in flight and their
// results among builds, and keeps the content and record stores; workers execute, and only they do.
//
// # Invariants
//
// The package imports neither buildah, the container store nor Starlark; TestImportRules holds it to that.
// Coordinator implements api.Executor, which remote.Server serves to clients, and remote.Workers, which it serves
// to workers; coordination state lives in memory, but with Config.State its jobs are journaled to disk, and a
// restart resumes them: journal.go owns the records, each written and synced once the jobs table has admitted the
// job, through a sched.Reservation, and before the submission is acknowledged, so that a refused job is never
// recorded; the journal's lock keeps a second coordinator from its state.
// What a restart loses is the workers' registrations and the attempts they run, which their next requests are
// refused, so that they kill what they run and register again; a resumed build runs again from its start, and what
// finished before is a hit. A push or prune cut off by a restart fails, since what it wrote is unknown, and a
// cancelled or released job ends too. A shutdown records nothing, so that what it ends is resumed, not cancelled.
// What a restart decides, a resumed build's count of restarts and the outcome of a job it ends, is recorded before
// the job runs or is readable, or the coordinator fails to start, so that the next restart finds the same; what a
// running coordinator records, a cancellation, a release and an outcome, is reported to Config.Warn if it can't be,
// and the job goes on in memory, since a restart then only runs a build again, or ends it, or a push as unknown.
// Only the coordinator writes records, through sched.Sharing, once a worker has published an artifact and
// reported; a report is accepted from a live attempt alone: one whose lease hasn't expired, that wasn't killed and
// wasn't replaced by a retry, so that a stale report is discarded.
// A build's demand ends an attempt no other build needs: the worker is told to kill it at its next heartbeat,
// and the build's terminal event doesn't wait for the worker's cleanup.
// Work whose worker is lost is tried again elsewhere, a bounded number of times; a push is never retried,
// since its write may have happened.
//
// # Lifecycle
//
// Workers register with their platform, version and capacity, and ask for assignments, which wait until there is
// work for their platform; slots go to the build with the fewest attempts running, then the oldest work.
// A worker renews each attempt's lease by heartbeat, carrying its estimate of its clock's offset, made from the
// coordinator's clock in earlier answers, and sends what it prints and traces as events, which reach the attached
// builds and, shifted by that offset onto the coordinator's clock, the starting build's trace, nested where the
// assignment's trace context says. An attempt told to die at a heartbeat ends then, freeing its worker's slot.
// pool.sweep loses attempts whose leases expired and forgets workers not seen for a lease; a heartbeat, events or
// report that meets an expired attempt first loses it too, so that none renews or ends it between sweeps.
// Submit refuses a graph for a platform the deployment doesn't offer, by configuration or by a worker registered now;
// one for an offered platform with no worker present waits, and says so to the build after a heartbeat interval.
// The bound on open jobs is sched.Jobs's, decided under its lock, as are the grace period, after which an unwatched
// build's demand is withdrawn, and the window, after which a finished build is evicted; the pool's ticker sweeps both.
// The journal keeps the instance, so that a client resumes its watch across a restart that kept the disk, and the
// submission IDs, so that a retried submission that an earlier run accepted gets that run's job.
// A resumed job's events start a restartPositions per restart past its first run's, so that a watcher resuming from
// an earlier run's position meets a gap that says the coordinator restarted; a finished job's terminal event is
// recorded with its position, through sched.Spec.Ending, before any watcher sees it, and restored there, so that a
// client holding the build past it gets nothing, and one before it a gap, then the event.
// Release lets a client end its hold on a build once it has delivered. Prune is refused while any job is open, while
// any attempt is live, a killed one included until its worker reports or its lease would have passed, since the
// answer telling the worker may be lost, and for a lease after the instance started; it clears the content, then the
// records, which advances the cache generation, so that a client that read the new generation uploads to a store
// nothing clears, and the cache repository's tags last. The generation advertised is one more than the records'
// count of prunes, so that a graph's zero, which skips the check, is never a real one. A build of a graph captured
// against another generation fails once it is open, when no prune can start, and Report refuses an attempt that
// started in one, with messages that say to run the command again.
// A cancelled push's outcome says the destination may or may not have been written.
// Emulated work, a request the scheduler marks Emulated, goes only to a worker of the build's platform that reports
// an emulator for the image's platform; a platform neither configured as emulated nor emulated by a registered
// worker fails the target at once, and the pool starts or retries an emulated attempt only while a build that
// permits emulation needs it, which sched.Permitted tells it.
//
// # Changes
//
// pool.go owns workers, attempts, assignment, leases, kills and reports; coordinator.go owns the executor's
// methods, the build's context and recovery; journal.go owns the jobs on disk; checker.go checks cached results
// against the content store and the registry.
// coord_test.go drives a coordinator with fake workers over the worker-facing API; restart_test.go restarts one over
// the same stores and state.
package coord
