package run

import (
	"encoding/json"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/fileset"

	"github.com/opencontainers/go-digest"
)

// actionVersion changes whenever what a key covers, or how a target is built, changes,
// so that results built the old way aren't reused.
const actionVersion = 6

// action is everything a target's result depends on.
// Its digest is the target's key in the action cache.
// Target names are left out, so renaming a target keeps its results.
// The requested timestamp is deliberately absent: reused results retain their epoch.
// Added tasks' epochs are inputs through actionStep.Epoch; the base image ID already fixes its epoch.
type action struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"` // "layer" or "task"
	Base    string `json:"base"` // image ID of the base, or "scratch"
	// Platform distinguishes layer images, including those on scratch, whose base has no platform.
	// Tasks omit it: their results depend on the base and steps, which already contain configured values.
	Platform string             `json:"platform,omitempty"`
	Steps    []actionStep       `json:"steps,omitempty"`   // in the order applied
	Command  []string           `json:"command,omitempty"` // a layer's default command
	Source   string             `json:"source,omitempty"`  // a task's directory of output
	Output   *fileset.Selection `json:"output,omitempty"`  // a task's output, within source
}

// actionStep is one step of an action.
// Op says which step, and which other fields it uses.
type actionStep struct {
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

// key returns a's key in the action cache.
func (a *action) key() digest.Digest {
	data, err := json.Marshal(a)
	if err != nil {
		panic(err) // an action always encodes
	}
	return digest.FromBytes(data)
}
