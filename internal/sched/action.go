package sched

import (
	"encoding/json"
	"slices"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/fileset"
	"github.com/Topher-the-Geek/vamos/internal/graph"

	"github.com/opencontainers/go-digest"
)

// ActionVersion changes whenever what a key covers, or how a target is built, changes,
// so that results built the old way aren't reused.
const ActionVersion = 9 // 9: a layer's entrypoint

// The kinds of Action.
const (
	KindLayer   = "layer"
	KindTask    = "task"
	KindResolve = "resolve" // resolve a tag to an image for a platform, pulling it
)

// Action is the keyed payload of an Action: everything a target's result depends on.
// Its digest is the target's key in the record store; the version is kept with it.
// Target names and positions are left out, so renaming a target keeps its results.
// The requested timestamp is deliberately absent: reused results retain their epoch.
// Added tasks' epochs are inputs through Step.Epoch; the base image ID already fixes its epoch.
// A resolve has no key: its result is the tag record it leaves, not an Action result.
type Action struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`           // KindLayer, KindTask or KindResolve
	Base    string `json:"base,omitempty"` // image ID of the base, or "Scratch"
	// Platform distinguishes layer images, including those on Scratch, whose base has no platform,
	// and says which platform a resolve wants an image for.
	// Tasks omit it: their results depend on the base and steps, which already contain configured values.
	Platform string `json:"platform,omitempty"`
	// Image is the tag a resolve resolves, and Index the index it resolved to earlier in the job, if it did,
	// so that every platform's image of one tag comes from one index even if the tag moves meanwhile.
	Image   string        `json:"image,omitempty"`
	Index   digest.Digest `json:"index,omitempty"`
	Steps   []Step        `json:"steps,omitempty"`   // in the order applied
	Command []string      `json:"command,omitempty"` // a layer's default command
	// Entrypoint is a layer's, as graph.Layer.Entrypoint says: nil keeps the base's, and an empty one sets none.
	Entrypoint *[]string          `json:"entrypoint,omitempty"`
	Source     string             `json:"source,omitempty"` // a task's directory of output
	Output     *fileset.Selection `json:"output,omitempty"` // a task's output, within source
}

// Step is one step of an Action.
// Op says which step, and which other fields it uses.
type Step struct {
	Op    string      `json:"op"`              // "add", "run", "env", "build_env" or "workdir"
	Files *cache.Blob `json:"files,omitempty"` // add: tree of the files, as placed beneath dest
	Epoch int64       `json:"epoch,omitempty"` // add: a task's epoch, in Unix seconds; it stamps what adds its files
	Dest  string      `json:"dest,omitempty"`  // add
	Owner string      `json:"owner,omitempty"` // add
	Group string      `json:"group,omitempty"` // add
	Argv  []string    `json:"argv,omitempty"`  // run
	Vars  []string    `json:"vars,omitempty"`  // env and build_env: NAME=value, sorted
	Dir   string      `json:"dir,omitempty"`   // workdir
}

// Key returns a's key in the action cache.
func (a *Action) Key() digest.Digest {
	data, err := json.Marshal(a)
	if err != nil {
		panic(err) // an Action always encodes
	}
	return digest.FromBytes(data)
}

// StepOf returns the step in's part of its target's action, but for the tree of its files.
// Variables, which have different names, are sorted, so that their order doesn't change the key.
func StepOf(in graph.Step) Step {
	switch in.Op {
	case graph.OpAdd:
		return Step{Op: in.Op, Dest: in.Dest, Owner: in.Owner, Group: in.Group}
	case graph.OpRun:
		return Step{Op: in.Op, Argv: in.Argv}
	case graph.OpEnv, graph.OpBuildEnv:
		return Step{Op: in.Op, Vars: slices.Sorted(slices.Values(in.Vars))}
	case graph.OpWorkdir:
		return Step{Op: in.Op, Dir: in.Dir}
	}
	return Step{Op: in.Op}
}
