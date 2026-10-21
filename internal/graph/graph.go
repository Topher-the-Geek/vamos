package graph

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"slices"
	"strings"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/fileset"
	"github.com/containerd/platforms"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/docker/reference"
)

// Version changes whenever what a graph means changes,
// so that a client and an executor of different versions refuse each other's graphs rather than build something else.
const Version = 3 // 3: a layer's entrypoint

// Graph is everything a build runs:
// each target in each configuration it's needed in, in an order where each node comes after the nodes it uses.
// It holds no Starlark, so that what runs it needs nothing of the BUILD files that made it.
type Graph struct {
	Version int `json:"version"`
	// ExecPlatform is the platform the graph was configured for run() steps to run on without emulation,
	// as EXEC_PLATFORM named it, normalized, so that an executor for another platform can refuse the graph.
	ExecPlatform string `json:"exec_platform"`
	Nodes        []Node `json:"nodes"`
	Roots        []Ref  `json:"roots"` // the targets the command named, once an index is chosen from
}

// Ref names a node by its position in Graph.Nodes.
// A node refers only to nodes before it, so a graph is acyclic by construction.
type Ref int

// TreeFlow names the tree b in a trace, so that the spans that make it and those that use it are joined,
// in whichever process they are.
func TreeFlow(b cache.Blob) string {
	return "tree " + b.Digest.String()
}

// ImageFlow names the image with ID id in a trace, as TreeFlow names a tree.
func ImageFlow(id string) string {
	return "image " + id
}

// Scratch is the empty image, as a base.
const Scratch Ref = -1

// Node is exactly one of Source, Pull, Layer, Task and Index.
type Node struct {
	Label string `json:"label"` // as messages show it: "app" or "//ui:build"
	Desc  string `json:"desc"`  // as messages describe it: `layer "//ui:build" (platform=linux/arm64, mode="debug")`
	Pos   string `json:"pos"`   // the declaration's file:line:col

	Source *Source `json:"source,omitempty"`
	Pull   *Pull   `json:"pull,omitempty"`
	Layer  *Layer  `json:"layer,omitempty"`
	Task   *Task   `json:"task,omitempty"`
	Index  *Index  `json:"index,omitempty"`
}

// Source is a glob's files, already stored in the build cache.
type Source struct {
	Tree cache.Blob `json:"tree"`
}

// Pull is an image that a tag names, for one platform.
type Pull struct {
	Image    string `json:"image"`    // a name with a tag: docker.io/library/node:24-alpine
	Platform string `json:"platform"` // normalized, as platforms.Format gives it
}

// Layer is an image made by applying steps to a base.
type Layer struct {
	Base     Ref      `json:"base"`     // a Pull, a Layer or Scratch
	Platform string   `json:"platform"` // its image's
	Steps    []Step   `json:"steps,omitempty"`
	Command  []string `json:"command,omitempty"`
	// Entrypoint, if set, replaces the base's entrypoint, even with none, and its command if Command is empty,
	// as a Dockerfile's ENTRYPOINT does; nil keeps both.
	Entrypoint *[]string `json:"entrypoint,omitempty"`
}

// Task applies steps to a base, keeps the files that Output selects, if any, and discards its image.
type Task struct {
	Base   Ref     `json:"base"` // a Pull, a Layer or Scratch
	Steps  []Step  `json:"steps,omitempty"`
	Output *Output `json:"output,omitempty"`
}

// Output is the files a task keeps.
type Output struct {
	Source    string             `json:"source"` // an absolute directory in the task's container
	Selection *fileset.Selection `json:"selection"`
}

// Index is an index of images, each for its own platform.
type Index struct {
	Manifests []Ref `json:"manifests"` // Pulls and Layers, in the order the index lists them
}

