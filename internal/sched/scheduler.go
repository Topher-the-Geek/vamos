package sched

import (
	"bytes"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/Topher-the-Geek/vamos/internal/api"
	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/tracing"

	"github.com/opencontainers/go-digest"
)

// Request is one self-contained action, as the Scheduler hands it to a Runner:
// the keyed payload, which the action's key digests, and execution metadata, which the key excludes.
// A worker needs nothing else to execute it, and never the graph or the record store; it crosses as JSON.
type Request struct {
	Action *Action       `json:"action"`        // the keyed payload
	Key    digest.Digest `json:"key,omitempty"` // the payload's key, under which a layer's or task's result is kept

	// Epoch is what a layer or task stamps its files with: resolved by the Scheduler from its base,
	// the tasks whose files it adds and the requested timestamp.
	Epoch time.Time `json:"epoch,omitzero"`
	// Platform is the platform of the image a layer or task is built on, which buildah is told to expect;
	// "" on scratch.
	Platform string `json:"platform,omitempty"`
	// Bases is where the base image's bytes are, for a Runner whose store lacks it; the image is Action.Base.
	Bases []cache.Location `json:"bases,omitempty"`
	// Displays names each step as the BUILD file wrote it, by index, for the trace; messages are the Scheduler's.
	Displays []string `json:"displays,omitempty"`
	// Tag is the record of the tag a resolve resolves, as the record store has it, or nil if it has none,
	// so that the Runner reads no record.
	Tag *cache.TagRecord `json:"tag,omitempty"`
	// Again asks a resolve to resolve its tag afresh, rather than as Tag records it.
	Again bool `json:"again,omitempty"`
	// Force says the job asked for this run, as --rebuild does, a regeneration or a fresh resolution:
	// it never joins an attempt in flight, and consulted no cache.
	Force bool `json:"force,omitempty"`
	// Rewrite names the blobs found corrupt in this job, which a task that makes them writes again
	// rather than finding them present.
	Rewrite map[digest.Digest]bool `json:"rewrite,omitempty"`
	// Emulated says the action's run() steps run in an image for another platform than the execution platform,
	// so under emulation: such work goes only to a worker that emulates ImagePlatform.
	Emulated bool `json:"emulated,omitempty"`
	// Permitted says the job permits emulated execution; a job without it may attach to an emulating attempt in
	// flight, but starts none, and fails the target with the mismatch message instead.
	Permitted bool `json:"permitted,omitempty"`
}

// ImagePlatform returns the platform of the image the action's run() steps run in: Platform, or for an action on
// Scratch, whose base has no platform, the platform of the image it makes.
func (r *Request) ImagePlatform() string {
	if r.Action != nil && r.Action.Base == Scratch {
		return r.Action.Platform
	}
	return r.Platform
}

// Report is what a Runner returns from an attempt, for the caller to record once the artifact is published:
// the result, and for a resolve the tag record to write, with its instance for the platform complete.
// It crosses as JSON, from a worker to its coordinator.
type Report struct {
	Result    *cache.ActionResult `json:"result"`
	Tag       string              `json:"tag,omitempty"` // the tag a resolve resolved, whose record is TagRecord
	TagRecord *cache.TagRecord    `json:"tag_record,omitempty"`
}

// Output receives what one action prints.
type Output interface {
	// Output passes on what the action's run() steps print to stream: cache.StreamStdout or cache.StreamStderr.
	// data is valid only during the call.
	Output(stream byte, data []byte)
}

// OutputFunc is an Output that calls itself.
type OutputFunc func(stream byte, data []byte)

// Output calls the function.
func (f OutputFunc) Output(stream byte, data []byte) {
	f(stream, data)
}

// Runner executes actions.
// The Scheduler and the Runner share no state but the content store; the Runner writes no record.
type Runner interface {
	// Run executes req: it materializes the action's inputs from the stores, runs its steps,
	// publishes its artifact, and returns what to record, which the caller writes.
	// A step that fails is reported as a StepError.
	Run(ctx context.Context, req Request, out Output) (*Report, error)
}

// Checker checks that the artifacts a cached result names are present where it names them.
// The local executor checks its container store; a coordinator checks its registry and stores.
type Checker interface {
	// Check reports why result, published for the action a, can't be used, if it can't:
	// an artifact it names, such as its image or its files, may be gone or corrupt.
	// A store that can't be reached is an error, which fails the lookup, rather than a problem with the result.
	Check(ctx context.Context, a *Action, result *cache.ActionResult) (problem, err error)
}

// StepError says that the step at index Step of an action failed with Err.
// Hint explains a likely cause, such as a missing emulator, or is "";
// Cleanup is a failure to remove the action's container afterwards, or nil;
// Log is what the attempt printed, saved to the content store, if anything was.
type StepError struct {
	Step    int
	Err     error
	Hint    string
	Cleanup error
	Log     *cache.Blob
}

// Error describes the failure by the step's index; the Scheduler names the step as the BUILD file wrote it.
func (e *StepError) Error() string {
	msg := fmt.Sprintf("step %d failed: %s", e.Step, e.message())
	if e.Hint != "" {
		msg += "; " + e.Hint
	}
	return msg
}

