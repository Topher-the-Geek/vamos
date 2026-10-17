package run

import (
	"bytes"
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

// actionRequest is one self-contained action, as the scheduler hands it to a runner:
// the keyed payload, which the action's key digests, and execution metadata, which the key excludes.
// A worker needs nothing else to execute it, and never the graph.
type actionRequest struct {
	action *action       // the keyed payload
	key    digest.Digest // the payload's key, under which a layer's or task's result is published; "" for a resolve

	// epoch is what a layer or task stamps its files with: resolved by the scheduler from its base,
	// the tasks whose files it adds and the requested timestamp.
	epoch time.Time
	// platform is the platform of the image a layer or task is built on, which buildah is told to expect;
	// "" on scratch.
	platform string
	// displays names each step as the BUILD file wrote it, by index, for the trace; messages are the scheduler's.
	displays []string
	// again asks a resolve to resolve its tag afresh, rather than as the record store has it.
	again bool
	// force says the job asked for this run, as --rebuild does, a regeneration or a fresh resolution:
	// it never joins an attempt in flight, and consulted no cache.
	force bool
	// rewrite names the blobs found corrupt in this job, which a task that makes them writes again
	// rather than finding them present.
	rewrite map[digest.Digest]bool
}

// actionEvents receives what one action prints.
type actionEvents interface {
	// Output passes on what the action's run() steps print to stream: cache.StreamStdout or cache.StreamStderr.
	// data is valid only during the call.
	Output(stream byte, data []byte)
}

// outputFunc is an actionEvents that calls itself.
type outputFunc func(stream byte, data []byte)

// Output calls the function.
func (f outputFunc) Output(stream byte, data []byte) {
	f(stream, data)
}

// runner executes actions and checks their results.
// The scheduler and the runner share no state but the stores.
type runner interface {
	// check reports why result, published for the action a, can't be used, if it can't:
	// an artifact it names, such as its image or its files, may be gone or corrupt.
	// A store that can't be reached is an error, which fails the lookup, rather than a problem with the result.
	check(ctx context.Context, a *action, result *cache.ActionResult) (problem, err error)
	// run executes req: it materializes the action's inputs from the stores, runs its steps,
	// publishes its artifact and then, for a layer or task, its result under req.key, and returns the result.
	// A step that fails is reported as a stepError.
	run(ctx context.Context, req actionRequest, ev actionEvents) (*cache.ActionResult, error)
}

// stepError says that the step at index step of an action failed with err.
// hint explains a likely cause, such as a missing emulator, or is "";
// cleanup is a failure to remove the action's container afterwards, or nil;
// log is what the attempt printed, saved to the content store, if anything was.
type stepError struct {
	step    int
	err     error
	hint    string
	cleanup error
	log     *cache.Blob
}

// Error describes the failure by the step's index; the scheduler names the step as the BUILD file wrote it.
func (e *stepError) Error() string {
	msg := fmt.Sprintf("step %d failed: %s", e.step, e.message())
	if e.hint != "" {
		msg += "; " + e.hint
	}
	return msg
}

// message describes the step's failure, and the cleanup failure after it, if there was one.
func (e *stepError) message() string {
	if e.cleanup == nil {
		return e.err.Error()
	}
	return e.err.Error() + "; removing its container: " + e.cleanup.Error()
}

// failure is a job's failure as data, which the client renders against its graph, as an error that the scheduler
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
func (s *scheduler) failed(ref graph.Ref, err error) error {
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

// Unwrap returns the step's error.
func (e *stepError) Unwrap() error {
	return e.err
}

// scheduler walks a graph in dependency order, resolving each node's inputs, computing its key,
// looking its result up, and handing the runner the actions that must run.
// It holds no state beyond the stores and the runner, and never opens container storage.
type scheduler struct {
	graph     *graph.Graph
	content   cache.ContentStore
	records   cache.RecordStore
	runner    runner
	events    Events
	rebuild   map[graph.Ref]bool // the roots to run again, and the pulls to resolve again
	timestamp time.Time          // if set, the earliest time that targets which run stamp their files with

	corrupt map[digest.Digest]bool   // blobs found corrupt in this job, which a task that makes them rewrites
	pinned  map[string]digest.Digest // the index each tag resolved to in this job, so every platform shares it
	images  map[graph.Ref]builtImage // the images of pulls and layers, once they are ready
	trees   map[graph.Ref]setTree    // the files of tasks, once they have run
	indexes map[graph.Ref][]api.Manifest
}

// builtImage is the image of a pull or a layer, once it is in the executor's image store.
type builtImage struct {
	id       string    // its image ID, or scratch
	platform string    // as platforms.Format gives it; "" for scratch
	epoch    time.Time // when it was made; files that targets add to it are stamped with this, or a later epoch
}

// setTree is the files of a task, once they are in the content store.
type setTree struct {
	tree  cache.Blob // the files, as a tree
	epoch time.Time  // as for builtImage
}

// newScheduler returns a scheduler of g, running what it must through r, with rebuild for the roots,
// as api.GraphRequest.Rebuild asks.
func newScheduler(
	g *graph.Graph, content cache.ContentStore, records cache.RecordStore, r runner, ev Events,
	rebuild bool, timestamp time.Time,
) *scheduler {
	again := map[graph.Ref]bool{}
	for _, ref := range g.Roots {
		again[ref] = rebuild
		// An index is built by building its manifests.
		if idx := g.Node(ref).Index; idx != nil {
			for _, m := range idx.Manifests {
				again[m] = rebuild
			}
		}
	}
	return &scheduler{
		graph:     g,
		content:   content,
		records:   records,
		runner:    r,
		events:    ev,
		rebuild:   again,
		timestamp: timestamp,
		corrupt:   map[digest.Digest]bool{},
		pinned:    map[string]digest.Digest{},
		images: map[graph.Ref]builtImage{
			graph.Scratch: {id: scratch, epoch: time.Unix(0, 0)}, // buildah starts an empty image from this name
		},
		trees:   map[graph.Ref]setTree{},
		indexes: map[graph.Ref][]api.Manifest{},
	}
}

// run resolves the graph's pulls first, so a bad image name is reported before any slow target runs,
// then runs its layers, tasks and indexes in order, and returns a result for each root.
func (s *scheduler) run(ctx context.Context) ([]api.Result, error) {
	g := s.graph
	// Every pull of a tag that one pulls again gets the image it names now, even if pulled before that one.
	againTags := map[string]bool{}
	for i, n := range g.Nodes {
		if n.Pull != nil && s.rebuild[graph.Ref(i)] {
			againTags[n.Pull.Image] = true
		}
	}
	for i, n := range g.Nodes {
		if n.Pull != nil {
			if err := s.resolve(ctx, graph.Ref(i), againTags[n.Pull.Image]); err != nil {
				return nil, s.failed(graph.Ref(i), err)
			}
		}
	}

	regenerated := map[graph.Ref]bool{}
	for i := range g.Nodes {
		ref, n := graph.Ref(i), &g.Nodes[i]
		var err error
		switch {
		case n.Layer != nil, n.Task != nil:
			why := ""
			if s.rebuild[ref] {
				why = "rebuilding"
			}
			err = s.regenerating(ctx, regenerated, func() error { return s.runCached(ctx, ref, why) })
		case n.Index != nil:
			err = s.runIndex(ctx, ref)
		}
		if err != nil {
			return nil, err
		}
	}

	var results []api.Result
	for _, ref := range g.Roots {
		res := api.Result{Node: ref}
		switch n := g.Node(ref); {
		case n.Pull != nil, n.Layer != nil:
			res.Image = s.images[ref].id
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

// regenerating runs run, a target, and if it finds the files of a task it adds corrupt in the content store,
// runs that task again, replacing its result and every file of its tree, and then tries the target again.
// A task that another target found corrupt earlier in this job isn't run again a second time:
// regenerated says which have been, so that nothing loops.
// The files of a glob are not regenerable: the job fails, and the client that still has the project
// may capture and submit them again.
// Nor is a failure whose container couldn't be removed: a later success would hide it, so the job fails with it.
func (s *scheduler) regenerating(ctx context.Context, regenerated map[graph.Ref]bool, run func() error) error {
	for {
		err := run()
		ie, ok := s.corruptInput(err)
		if !ok || ie.cleanup != nil || s.graph.Node(ie.from).Task == nil || regenerated[ie.from] {
			return err
		}
		from := ie.from
		regenerated[from] = true
		// Every file of the old tree is written again, since any of them could be corrupt in the same way.
		if t, terr := cache.ReadTree(ctx, s.content, s.trees[from].tree); terr == nil {
			for _, b := range t.Files() {
				s.corrupt[b.Digest] = true
			}
		}
		why := "building again, since its cached files are corrupt: " + err.Error()
		if err := s.regenerating(ctx, regenerated, func() error { return s.runCached(ctx, from, why) }); err != nil {
			return err
		}
	}
}

// corruptInput reports whether err, from running a target, says that the files an add() step adds are corrupt,
// and if so returns the failure, which says which node made them.
func (s *scheduler) corruptInput(err error) (*inputError, bool) {
	var ie *inputError
	if !errors.As(err, &ie) || !errors.Is(err, cache.ErrCorrupt) {
		return nil, false
	}
	return ie, true
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

// resolve resolves the pull ref: the image its tag names for its platform, from the index the tag resolved to
// earlier in this job, if it did, so that every platform's image of one tag comes from one index.
// With again, the tag is resolved afresh rather than as the record store has it.
func (s *scheduler) resolve(ctx context.Context, ref graph.Ref, again bool) (err error) {
	n := s.graph.Node(ref)
	ctx, span := tracing.StartTrack(ctx, n.Desc, n.Desc, "image", n.Pull.Image, "platform", n.Pull.Platform)
	defer func() { span.EndWith(err) }()
	if s.rebuild[ref] {
		// The tag may name a newer image now.
		s.events.Status(ref, "pulling again")
	}
	a := &action{
		Version:  actionVersion,
		Kind:     kindResolve,
		Image:    n.Pull.Image,
		Platform: n.Pull.Platform,
		Index:    s.pinned[n.Pull.Image],
	}
	req := actionRequest{action: a, again: again && a.Index == ""}
	// A fresh resolution never joins one in flight, which may give the tag as the record store has it.
	req.force = s.rebuild[ref] || req.again
	result, err := s.runner.run(ctx, req, s.output(ref))
	if err != nil {
		return err
	}
	if result.Image == "" || result.Platform == "" || result.Index == "" {
		return fmt.Errorf("the resolution of %s reported no image, platform or index", n.Pull.Image)
	}
	s.pinned[n.Pull.Image] = result.Index
	img := builtImage{id: result.Image.Encoded(), platform: result.Platform, epoch: time.Unix(result.Epoch, 0)}
	s.images[ref] = img
	span.Set("image ID", img.id, "epoch", img.epoch.UTC().Format(time.RFC3339))
	span.Produce(graph.ImageFlow(img.id))
	return nil
}

// runCached makes the result of the layer or task ref ready for later targets,
// reusing the result the record store holds for its inputs or else running it.
// It reports which it did, and replays a reused target's output, so a build prints what the target printed.
// If why isn't "", it runs the target even if its result is cached, reporting why, and replaces that result.
func (s *scheduler) runCached(ctx context.Context, ref graph.Ref, why string) (err error) {
	n := s.graph.Node(ref)
	target := n.Desc
	a := s.inputs(n)
	key := a.key()
	ctx, span := tracing.StartTrack(ctx, target, target, "key", key.String(), "base", a.Base)
	if n.Layer != nil {
		span.Set("platform", n.Layer.Platform)
	}
	defer func() { span.EndWith(err) }()
	status := func(status string) {
		span.Set("outcome", status)
		s.events.Status(ref, status)
	}
	if why != "" {
		status(why)
	} else {
		result, log, problem, err := s.lookup(ctx, a, key)
		switch {
		case err != nil:
			return s.failed(ref, fmt.Errorf("reading the build cache: %w", err))
		case result == nil:
			status("building")
		case problem == nil:
			status("cached")
			if err := s.replay(ctx, ref, log); err != nil {
				return s.failed(ref, fmt.Errorf("replaying its output from the build cache: %w", err))
			}
			s.record(ref, result)
			if tracing.Enabled(ctx) {
				span.Set(resultArgs(result)...)
			}
			span.Produce(s.flow(ref)) // as the commit or collection that made it would
			return nil
		default:
			status(fmt.Sprintf("building, since the cached result can't be used: %v", problem))
		}
	}

	base := s.images[n.Base()]
	if n.Layer != nil && n.Base() != graph.Scratch && base.platform != n.Layer.Platform {
		return s.failed(ref, fmt.Errorf("its base %s is for %s, not %s; a layer's base must be for its platform",
			s.graph.Node(n.Base()).Label, base.platform, n.Layer.Platform))
	}
	req := actionRequest{
		action:   a,
		key:      key,
		epoch:    s.epoch(n),
		platform: base.platform,
		displays: displays(n),
		force:    why != "",
		rewrite:  maps.Clone(s.corrupt), // the runner's own, as a wire would give it
	}
	result, err := s.runner.run(ctx, req, s.output(ref))
	if err != nil {
		return s.describe(ref, key, err)
	}
	s.record(ref, result)
	if tracing.Enabled(ctx) {
		span.Set(resultArgs(result)...)
	}
	return nil
}

// describe returns err, from running the action key of the layer or task ref, as a failure naming the node and,
// for a step's failure, the step, with its log and what to do about corrupt sources.
// A failure at a step that adds files is an inputError, so that regenerating can tell whose.
func (s *scheduler) describe(ref graph.Ref, key digest.Digest, err error) error {
	n := s.graph.Node(ref)
	var se *stepError
	if !errors.As(err, &se) || se.step >= len(n.Steps()) {
		f := &failure{Failure: api.Failure{Nodes: []graph.Ref{ref}, Action: key}, graph: s.graph, cause: err}
		var logged *loggedError
		switch {
		case se != nil: // a step the node doesn't have: a bug, reported with what is known
			f.Message, f.Hint, f.Log, f.cause = se.message(), se.hint, se.log, se.err
		case errors.As(err, &logged):
			f.Message, f.Log = err.Error(), logged.log
		default:
			f.Message = err.Error()
		}
		return f
	}
	in := n.Steps()[se.step]
	hint := se.hint
	if in.From != nil && s.graph.Node(*in.From).Source != nil && errors.Is(err, cache.ErrCorrupt) {
		if hint != "" {
			hint += "; "
		}
		hint += "the project must be captured and submitted again"
	}
	step := se.step
	described := &failure{
		Failure: api.Failure{
			Nodes:   []graph.Ref{ref},
			Action:  key,
			Step:    &step,
			Message: se.message(),
			Hint:    hint,
			Log:     se.log,
		},
		graph: s.graph,
		cause: se.err,
	}
	if in.From != nil {
		return &inputError{from: *in.From, err: described, cleanup: se.cleanup}
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

// lookup returns the result that the record store holds for key, the action a's, with its log,
// and why it can't be used, if it can't, or a nil result if the store has none.
func (s *scheduler) lookup(ctx context.Context, a *action, key digest.Digest) (
	_ *cache.ActionResult, log []byte, problem error, err error) {
	_, span := tracing.Start(ctx, "cache lookup", "key", key.String())
	defer func() { span.EndWith(err) }()
	result, err := s.records.Result(ctx, key)
	if err != nil || result == nil {
		span.Set("hit", false)
		return nil, nil, nil, err
	}
	if problem, err = s.runner.check(ctx, a, result); err != nil {
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
			if problem, err = storeProblem("its output can't be used from the cache", err); err != nil {
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

// inputs returns the action of the layer or task n: its inputs, which must be ready.
// The files it adds are in the content store, so that the target is built from the files its key describes.
func (s *scheduler) inputs(n *graph.Node) *action {
	a := &action{
		Version: actionVersion,
		Kind:    kindLayer,
		Base:    s.images[n.Base()].id,
	}
	if n.Task != nil {
		a.Kind = kindTask
		if out := n.Task.Output; out != nil {
			sel := *out.Selection // the runner's own, which it compiles, rather than the graph's
			a.Source, a.Output = out.Source, &sel
		}
	} else {
		a.Platform = n.Layer.Platform
		a.Command = n.Layer.Command
	}
	for _, in := range n.Steps() {
		entry := actionEntry(in)
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
func (s *scheduler) files(ref graph.Ref) cache.Blob {
	if src := s.graph.Node(ref).Source; src != nil {
		return src.Tree
	}
	return s.trees[ref].tree
}

// replay passes on log, what the cached target ref printed, to its events.
func (s *scheduler) replay(ctx context.Context, ref graph.Ref, log []byte) error {
	if log == nil {
		return nil
	}
	out := s.output(ref)
	return cache.ReplayLog(ctx, bytes.NewReader(log), streamWriter{out, cache.StreamStdout},
		streamWriter{out, cache.StreamStderr})
}

// output returns the events that what the node ref prints goes to.
func (s *scheduler) output(ref graph.Ref) actionEvents {
	return outputFunc(func(stream byte, data []byte) { s.events.Output(ref, stream, data) })
}

// streamWriter passes what is written to it to an action's events, as the output of one stream.
type streamWriter struct {
	ev     actionEvents
	stream byte
}

// Write passes data on.
func (w streamWriter) Write(data []byte) (int, error) {
	w.ev.Output(w.stream, data)
	return len(data), nil
}

// record makes the result of the layer or task ref available to later targets.
func (s *scheduler) record(ref graph.Ref, result *cache.ActionResult) {
	n := s.graph.Node(ref)
	epoch := time.Unix(result.Epoch, 0)
	if n.Task != nil {
		if n.Task.Output != nil {
			s.trees[ref] = setTree{tree: *result.Output, epoch: epoch}
		}
	} else {
		s.images[ref] = builtImage{id: result.Image.Encoded(), platform: result.Platform, epoch: epoch}
	}
}

// flow returns the key in a trace of what the layer or task ref made, which spans that use it name too.
func (s *scheduler) flow(ref graph.Ref) string {
	if s.graph.Node(ref).Task != nil {
		return graph.TreeFlow(s.trees[ref].tree)
	}
	return graph.ImageFlow(s.images[ref].id)
}

// epoch returns the time to stamp the files of n's result with when it runs:
// the latest of the epochs of its base and of the tasks whose files it adds, and of s.timestamp.
func (s *scheduler) epoch(n *graph.Node) time.Time {
	epoch := s.images[n.Base()].epoch
	if s.timestamp.After(epoch) {
		epoch = s.timestamp
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

// runIndex makes the result of the index ref: its manifests' images,
// each checked to be for the platform the index asked for, by what its result recorded.
func (s *scheduler) runIndex(ctx context.Context, ref graph.Ref) (err error) {
	n := s.graph.Node(ref)
	_, span := tracing.StartTrack(ctx, n.Desc, n.Desc)
	defer func() { span.EndWith(err) }()
	var manifests []api.Manifest
	for _, m := range n.Index.Manifests {
		img := s.images[m]
		want := s.graph.Node(m).Platform()
		if img.platform != want {
			return s.failed(ref, fmt.Errorf("the image of %s is for %s, not %s", s.graph.Node(m).Label,
				img.platform, want))
		}
		manifests = append(manifests, api.Manifest{Image: img.id, Platform: want})
		span.Set(want, img.id)
	}
	s.indexes[ref] = manifests
	return nil
}