// Step is one step of a layer or task.
// Op says which, and which other fields it uses.
type Step struct {
	Op      string   `json:"op"`              // "add", "run", "env", "build_env" or "workdir"
	Display string   `json:"display"`         // as the BUILD file wrote it, for messages: `run(["pnpm", "install"])`
	From    *Ref     `json:"from,omitempty"`  // add: a Source, or a Task with Output
	Dest    string   `json:"dest,omitempty"`  // add
	Owner   string   `json:"owner,omitempty"` // add
	Group   string   `json:"group,omitempty"` // add
	Argv    []string `json:"argv,omitempty"`  // run
	Vars    []string `json:"vars,omitempty"`  // env and build_env: NAME=value, in the order given
	Dir     string   `json:"dir,omitempty"`   // workdir
}

// The ops of steps.
const (
	OpAdd      = "add"
	OpRun      = "run"
	OpEnv      = "env"
	OpBuildEnv = "build_env"
	OpWorkdir  = "workdir"
)

// Node returns the node that ref names.
func (g *Graph) Node(ref Ref) *Node {
	return &g.Nodes[ref]
}

// Base returns the base of the layer or task n, or Scratch if n is neither.
func (n *Node) Base() Ref {
	switch {
	case n.Layer != nil:
		return n.Layer.Base
	case n.Task != nil:
		return n.Task.Base
	}
	return Scratch
}

// Steps returns the steps of the layer or task n.
func (n *Node) Steps() []Step {
	switch {
	case n.Layer != nil:
		return n.Layer.Steps
	case n.Task != nil:
		return n.Task.Steps
	}
	return nil
}

// Platform returns the platform of the pull or layer n, or "" if n is neither.
func (n *Node) Platform() string {
	switch {
	case n.Pull != nil:
		return n.Pull.Platform
	case n.Layer != nil:
		return n.Layer.Platform
	}
	return ""
}

// Kind names the kind of n, or "" if it sets none.
func (n *Node) Kind() string {
	switch {
	case n.Source != nil:
		return "source"
	case n.Pull != nil:
		return "pull"
	case n.Layer != nil:
		return "layer"
	case n.Task != nil:
		return "task"
	case n.Index != nil:
		return "index"
	}
	return ""
}

// Encode returns g's canonical encoding: encoding/json writes fields in the order declared, and a graph holds no maps.
func Encode(g *Graph) []byte {
	data, err := json.Marshal(g)
	if err != nil {
		panic(err) // a graph always encodes
	}
	return data
}

// Decode decodes and validates a graph that Encode encoded.
func Decode(data []byte) (*Graph, error) {
	var g Graph
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, fmt.Errorf("invalid build graph: %w", err)
	}
	if err := g.Validate(); err != nil {
		return nil, err
	}
	return &g, nil
}

// Validate checks that g is a graph that can be run, and compiles its tasks' selections for matching.
// Load checks every mistake a BUILD file can make first, with a message saying where,
// so an error here means a graph that vamos didn't make, or a bug.
func (g *Graph) Validate() error {
	if g.Version != Version {
		return fmt.Errorf("invalid build graph: version %d, want %d", g.Version, Version)
	}
	if err := checkPlatform(g.ExecPlatform); err != nil {
		return fmt.Errorf("invalid build graph: execution %w", err)
	}
	for i := range g.Nodes {
		if err := g.validateNode(Ref(i)); err != nil {
			return fmt.Errorf("invalid build graph: node %d (%s): %w", i, g.Nodes[i].Desc, err)
		}
	}
	if len(g.Roots) == 0 {
		return errors.New("invalid build graph: it has no roots")
	}
	used := make([]bool, len(g.Nodes))
	for i, root := range g.Roots {
		if root < 0 || int(root) >= len(g.Nodes) {
			return fmt.Errorf("invalid build graph: root %d is %d, which names no node", i, root)
		}
		if slices.Contains(g.Roots[:i], root) {
			return fmt.Errorf("invalid build graph: node %d is a root twice", root)
		}
		used[root] = true
	}
	// Refs point only backward, so each node's users come after it.
	for i := len(g.Nodes) - 1; i >= 0; i-- {
		if !used[i] {
			return fmt.Errorf("invalid build graph: node %d (%s) is used by no root", i, g.Nodes[i].Desc)
		}
		for _, ref := range g.Nodes[i].Refs() {
			if ref != Scratch {
				used[ref] = true
			}
		}
	}
	return nil
}