// message describes the step's failure, and the cleanup failure after it, if there was one.
func (e *StepError) message() string {
	if e.Cleanup == nil {
		return e.Err.Error()
	}
	return e.Err.Error() + "; removing its container: " + e.Cleanup.Error()
}

// Unwrap returns the step's error.
func (e *StepError) Unwrap() error {
	return e.Err
}

// loggedError is a failure of an attempt other than at a step, such as its cancellation
// or a failure to collect its output or commit its image, with what the attempt printed, saved.
type loggedError struct {
	err error
	log *cache.Blob
}

// WithLog returns err with log, the attempt's saved output, or err alone if no log was saved.
func WithLog(err error, log *cache.Blob) error {
	if log == nil {
		return err
	}
	return &loggedError{err: err, log: log}
}

// Error describes the failure.
func (e *loggedError) Error() string {
	return e.err.Error()
}

// Unwrap returns the failure.
func (e *loggedError) Unwrap() error {
	return e.err
}

// Scratch is the name buildah starts an empty image from, as in a Dockerfile's "FROM scratch".
const Scratch = "scratch"

// StoreProblem tells a store's err apart: content that is missing, corrupt or too large makes a result unusable,
// described by what, while any other error means the store couldn't be reached, which fails the operation.
func StoreProblem(what string, err error) (problem, unavailable error) {
	if errors.Is(err, cache.ErrMissing) || errors.Is(err, cache.ErrCorrupt) || errors.Is(err, cache.ErrOverLimit) {
		return fmt.Errorf("%s: %w", what, err), nil
	}
	return nil, err
}

// failure is a job's failure as data, which the client renders against its graph, as an error that the Scheduler
// describes against its own; Unwrap gives the cause, so that errors.Is tells a corrupt input.
type failure struct {
	api.Failure
	graph *graph.Graph
	cause error
}

// Error describes the failure as the client would.
func (f *failure) Error() string {
	return f.Describe(f.graph)
}

// Unwrap returns the cause.
func (f *failure) Unwrap() error {
	return f.cause
}

// failed returns err, from running the node ref, as a failure naming the node, with the attempt's log if it has one.
func (s *Scheduler) failed(ref graph.Ref, err error) error {
	var f *failure
	if errors.As(err, &f) {
		return err
	}
	f = &failure{
		Failure: api.Failure{Nodes: []graph.Ref{ref}, Message: err.Error()},
		graph:   s.graph,
		cause:   err,
	}
	var logged *loggedError
	if errors.As(err, &logged) {
		f.Log = logged.log
	}
	return f
}

// Backend is what a Scheduler reads and runs through: the stores, the check of cached results against where their
// artifacts are, and the Runner, which executes what must run.
type Backend struct {
	Content cache.ContentStore
	Records cache.RecordStore
	Checker Checker
	Runner  Runner
}

// Options is how a Scheduler runs a graph, beyond the graph itself.
type Options struct {
	Rebuild   bool      // run the roots again, and resolve their tags again, as api.GraphRequest.Rebuild asks
	Timestamp time.Time // if set, the earliest time that targets which run stamp their files with
	// Bound is how many targets the Scheduler runs at once; 1, or less, runs them one at a time, in node order.
	Bound int
	// Emulate says the build permits run() steps for another platform than the execution platform, which run
	// under emulation; without it, and with OptIn, such a target fails before anything runs.
	Emulate bool
	// OptIn makes emulation need the permission; without it the executor emulates as its binfmt handlers let it,
	// as local builds always have.
	OptIn bool
}

// Scheduler walks a graph in dependency order, resolving each node's inputs, computing its key,
// looking its result up, and handing the Runner the actions that must run.
// It holds no state beyond the stores and the Runner, and never opens container storage.
// Its maps are written by walk alone, between dispatches, and read by what it dispatches only through the request
// it was given, so that targets running at once share nothing but the stores.
type Scheduler struct {
	graph   *graph.Graph
	content cache.ContentStore
	records cache.RecordStore
	checker Checker
	runner  Runner
	events  Events
	opts    Options
	rebuild map[graph.Ref]bool // the roots to run again, and the pulls to resolve again

	corrupt map[digest.Digest]bool   // blobs found corrupt in this job, which a task that makes them rewrites
	pinned  map[string]digest.Digest // the index each tag resolved to in this job, so every platform shares it
	images  map[graph.Ref]builtImage // the images of pulls and layers, once they are ready
	trees   map[graph.Ref]setTree    // the files of tasks, once they have run
	indexes map[graph.Ref][]api.Manifest

	// handled, if set, is called by walk once it has handled each completion,
	// so that a test can order what it releases after what the walk has seen.
	handled func(completion)
}

// builtImage is the image of a pull or a layer, once it is ready: in the executor's image store, or at a location.
type builtImage struct {
	id        string           // its image ID, or Scratch
	platform  string           // as platforms.Format gives it; "" for Scratch
	epoch     time.Time        // when it was made; files that targets add to it are stamped with this, or a later epoch
	locations []cache.Location // where its bytes are
}

