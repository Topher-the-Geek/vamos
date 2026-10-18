// Package worker runs the attempts a coordinator assigns, each in a contained subprocess, and reports what they made.
//
// # Invariants
//
// Every process an attempt starts is in the attempt's cgroup, which the subprocess is started in, so that a kill
// reaches a command that daemonized itself; a worker that can't make cgroups refuses to register.
// An attempt never outlives its lease: the worker renews it by heartbeat and kills the attempt itself once the lease
// couldn't be renewed for most of its length, measured on this machine's monotonic clock from when the last
// renewed request was sent, since the coordinator renewed on receiving it, whatever request is still in flight,
// so that the coordinator may treat an expired lease as a stopped attempt; the kill is issued before the lease ends,
// and a process in uninterruptible sleep may take longer to be gone.
// The coordinator starts the first lease before sending the assignment, whose delivery may take any part of it,
// so the subprocess starts only once a renewal has been answered, and the fence counts from that renewal's send.
// A heartbeat that gets no answer, or times out, is a transport failure that only the fence decides; an answer that
// the attempt is stale, or asks for the kill, kills it at once.
// After a kill, after self-fencing, and on starting after a crash, the worker removes the attempt's container,
// which run names for the attempt, and its cgroup.
// The subprocess is told the worker's graph root and run root, so that it builds where the worker cleans up.
// On shutdown the worker waits for the attempts it has killed for one deadline in all, not one each.
// The worker's registry credentials are its own; nothing of the client's reaches it.
// At registration the worker reports the platforms it emulates, read from the enabled qemu handlers in binfmt_misc,
// so that the coordinator sends it emulated work for them and no other.
//
// # Lifecycle
//
// Run registers, then asks for assignments until the context ends; a coordinator that can't be reached for a lease,
// or that has restarted and knows nothing of the registration, makes the worker kill what it runs and register again.
// Each attempt is run.Attempt in a subprocess of this binary, given its assignment on standard input; its output and
// spans come back as lines, which the worker sends to the coordinator in batches the transport takes, reading no more
// while a bounded amount waits, so that a slow coordinator slows the attempt's output, and its last line is the report,
// which the worker sends once the subprocess has ended and its container is gone.
// A killed attempt reports nothing: the coordinator has stopped counting it.
//
// # Changes
//
// cgroup.go owns containment; worker.go registration and the work loop; attempt.go one attempt's subprocess,
// heartbeats, fencing and relay; emulation.go reads the binfmt_misc handlers.
// ChildFromEnv and TestChild let a test binary be the subprocess.
// worker_test.go runs a worker against an in-process coordinator and registry.
package worker