// Refs returns the nodes that n uses.
func (n *Node) Refs() []Ref {
	var refs []Ref
	switch {
	case n.Layer != nil:
		refs = append(refs, n.Layer.Base)
	case n.Task != nil:
		refs = append(refs, n.Task.Base)
	case n.Index != nil:
		refs = append(refs, n.Index.Manifests...)
	}
	for _, in := range n.Steps() {
		if in.From != nil {
			refs = append(refs, *in.From)
		}
	}
	return refs
}

// validateNode checks the node ref.
func (g *Graph) validateNode(ref Ref) error {
	n := &g.Nodes[ref]
	kinds := 0
	for _, set := range []bool{n.Source != nil, n.Pull != nil, n.Layer != nil, n.Task != nil, n.Index != nil} {
		if set {
			kinds++
		}
	}
	if kinds != 1 {
		return fmt.Errorf("it sets %d kinds, want 1", kinds)
	}
	if err := noNUL(n.Label, n.Desc, n.Pos); err != nil {
		return err
	}
	switch {
	case n.Source != nil:
		return n.Source.Tree.Digest.Validate()

	case n.Pull != nil:
		if err := checkPlatform(n.Pull.Platform); err != nil {
			return err
		}
		named, err := reference.ParseNamed(n.Pull.Image)
		if err != nil {
			return fmt.Errorf("image %q: %w", n.Pull.Image, err)
		}
		if _, ok := named.(reference.NamedTagged); !ok {
			return fmt.Errorf("image %q has no tag", n.Pull.Image)
		}
		if _, ok := named.(reference.Digested); ok {
			return fmt.Errorf("image %q has a digest", n.Pull.Image)
		}
		return nil

	case n.Layer != nil:
		l := n.Layer
		if err := g.checkRef(ref, "base", l.Base, true, "pull", "layer"); err != nil {
			return err
		}
		if err := checkPlatform(l.Platform); err != nil {
			return err
		}
		if l.Base != Scratch && g.Nodes[l.Base].Platform() != l.Platform {
			return fmt.Errorf("its base is for %s, not %s", g.Nodes[l.Base].Platform(), l.Platform)
		}
		if err := noNUL(l.Command...); err != nil {
			return err
		}
		if l.Entrypoint != nil {
			if err := noNUL(*l.Entrypoint...); err != nil {
				return err
			}
		}
		return g.checkSteps(ref, l.Steps)

	case n.Task != nil:
		t := n.Task
		if err := g.checkRef(ref, "base", t.Base, true, "pull", "layer"); err != nil {
			return err
		}
		if out := t.Output; out != nil {
			if !path.IsAbs(out.Source) {
				return fmt.Errorf("source %q isn't absolute", out.Source)
			}
			if out.Selection == nil {
				return errors.New("its output has no selection")
			}
			patterns := append(slices.Clone(out.Selection.Include), out.Selection.Exclude...)
			if err := noNUL(append(patterns, out.Source)...); err != nil {
				return err
			}
			if err := out.Selection.Compile(out.Source); err != nil {
				return err
			}
		}
		return g.checkSteps(ref, t.Steps)
	}

	if len(n.Index.Manifests) == 0 {
		return errors.New("an index has no manifests")
	}
	var seen []string
	for i, m := range n.Index.Manifests {
		if err := g.checkRef(ref, fmt.Sprintf("manifests[%d]", i), m, false, "pull", "layer"); err != nil {
			return err
		}
		platform := g.Nodes[m].Platform()
		if slices.Contains(seen, platform) {
			return fmt.Errorf("two manifests are for %s", platform)
		}
		seen = append(seen, platform)
	}
	return nil
}