// setTree is the files of a task, once they are in the content store.
type setTree struct {
	tree  cache.Blob // the files, as a tree
	epoch time.Time  // as for builtImage
}

// New returns a Scheduler of g, through b, reporting to ev, run as opts says.
func New(g *graph.Graph, b Backend, ev Events, opts Options) *Scheduler {
	again := map[graph.Ref]bool{}
	for _, ref := range g.Roots {
		again[ref] = opts.Rebuild
		// An index is built by building its manifests.
		if idx := g.Node(ref).Index; idx != nil {
			for _, m := range idx.Manifests {
				again[m] = opts.Rebuild
			}
		}
	}
	return &Scheduler{
		graph:   g,
		content: b.Content,
		records: b.Records,
		checker: b.Checker,
		runner:  b.Runner,
		events:  ev,
		opts:    opts,
		rebuild: again,
		corrupt: map[digest.Digest]bool{},
		pinned:  map[string]digest.Digest{},
		images: map[graph.Ref]builtImage{
			graph.Scratch: {id: Scratch, epoch: time.Unix(0, 0)}, // buildah starts an empty image from this name
		},
		trees:   map[graph.Ref]setTree{},
		indexes: map[graph.Ref][]api.Manifest{},
	}
}

// Run resolves the graph's pulls first, so a bad image name is reported before any slow target runs,
// then runs its layers, tasks and indexes in dependency order, and returns a result for each root.
func (s *Scheduler) Run(ctx context.Context) ([]api.Result, error) {
	if err := s.walk(ctx); err != nil {
		return nil, err
	}
	var results []api.Result
	for _, ref := range s.graph.Roots {
		res := api.Result{Node: ref}
		switch n := s.graph.Node(ref); {
		case n.Pull != nil, n.Layer != nil:
			img := s.images[ref]
			res.Image, res.Locations = img.id, img.locations
		case n.Index != nil:
			res.Index = s.indexes[ref]
		case n.Task != nil && n.Task.Output != nil:
			t := s.trees[ref]
			res.Files, res.Epoch = &t.tree, t.epoch.Unix()
		}
		results = append(results, res)
	}
	return results, nil
}

// completion is what running one node reports back to the walk.
type completion struct {
	ref       graph.Ref
	result    *cache.ActionResult // a pull's, layer's or task's; nil for an index
	manifests []api.Manifest      // an index's
	err       error
}