// checkRef checks that ref, the field what of the node from, names a node of one of kinds before from,
// or else Scratch if scratch.
func (g *Graph) checkRef(from Ref, what string, ref Ref, scratch bool, kinds ...string) error {
	if ref == Scratch && scratch {
		return nil
	}
	if ref < 0 || ref >= from {
		return fmt.Errorf("%s is %d, which isn't an earlier node", what, ref)
	}
	if kind := g.Nodes[ref].Kind(); !slices.Contains(kinds, kind) {
		return fmt.Errorf("%s is a %s, want %s", what, kind, strings.Join(kinds, " or "))
	}
	return nil
}

// checkSteps checks the steps of the layer or task ref.
func (g *Graph) checkSteps(ref Ref, steps []Step) error {
	for i, in := range steps {
		if err := g.checkStep(ref, in); err != nil {
			return fmt.Errorf("steps[%d] (%s): %w", i, in.Display, err)
		}
	}
	return nil
}

// checkStep checks the step in of the layer or task ref:
// that it sets only the fields of its op, and that they are valid.
func (g *Graph) checkStep(ref Ref, in Step) error {
	strs := append([]string{in.Display, in.Dest, in.Owner, in.Group, in.Dir}, append(in.Argv, in.Vars...)...)
	if err := noNUL(strs...); err != nil {
		return err
	}
	others := in // the fields its op doesn't use
	others.Op, others.Display = "", ""
	switch in.Op {
	case OpAdd:
		others.From, others.Dest, others.Owner, others.Group = nil, "", "", ""
		if in.From == nil {
			return errors.New("it adds no files")
		}
		if err := g.checkRef(ref, "from", *in.From, false, "source", "task"); err != nil {
			return err
		}
		if t := g.Nodes[*in.From].Task; t != nil && t.Output == nil {
			return errors.New("from is a task without output")
		}
		if err := CheckPath("dest", in.Dest); err != nil {
			return err
		}
		if strings.Contains(in.Owner, ":") || strings.Contains(in.Group, ":") {
			return errors.New(`owner and group must not contain ":"`)
		}
	case OpRun:
		others.Argv = nil
		if len(in.Argv) == 0 {
			return errors.New("argv is empty")
		}
	case OpEnv, OpBuildEnv:
		others.Vars = nil
		if len(in.Vars) == 0 {
			return errors.New("it sets no variables")
		}
		var names []string
		for _, v := range in.Vars {
			name, value, ok := strings.Cut(v, "=")
			if !ok || name == "" {
				return fmt.Errorf("%q isn't NAME=value", v)
			}
			if slices.Contains(names, name) {
				return fmt.Errorf("variable %q is given twice", name)
			}
			names = append(names, name)
			if _, err := ExpandVars(value, func(string) string { return "" }); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	case OpWorkdir:
		others.Dir = ""
		if err := CheckPath("path", in.Dir); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown op %q", in.Op)
	}
	if !reflect.DeepEqual(others, Step{}) {
		return fmt.Errorf("it sets fields that %s doesn't use", in.Op)
	}
	return nil
}

// checkPlatform checks that text is a platform as platforms.Format gives it, normalized.
func checkPlatform(text string) error {
	p, err := ParsePlatform(text)
	if err != nil {
		return fmt.Errorf("platform %q: %w", text, err)
	}
	if platforms.Format(p) != text {
		return fmt.Errorf("platform %q isn't normalized; want %q", text, platforms.Format(p))
	}
	return nil
}

// noNUL reports a NUL byte in any of strs.
func noNUL(strs ...string) error {
	for _, s := range strs {
		if strings.Contains(s, "\x00") {
			return fmt.Errorf("%q holds a NUL byte", s)
		}
	}
	return nil
}

// ParsePlatform parses the platform of a node, as platforms.Format gave it.
func ParsePlatform(text string) (imgspecv1.Platform, error) {
	p, err := platforms.Parse(text)
	if err != nil {
		return imgspecv1.Platform{}, err
	}
	return platforms.Normalize(p), nil
}