// walk runs every node of the graph, each once its dependencies are done, as many at once as the bound allows,
// the lowest node first among those ready, so that a bound of one runs them in node order.
// Every pull resolves before any target runs, so that a bad image name fails before a slow target,
// and pulls of one tag resolve in order, so that the later ones are pinned to the index the first resolved.
// Once a node fails, nothing more is dispatched, what runs is waited for, so that it publishes what it made
// and the job produces no event after it ends, and the first failure is returned.
// A target that finds the files of a task it adds corrupt in the content store has that task run again,
// replacing its result and every file of its tree, and is then tried again; a task is regenerated once at most,
// so that nothing loops, and a target that started before the regeneration ended is simply tried again.
// The files of a glob are not regenerable: the job fails, and the client that still has the project
// may capture and submit them again.
// Nor is a failure whose container couldn't be removed: a later success would hide it, so the job fails with it.
func (s *Scheduler) walk(ctx context.Context) error {
	g := s.graph
	// Every pull of a tag that one pulls again gets the image it names now, even if pulled before that one.
	againTags := map[string]bool{}
	var pulls []graph.Ref
	for i, n := range g.Nodes {
		if n.Pull != nil {
			pulls = append(pulls, graph.Ref(i))
			if s.rebuild[graph.Ref(i)] {
				againTags[n.Pull.Image] = true
			}
		}
	}
	// Every pull is a dependency of every other node; rather than an edge each, the pulls left are counted,
	// so that a graph's dependencies grow with its edges, not with its pulls times its targets.
	pullsLeft := len(pulls)
	dependents := make([][]graph.Ref, len(g.Nodes))
	lastOfTag := map[string]graph.Ref{}
	done := make([]bool, len(g.Nodes))
	why := map[graph.Ref]string{} // why a layer or task runs though it may be cached, if it must
	for i, n := range g.Nodes {
		ref := graph.Ref(i)
		switch {
		case n.Source != nil:
			done[i] = true // its files are in the content store already
		case n.Pull != nil:
			if prev, ok := lastOfTag[n.Pull.Image]; ok {
				dependents[prev] = append(dependents[prev], ref)
			}
			lastOfTag[n.Pull.Image] = ref
		default:
			for _, r := range n.Refs() {
				if r != graph.Scratch {
					dependents[r] = append(dependents[r], ref)
				}
			}
			if n.Index == nil && s.rebuild[ref] {
				why[ref] = "rebuilding"
			}
		}
	}
	waiting := make([]int, len(g.Nodes)) // how many of each node's dependencies, but the pulls, aren't done
	for i, ds := range dependents {
		if !done[i] {
			for _, d := range ds {
				waiting[d]++
			}
		}
	}

	bound := max(s.opts.Bound, 1)
	running := make([]bool, len(g.Nodes))
	started := make([]int, len(g.Nodes)) // when each node last started, by seq
	finished := map[graph.Ref]int{}      // when each node last finished, by seq
	regenerated := map[graph.Ref]bool{}  // the tasks run again because their files were found corrupt
	seq := 0
	completions := make(chan completion)
	active := 0
	var failed error
	ready := func(i int) bool {
		return !done[i] && !running[i] && waiting[i] == 0 && (pullsLeft == 0 || g.Nodes[i].Pull != nil)
	}
	// queue holds the nodes that became ready, lowest first; one that is no longer ready when it comes up is
	// skipped, so that a node is queued whenever it may have become ready, without checking whether it is queued.
	queue := &refHeap{}
	enqueue := func(i int) {
		if ready(i) {
			heap.Push(queue, i)
		}
	}
	for i := range g.Nodes {
		enqueue(i)
	}
	// setDone marks the node i done or not, counting it for the nodes that depend on it, and queues what it readies.
	setDone := func(i int, d bool) {
		done[i] = d
		for _, r := range dependents[i] {
			if d {
				waiting[r]--
				enqueue(int(r))
			} else {
				waiting[r]++
			}
		}
		if g.Nodes[i].Pull != nil && d {
			// Pulls are never undone: only tasks are made again.
			if pullsLeft--; pullsLeft == 0 {
				for j := range g.Nodes {
					enqueue(j)
				}
			}
		}
	}
	for {
		for failed == nil && active < bound {
			for queue.Len() > 0 && !ready((*queue)[0]) {
				heap.Pop(queue)
			}
			if queue.Len() == 0 {
				break
			}
			i := heap.Pop(queue).(int)
			ref := graph.Ref(i)
			// A cancelled job starts nothing more; what has run stands, and the node that would have run names it.
			// Nor does one whose demand was withdrawn, for want of a client: what runs finishes and publishes.
			if err := ctx.Err(); err != nil {
				failed = s.failed(ref, err)
				break
			}
			select {
			case <-Withdrawn(ctx):
				failed = s.failed(ref, ErrWithdrawn)
			default:
			}
			if failed != nil {
				break
			}
			seq++
			started[i], running[i] = seq, true
			active++
			start := s.prepare(ref, why[ref], againTags)
			go func() { completions <- start(ctx) }()
		}
		if active == 0 {
			break
		}
		c := <-completions
		active--
		running[c.ref] = false
		seq++
		switch from, ok := s.regenerable(c.err); {
		case c.err == nil:
			finished[c.ref] = seq
			s.record(c)
			setDone(int(c.ref), true)
		case failed != nil:
			// The first failure is the job's.
		case !ok:
			failed = c.err
		case !regenerated[from]:
			regenerated[from] = true
			// Every file of the old tree is written again, since any of them could be corrupt in the same way.
			if t, terr := cache.ReadTree(ctx, s.content, s.trees[from].tree); terr == nil {
				for _, b := range t.Files() {
					s.corrupt[b.Digest] = true
				}
			}
			why[from] = "building again, since its cached files are corrupt: " + c.err.Error()
			setDone(int(from), false) // the target waits for it, and is then tried again
			enqueue(int(from))
		case !done[from] || started[c.ref] < finished[from]:
			// The task is being made again, or was while the target ran: the target is tried again as it is.
			enqueue(int(c.ref))
		default:
			failed = c.err
		}
		if s.handled != nil {
			s.handled(c)
		}
	}
	if failed != nil {
		return failed
	}
	for i, n := range g.Nodes {
		if !done[i] {
			return fmt.Errorf("node %d (%s) was never ready to run", i, n.Desc)
		}
	}
	return nil
}

// refHeap is a min-heap of node indexes, through container/heap.
type refHeap []int

func (h refHeap) Len() int           { return len(h) }
func (h refHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h refHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *refHeap) Push(x any)        { *h = append(*h, x.(int)) }

// Pop removes and returns the last element, as container/heap requires.
func (h *refHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// regenerable reports whether err, from running a target, says that the files an add() step adds are corrupt
// and were made by a task, which can make them again, and if so returns that task.
func (s *Scheduler) regenerable(err error) (graph.Ref, bool) {
	var ie *inputError
	if !errors.As(err, &ie) || !errors.Is(err, cache.ErrCorrupt) || ie.cleanup != nil {
		return 0, false
	}
	if s.graph.Node(ie.from).Task == nil {
		return 0, false
	}
	return ie.from, true
}

// inputError says that running a target failed at the step that adds the files of the node from;
// cleanup is the failure to remove the target's container afterwards, or nil.
type inputError struct {
	from    graph.Ref
	err     error
	cleanup error
}

// Error describes the failure.
func (e *inputError) Error() string {
	return e.err.Error()
}

// Unwrap returns the failure.
func (e *inputError) Unwrap() error {
	return e.err
}

// prepare returns what runs the node ref, with why if it is a layer or task that must run though it may be cached,
// and againTags naming the tags to resolve afresh.
// Everything the node needs of the Scheduler's state is read here, before it starts; the function returned reads
// only the stores, so that it can run beside other nodes.
func (s *Scheduler) prepare(ref graph.Ref, why string, againTags map[string]bool) func(context.Context) completion {
	n := s.graph.Node(ref)
	switch {
	case n.Pull != nil:
		return s.prepareResolve(ref, againTags[n.Pull.Image])
	case n.Index != nil:
		return s.prepareIndex(ref)
	}
	return s.prepareTarget(ref, why)
}

// record keeps what the node c.ref made, for the nodes that use it and for the results.
func (s *Scheduler) record(c completion) {
	n := s.graph.Node(c.ref)
	switch {
	case n.Pull != nil:
		s.pinned[n.Pull.Image] = c.result.Index
		s.images[c.ref] = madeImage(c.result)
	case n.Index != nil:
		s.indexes[c.ref] = c.manifests
	case n.Task != nil:
		if n.Task.Output != nil {
			s.trees[c.ref] = setTree{tree: *c.result.Output, epoch: time.Unix(c.result.Epoch, 0)}
		}
	default:
		s.images[c.ref] = madeImage(c.result)
	}
}

// madeImage returns the image that result, a pull's or layer's, made.
func madeImage(result *cache.ActionResult) builtImage {
	return builtImage{
		id:        result.Image.Encoded(),
		platform:  result.Platform,
		epoch:     time.Unix(result.Epoch, 0),
		locations: result.Locations,
	}
}

// prepareResolve returns what resolves the pull ref: the image its tag names for its platform, from the index the
// tag resolved to earlier in this job, if it did, so that every platform's image of one tag comes from one index.
// With again, the tag is resolved afresh rather than as the record store has it.
func (s *Scheduler) prepareResolve(ref graph.Ref, again bool) func(context.Context) completion {
	n := s.graph.Node(ref)
	a := &Action{
		Version:  ActionVersion,
		Kind:     KindResolve,
		Image:    n.Pull.Image,
		Platform: n.Pull.Platform,
		Index:    s.pinned[n.Pull.Image],
	}
	again = again && a.Index == ""
	// A fresh resolution never joins one in flight, which may give the tag as the record store has it.
	force := s.rebuild[ref] || again
	return func(ctx context.Context) (c completion) {
		c.ref = ref
		ctx, span := tracing.StartTrack(ctx, n.Desc, n.Desc, "image", n.Pull.Image, "platform", n.Pull.Platform)
		defer func() { span.EndWith(c.err) }()
		if s.rebuild[ref] {
			// The tag may name a newer image now.
			s.events.Status(ref, "pulling again")
		}
		result, err := s.resolve(ctx, ref, a, again, force)
		if err != nil {
			c.err = s.failed(ref, err)
			return c
		}
		img := madeImage(result)
		span.Set("image ID", img.id, "epoch", img.epoch.UTC().Format(time.RFC3339))
		span.Produce(graph.ImageFlow(img.id))
		c.result = result
		return c
	}
}

// resolve resolves a, the pull ref's Action: from the tag's record, if it holds a complete instance for the platform
// whose image the Checker finds present, and otherwise through the Runner, which is given the record,
// so that a cached resolve asks the Runner nothing.
// A record of another index than the one pinned is passed on all the same: the Runner resolves the pinned index.
func (s *Scheduler) resolve(ctx context.Context, ref graph.Ref, a *Action, again, force bool) (
	*cache.ActionResult, error) {
	var recorded *cache.TagRecord
	if !again {
		var err error
		if recorded, err = s.records.Tag(ctx, a.Image); err != nil {
			return nil, fmt.Errorf("reading the build cache: %w", err)
		}
		if recorded != nil && (a.Index == "" || recorded.Digest == a.Index) {
			if i, ok := recorded.Instances[a.Platform]; ok && i.Complete() {
				result := &cache.ActionResult{
					Image:     i.Image,
					Platform:  i.Platform,
					Index:     recorded.Digest,
					Epoch:     i.Epoch,
					Locations: i.Locations,
				}
				problem, err := s.checker.Check(ctx, a, result)
				if err != nil {
					return nil, fmt.Errorf("reading the build cache: %w", err)
				}
				if problem == nil {
					tracing.Current(ctx).Set("cached", true)
					return result, nil
				}
				tracing.Current(ctx).Set("unusable", problem.Error())
			}
		}
	}
	req := Request{Action: a, Tag: recorded, Again: again, Force: force}
	rep, err := s.runner.Run(ctx, req, s.output(ref))
	if err != nil {
		return nil, err
	}
	result := rep.Result
	if result == nil || result.Image == "" || result.Platform == "" || result.Index == "" {
		return nil, fmt.Errorf("the resolution of %s reported no image, platform or index", a.Image)
	}
	return result, nil
}

// prepareTarget returns what makes the result of the layer or task ref ready for later targets,
// reusing the result the record store holds for its inputs or else running it.
// If why isn't "", it runs the target even if its result is cached, reporting why, and replaces that result.
func (s *Scheduler) prepareTarget(ref graph.Ref, why string) func(context.Context) completion {
	n := s.graph.Node(ref)
	a := s.inputs(n)
	base := s.images[n.Base()]
	req := Request{
		Action:    a,
		Key:       a.Key(),
		Epoch:     s.epoch(n),
		Platform:  base.platform,
		Bases:     base.locations,
		Displays:  displays(n),
		Force:     why != "",
		Rewrite:   maps.Clone(s.corrupt), // the Runner's own, as a wire would give it
		Permitted: s.opts.Emulate || !s.opts.OptIn,
	}
	req.Emulated = s.emulates(&req) != ""
	return func(ctx context.Context) completion {
		result, err := s.runCached(ctx, ref, req, base, why)
		return completion{ref: ref, result: result, err: err}
	}
}

// runCached makes the result of the layer or task ref, whose request is req and whose base is base,
// reusing the result the record store holds for its inputs or else running it.
// It reports which it did, and replays a reused target's output, so a build prints what the target printed.
// If why isn't "", it runs the target even if its result is cached, reporting why, and replaces that result.
func (s *Scheduler) runCached(
	ctx context.Context, ref graph.Ref, req Request, base builtImage, why string,
) (_ *cache.ActionResult, err error) {
	n := s.graph.Node(ref)
	target := n.Desc
	a, key := req.Action, req.Key
	ctx, span := tracing.StartTrack(ctx, target, target, "key", key.String(), "base", a.Base)
	if n.Layer != nil {
		span.Set("platform", n.Layer.Platform)
	}
	defer func() { span.EndWith(err) }()
	status := func(status string) {
		span.Set("outcome", status)
		s.events.Status(ref, status)
	}
	if why == "" {
		result, log, problem, err := s.lookup(ctx, a, key)
		switch {
		case err != nil:
			return nil, s.failed(ref, fmt.Errorf("reading the build cache: %w", err))
		case result == nil:
			why = "building"
		case problem == nil:
			status("cached")
			if err := s.replay(ctx, ref, log); err != nil {
				return nil, s.failed(ref, fmt.Errorf("replaying its output from the build cache: %w", err))
			}
			if tracing.Enabled(ctx) {
				span.Set(resultArgs(result)...)
			}
			if flow := flowOf(result); flow != "" {
				span.Produce(flow) // as the commit or collection that made it would
			}
			return result, nil
		default:
			why = fmt.Sprintf("building, since the cached result can't be used: %v", problem)
		}
	}

	// What can't run is refused before it is reported as running.
	if n.Layer != nil && n.Base() != graph.Scratch && base.platform != n.Layer.Platform {
		return nil, s.failed(ref, fmt.Errorf("its base %s is for %s, not %s; a layer's base must be for its platform",
			s.graph.Node(n.Base()).Label, base.platform, n.Layer.Platform))
	}
	status(why)
	rep, err := s.runner.Run(ctx, req, s.output(ref))
	if errors.Is(err, ErrEmulationNeeded) {
		// The Runner found no emulating attempt for an unpermitted job to attach to, or the permitted jobs left.
		return nil, s.emulationRefused(ref, s.emulates(&req))
	}
	if err != nil {
		return nil, s.describe(ref, key, err)
	}
	if tracing.Enabled(ctx) {
		span.Set(resultArgs(rep.Result)...)
	}
	return rep.Result, nil
}

// emulates returns the platform that the run() steps of req run under emulation for: an image's platform other than
// the execution platform; or "" if nothing runs emulated.
func (s *Scheduler) emulates(req *Request) string {
	platform := req.ImagePlatform() // a task on Scratch has no platform, and its commands nothing to run
	if platform == "" || platform == s.graph.ExecPlatform {
		return ""
	}
	for _, in := range req.Action.Steps {
		if in.Op == graph.OpRun {
			return platform
		}
	}
	return ""
}

// emulationRefused returns the failure of the layer or task ref, whose run() steps would run under emulation for
// platform, which the build doesn't permit: it names both platforms and both ways out.
func (s *Scheduler) emulationRefused(ref graph.Ref, platform string) error {
	return &failure{
		Failure: api.Failure{
			Nodes: []graph.Ref{ref},
			Message: fmt.Sprintf("its commands would run under emulation: its image is for %s, and this build "+
				"runs commands on %s", platform, s.graph.ExecPlatform),
			Hint: fmt.Sprintf("give --platform %s to build for the platform that runs them, "+
				"or --emulate to allow emulation", s.graph.ExecPlatform),
		},
		graph: s.graph,
		cause: ErrEmulationNeeded,
	}
}

// ErrEmulationNeeded says that an emulated action can't start, because no job that permits emulation needs it:
// a Runner returns it to a job that lacks the permission and finds no attempt in flight to attach to, and a
// coordinator's runner returns it when it would start or retry an attempt whose permitted jobs have all left.
var ErrEmulationNeeded = errors.New("emulation isn't permitted by any build that needs this")

type demandKey struct{}

// withDemand returns ctx carrying demanded, which reports whether a job that still wants the attempt ctx belongs
// to is attached: one that was neither cancelled nor had its demand withdrawn.
func withDemand(ctx context.Context, demanded func() bool) context.Context {
	return context.WithValue(ctx, demandKey{}, demanded)
}

// Demanded reports whether a job still wants the attempt ctx belongs to, so that a coordinator takes back an attempt
// that waits for a worker once every job that needed it has expired; a context no Sharing made is always wanted.
func Demanded(ctx context.Context) bool {
	demanded, _ := ctx.Value(demandKey{}).(func() bool)
	return demanded == nil || demanded()
}

type permissionKey struct{}

// withPermission returns ctx carrying permitted, which reports whether a job that permits emulation still needs the
// attempt ctx belongs to.
func withPermission(ctx context.Context, permitted func() bool) context.Context {
	return context.WithValue(ctx, permissionKey{}, permitted)
}

// Permitted reports whether a job that permits emulation still needs the attempt ctx belongs to, so that a
// coordinator starts or retries an emulated attempt only then; a context no Sharing made permits everything.
func Permitted(ctx context.Context) bool {
	permitted, _ := ctx.Value(permissionKey{}).(func() bool)
	return permitted == nil || permitted()
}

// ErrWithdrawn fails a job whose demand was withdrawn before every target ran: its client stopped watching it for
// the executor's grace period, or released it; what had started finished and is cached.
var ErrWithdrawn = errors.New("the build expired: no client watched it within the grace period, so nothing more was " +
	"started; what had started is cached, so run the command again")

// describe returns err, from running the Action key of the layer or task ref, as a failure naming the node and,
// for a step's failure, the step, with its log and what to do about corrupt sources.
// A failure at a step that adds files is an inputError, so that walk can tell whose.
func (s *Scheduler) describe(ref graph.Ref, key digest.Digest, err error) error {
	n := s.graph.Node(ref)
	var se *StepError
	if !errors.As(err, &se) || se.Step >= len(n.Steps()) {
		f := &failure{Failure: api.Failure{Nodes: []graph.Ref{ref}, Action: key}, graph: s.graph, cause: err}
		var logged *loggedError
		switch {
		case se != nil: // a step the node doesn't have: a bug, reported with what is known
			f.Message, f.Hint, f.Log, f.cause = se.message(), se.Hint, se.Log, se.Err
		case errors.As(err, &logged):
			f.Message, f.Log = err.Error(), logged.log
		default:
			f.Message = err.Error()
		}
		return f
	}
	in := n.Steps()[se.Step]
	hint := se.Hint
	if in.From != nil && s.graph.Node(*in.From).Source != nil && errors.Is(err, cache.ErrCorrupt) {
		if hint != "" {
			hint += "; "
		}
		hint += "the project must be captured and submitted again"
	}
	step := se.Step
	described := &failure{
		Failure: api.Failure{
			Nodes:   []graph.Ref{ref},
			Action:  key,
			Step:    &step,
			Message: se.message(),
			Hint:    hint,
			Log:     se.Log,
		},
		graph: s.graph,
		cause: se.Err,
	}
	if in.From != nil {
		return &inputError{from: *in.From, err: described, cleanup: se.Cleanup}
	}
	return described
}

// displays returns the steps of n as the BUILD file wrote them.
func displays(n *graph.Node) []string {
	var names []string
	for _, in := range n.Steps() {
		names = append(names, in.Display)
	}
	return names
}

// lookup returns the result that the record store holds for key, the Action a's, with its log,
// and why it can't be used, if it can't, or a nil result if the store has none.
func (s *Scheduler) lookup(ctx context.Context, a *Action, key digest.Digest) (
	_ *cache.ActionResult, log []byte, problem error, err error) {
	_, span := tracing.Start(ctx, "cache lookup", "key", key.String())
	defer func() { span.EndWith(err) }()
	result, err := s.records.Result(ctx, key)
	if err != nil || result == nil {
		span.Set("hit", false)
		return nil, nil, nil, err
	}
	if problem, err = s.checker.Check(ctx, a, result); err != nil {
		return nil, nil, nil, err
	}
	if problem == nil && result.Log != nil {
		log, err = s.content.Read(ctx, *result.Log, cache.MaxLogSize)
		if err == nil {
			err = cache.CheckLog(log)
			if err != nil {
				err = cache.Corrupt(*result.Log, "it isn't a whole log: "+err.Error())
			}
		}
		if err != nil {
			if problem, err = StoreProblem("its output can't be used from the cache", err); err != nil {
				return nil, nil, nil, err
			}
		}
	}
	span.Set("hit", problem == nil)
	if problem != nil {
		span.Set("unusable", problem.Error())
	}
	return result, log, problem, nil
}

// resultArgs returns what a trace records of a target's result: its image or its files, and its epoch.
func resultArgs(result *cache.ActionResult) []any {
	args := []any{"epoch", time.Unix(result.Epoch, 0).UTC().Format(time.RFC3339)}
	if result.Image != "" {
		args = append(args, "image ID", result.Image.Encoded())
	}
	if result.Output != nil {
		args = append(args, "files", result.Output.Digest.String())
	}
	return args
}

// flowOf returns the key in a trace of what result, a layer's or task's, made, which spans that use it name too,
// or "" for a task without output, which made nothing to use.
func flowOf(result *cache.ActionResult) string {
	switch {
	case result.Output != nil:
		return graph.TreeFlow(*result.Output)
	case result.Image != "":
		return graph.ImageFlow(result.Image.Encoded())
	}
	return ""
}

// inputs returns the Action of the layer or task n: its inputs, which must be ready.
// The files it adds are in the content store, so that the target is built from the files its key describes.
func (s *Scheduler) inputs(n *graph.Node) *Action {
	a := &Action{
		Version: ActionVersion,
		Kind:    KindLayer,
		Base:    s.images[n.Base()].id,
	}
	if n.Task != nil {
		a.Kind = KindTask
		if out := n.Task.Output; out != nil {
			sel := *out.Selection // the Runner's own, which it compiles, rather than the graph's
			a.Source, a.Output = out.Source, &sel
		}
	} else {
		a.Platform = n.Layer.Platform
		a.Command, a.Entrypoint = n.Layer.Command, n.Layer.Entrypoint
	}
	for _, in := range n.Steps() {
		entry := StepOf(in)
		if in.From != nil {
			b := s.files(*in.From)
			entry.Files = &b
			// The tree has no times, and the task's epoch can differ for the same files, e.g. after --rebuild.
			if e := s.trees[*in.From].epoch; !e.IsZero() {
				entry.Epoch = e.Unix()
			}
		}
		a.Steps = append(a.Steps, entry)
	}
	return a
}

// files returns the tree of the files of ref, a glob or a task that has run, stored in the content store.
func (s *Scheduler) files(ref graph.Ref) cache.Blob {
	if src := s.graph.Node(ref).Source; src != nil {
		return src.Tree
	}
	return s.trees[ref].tree
}

// replay passes on log, what the cached target ref printed, to its events.
func (s *Scheduler) replay(ctx context.Context, ref graph.Ref, log []byte) error {
	if log == nil {
		return nil
	}
	out := s.output(ref)
	return cache.ReplayLog(ctx, bytes.NewReader(log), StreamWriter{out, cache.StreamStdout},
		StreamWriter{out, cache.StreamStderr})
}

// output returns the events that what the node ref prints goes to.
func (s *Scheduler) output(ref graph.Ref) Output {
	return OutputFunc(func(stream byte, data []byte) { s.events.Output(ref, stream, data) })
}

// StreamWriter passes what is written to it to an action's Output, as the output of one stream.
type StreamWriter struct {
	Out    Output
	Stream byte
}

// Write passes data on.
func (w StreamWriter) Write(data []byte) (int, error) {
	w.Out.Output(w.Stream, data)
	return len(data), nil
}

// epoch returns the time to stamp the files of n's result with when it runs:
// the latest of the epochs of its base and of the tasks whose files it adds, and of the requested timestamp.
func (s *Scheduler) epoch(n *graph.Node) time.Time {
	epoch := s.images[n.Base()].epoch
	if s.opts.Timestamp.After(epoch) {
		epoch = s.opts.Timestamp
	}
	for _, in := range n.Steps() {
		if in.From != nil {
			if e := s.trees[*in.From].epoch; e.After(epoch) {
				epoch = e
			}
		}
	}
	return epoch
}

// prepareIndex returns what makes the result of the index ref: its manifests' images,
// each checked to be for the platform the index asked for, by what its result recorded.
func (s *Scheduler) prepareIndex(ref graph.Ref) func(context.Context) completion {
	n := s.graph.Node(ref)
	var manifests []api.Manifest
	var err error
	for _, m := range n.Index.Manifests {
		img := s.images[m]
		want := s.graph.Node(m).Platform()
		if img.platform != want {
			err = s.failed(ref, fmt.Errorf("the image of %s is for %s, not %s", s.graph.Node(m).Label,
				img.platform, want))
			break
		}
		manifests = append(manifests, api.Manifest{Image: img.id, Platform: want, Locations: img.locations})
	}
	return func(ctx context.Context) completion {
		_, span := tracing.StartTrack(ctx, n.Desc, n.Desc)
		for _, m := range manifests {
			span.Set(m.Platform, m.Image)
		}
		span.EndWith(err)
		return completion{ref: ref, manifests: manifests, err: err}
	}
}

// Failed returns the outcome of a job that err ended: its failure, as data if the scheduler made it so,
// or else by its message alone, and cancelled if the job's cancellation is what caused it.
func Failed(err error) api.Outcome {
	var f *failure
	outcome := api.Outcome{Cancelled: errors.Is(err, context.Canceled)}
	if errors.As(err, &f) {
		outcome.Failure = &f.Failure
	} else {
		outcome.Failure = &api.Failure{Message: err.Error()}
	}
	return outcome
}

// Rewriting is a content store that reports the blobs found corrupt in this run as corrupt even when their size
// is right, so that a task which makes them again writes them again instead of finding them present.
type Rewriting struct {
	cache.ContentStore
	Corrupt map[digest.Digest]bool
}

// Has reports b as corrupt if it was found so in this run.
func (r Rewriting) Has(ctx context.Context, b cache.Blob) error {
	if r.Corrupt[b.Digest] {
		return cache.Corrupt(b, "it was found corrupt earlier in this build")
	}
	return r.ContentStore.Has(ctx, b)
}
