package internal

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/afero"
	"go.podman.io/image/v5/docker/reference"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// buildFile is the sequence of steps declared by a BUILD file. Its last layer makes the image.
type buildFile struct {
	pulls []*pullSpec // in the order declared
	steps []*stepSpec // layers and tasks, in the order declared; each uses only earlier steps
}

// final returns the index in steps of the last layer, which makes the image.
func (f *buildFile) final() int {
	for i := len(f.steps) - 1; ; i-- {
		if f.steps[i].output == nil {
			return i
		}
	}
}

// scratch is the base name for an empty image, as in a Dockerfile's "FROM scratch".
const scratch = "scratch"

// pullSpec is an image to fetch from a registry, declared by a call to pull().
type pullSpec struct {
	name  string
	image reference.NamedTagged
}

// fileSet is a set of files that steps can name in their sources: files in the project,
// declared by glob(), or files a task makes.
type fileSet struct {
	name  string
	task  *stepSpec // the task that makes the files, or nil for a glob
	paths []string  // cleaned and relative, to the BUILD file or the task's source; directories include their contents
}

// stepSpec runs on another step's image, declared by a call to layer() or task(). A layer's
// image becomes the base of later steps; a task's image is discarded once its output is collected.
type stepSpec struct {
	name        string
	base        string     // name of an earlier pull or layer, or scratch
	sources     []*fileSet // placed in destination at their relative paths
	destination string
	run         []string // a layer's run step, or a task's command
	command     []string // a layer's default command
	source      string   // a task's directory of output
	output      *fileSet // a task's output, found in source; nil for a layer
}

// loadBuildFile executes dir/BUILD from fsys, which must call layer() at least once.
// Every step but the last layer must be used by a later step, as its base or in its sources,
// and the files named by glob() must exist within dir.
func loadBuildFile(fsys afero.Fs, dir string) (*buildFile, error) {
	info, err := fsys.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("directory %s does not exist", dir)
	} else if err != nil {
		return nil, err
	} else if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}

	buildPath := filepath.Join(dir, "BUILD")
	src, err := afero.ReadFile(fsys, buildPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no BUILD file in %s", dir)
	} else if err != nil {
		return nil, err
	}

	file := &buildFile{}
	fileSets := map[string]*fileSet{}    // globs and tasks, by name
	declared := map[string]declaration{} // each step's name
	used := map[string]bool{}            // names of steps that are a base or a source
	var images, files []string           // names of pulls and layers, and of globs and tasks, in order

	// declare records name for the step being declared by the builtin running in thread.
	declare := func(thread *starlark.Thread, name string) error {
		if name == "" {
			return errors.New("name must not be empty")
		}
		if name == scratch {
			return fmt.Errorf("name %q is reserved for the empty base image", name)
		}
		if d, ok := declared[name]; ok {
			return fmt.Errorf("name %q is already used at %s", name, d.pos)
		}
		fn := thread.CallFrame(0).Name
		declared[name] = declaration{kind: fn, pos: thread.CallFrame(1).Pos}
		if fn == "glob" || fn == "task" {
			files = append(files, name)
		} else {
			images = append(images, name)
		}
		return nil
	}

	glob := starlark.NewBuiltin("glob", func(
		thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
	) (starlark.Value, error) {
		var name string
		var include *starlark.List
		if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
			"name", &name,
			"include", &include,
		); err != nil {
			return nil, err
		}
		paths, err := includeList(fn.Name(), include)
		if err != nil {
			return nil, err
		}
		for _, p := range paths {
			if err := checkInclude(fsys, dir, p); err != nil {
				return nil, fmt.Errorf("%s: %w", fn.Name(), err)
			}
		}
		if err := declare(thread, name); err != nil {
			return nil, fmt.Errorf("%s: %w", fn.Name(), err)
		}
		fileSets[name] = &fileSet{name: name, paths: paths}
		return starlark.None, nil
	})

	pull := starlark.NewBuiltin("pull", func(
		thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
	) (starlark.Value, error) {
		var name, image string
		tag := "latest"
		if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
			"name", &name,
			"image", &image,
			"tag?", &tag,
		); err != nil {
			return nil, err
		}
		ref, err := parsePullImage(image, tag)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", fn.Name(), err)
		}
		if err := declare(thread, name); err != nil {
			return nil, fmt.Errorf("%s: %w", fn.Name(), err)
		}
		file.pulls = append(file.pulls, &pullSpec{name: name, image: ref})
		return starlark.None, nil
	})

	// addStep checks and declares s, which a call to fn names as sourceNames.
	addStep := func(thread *starlark.Thread, fn *starlark.Builtin, s *stepSpec, sourceNames []string) error {
		// Bases and sources must come earlier, so steps can't form a cycle.
		if d, ok := declared[s.base]; ok && fileSets[s.base] != nil {
			return fmt.Errorf("%s: base %q is a %s(), not a pull() or layer()", fn.Name(), s.base, d.kind)
		} else if !ok && s.base != scratch {
			return fmt.Errorf("%s: base %q is not \"scratch\" or an earlier pull() or layer()%s",
				fn.Name(), s.base, describeNames("steps", images))
		}
		for _, src := range sourceNames {
			set, ok := fileSets[src]
			if !ok {
				if d, ok := declared[src]; ok {
					return fmt.Errorf("%s: source %q is a %s(), not a glob() or task()", fn.Name(), src, d.kind)
				}
				return fmt.Errorf("%s: source %q is not an earlier glob() or task()%s",
					fn.Name(), src, describeNames("globs and tasks", files))
			}
			s.sources = append(s.sources, set)
		}
		if err := checkOverlap(s.sources); err != nil {
			return fmt.Errorf("%s: %w", fn.Name(), err)
		}
		if len(s.sources) > 0 && s.destination == "" {
			return fmt.Errorf("%s: destination is required when sources are given", fn.Name())
		}
		if s.destination != "" && !path.IsAbs(s.destination) {
			return fmt.Errorf("%s: destination %q must be an absolute path", fn.Name(), s.destination)
		}
		if err := declare(thread, s.name); err != nil {
			return fmt.Errorf("%s: %w", fn.Name(), err)
		}
		used[s.base] = true
		for _, src := range sourceNames {
			used[src] = true
		}
		file.steps = append(file.steps, s)
		return nil
	}

	layer := starlark.NewBuiltin("layer", func(
		thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
	) (starlark.Value, error) {
		var name, base, destination string
		var sources, run, command *starlark.List
		if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
			"name", &name,
			"base", &base,
			"sources?", &sources,
			"destination?", &destination,
			"run?", &run,
			"command?", &command,
		); err != nil {
			return nil, err
		}

		s := &stepSpec{name: name, base: base, destination: destination}
		sourceNames, err := stringList(fn.Name(), "sources", sources)
		if err != nil {
			return nil, err
		}
		if s.run, err = stringList(fn.Name(), "run", run); err != nil {
			return nil, err
		}
		if s.command, err = stringList(fn.Name(), "command", command); err != nil {
			return nil, err
		}
		if err := addStep(thread, fn, s, sourceNames); err != nil {
			return nil, err
		}
		return starlark.None, nil
	})

	task := starlark.NewBuiltin("task", func(
		thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
	) (starlark.Value, error) {
		var name, base, destination, source string
		var sources, command, include *starlark.List
		if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
			"name", &name,
			"base", &base,
			"source", &source,
			"include", &include,
			"sources?", &sources,
			"destination?", &destination,
			"command?", &command,
		); err != nil {
			return nil, err
		}

		s := &stepSpec{name: name, base: base, destination: destination, source: source}
		sourceNames, err := stringList(fn.Name(), "sources", sources)
		if err != nil {
			return nil, err
		}
		if s.run, err = stringList(fn.Name(), "command", command); err != nil {
			return nil, err
		}
		if !path.IsAbs(source) {
			return nil, fmt.Errorf("%s: source %q must be an absolute path", fn.Name(), source)
		}
		paths, err := includeList(fn.Name(), include)
		if err != nil {
			return nil, err
		}
		for _, p := range paths {
			if err := checkRelative(p, source); err != nil {
				return nil, fmt.Errorf("%s: %w", fn.Name(), err)
			}
		}
		s.output = &fileSet{name: name, task: s, paths: paths}
		if err := addStep(thread, fn, s, sourceNames); err != nil {
			return nil, err
		}
		fileSets[name] = s.output
		return starlark.None, nil
	})

	thread := &starlark.Thread{Name: "BUILD"}
	predeclared := starlark.StringDict{"glob": glob, "pull": pull, "layer": layer, "task": task}
	if _, err := starlark.ExecFileOptions(&syntax.FileOptions{}, thread, buildPath, src, predeclared); err != nil {
		if evalErr, ok := err.(*starlark.EvalError); ok {
			return nil, errors.New(describeEvalError(evalErr))
		}
		return nil, err
	}
	if !slices.ContainsFunc(file.steps, func(s *stepSpec) bool { return s.output == nil }) {
		return nil, fmt.Errorf("%s: layer() was never called", buildPath)
	}

	// A step nothing uses would be silently left out of the image.
	final := file.steps[file.final()].name
	for _, name := range files {
		if !used[name] {
			d := declared[name]
			return nil, fmt.Errorf("%s: %s: %q is not in the sources of any later layer() or task()",
				d.pos, d.kind, name)
		}
	}
	for _, p := range file.pulls {
		if !used[p.name] {
			return nil, fmt.Errorf("%s: pull: %q is not the base of any layer() or task()",
				declared[p.name].pos, p.name)
		}
	}
	for _, s := range file.steps {
		if s.output == nil && !used[s.name] && s.name != final {
			return nil, fmt.Errorf("%s: layer: %q is not the base of any later layer() or task(); "+
				"only the last layer() makes the image", declared[s.name].pos, s.name)
		}
	}
	return file, nil
}

// declaration records where a step's name was declared, and by which builtin.
type declaration struct {
	kind string // "glob", "pull", "layer" or "task"
	pos  syntax.Position
}

// parsePullImage returns the image named by a pull() call's image and tag arguments.
func parsePullImage(image, tag string) (reference.NamedTagged, error) {
	if image == "" {
		return nil, errors.New("image must not be empty")
	}
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return nil, fmt.Errorf("invalid image %q: %w", image, err)
	}
	if tagged, ok := named.(reference.Tagged); ok {
		return nil, fmt.Errorf("image %q must not include a tag; use tag=%q", image, tagged.Tag())
	}
	if _, ok := named.(reference.Digested); ok {
		return nil, fmt.Errorf("image %q must not include a digest", image)
	}
	ref, err := reference.WithTag(named, tag)
	if err != nil {
		return nil, fmt.Errorf("invalid tag %q: %w", tag, err)
	}
	return ref, nil
}

// describeNames lists the steps of a kind, such as "globs", declared so far, for an error
// about an unknown name.
func describeNames(kind string, names []string) string {
	if len(names) == 0 {
		return "; there are none before this"
	}
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = strconv.Quote(name)
	}
	return "; earlier " + kind + " are " + strings.Join(quoted, ", ")
}

// describeEvalError reports e at the innermost position within a BUILD file, in the
// one-line "file:line:col: message" form of compiler errors.
func describeEvalError(e *starlark.EvalError) string {
	for i := len(e.CallStack) - 1; i >= 0; i-- {
		if pos := e.CallStack[i].Pos; pos.Filename() != "<builtin>" && pos.IsValid() {
			return fmt.Sprintf("%s: %s", pos, e.Msg)
		}
	}
	return e.Msg
}

// includeList converts an include argument into a non-empty list of cleaned paths.
func includeList(fnName string, include *starlark.List) ([]string, error) {
	paths, err := stringList(fnName, "include", include)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("%s: include must not be empty", fnName)
	}
	for i, p := range paths {
		if p == "" {
			return nil, fmt.Errorf("%s: include[%d] is empty; use \".\" for the whole directory", fnName, i)
		}
		if strings.ContainsAny(p, "*?[") {
			return nil, fmt.Errorf("%s: include %q: patterns are not supported yet; list each file", fnName, p)
		}
		paths[i] = filepath.Clean(p)
	}
	return paths, nil
}

// checkRelative reports whether src is a relative path that stays within dir.
func checkRelative(src, dir string) error {
	if filepath.IsAbs(src) {
		return fmt.Errorf("include %q must be a path relative to %s", src, dir)
	}
	if clean := filepath.Clean(src); clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("include %q is outside %s", src, dir)
	}
	return nil
}

// checkInclude reports whether src is a relative path to a file or directory within dir,
// reached without following symbolic links.
func checkInclude(fsys afero.Fs, dir, src string) error {
	if err := checkRelative(src, dir); err != nil {
		return err
	}
	if filepath.Clean(src) == "." {
		// dir itself was already found, and may be reached through a link, as from a linked $PWD.
		return nil
	}
	// Check each component: lstat on the final path still follows parent links.
	current := dir
	for _, component := range strings.Split(filepath.Clean(src), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, _, err := lstat(fsys, current)
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("include %q not found in %s", src, dir)
		} else if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			if current == filepath.Join(dir, src) {
				return fmt.Errorf("include %q is a symbolic link; list the file it links to instead", src)
			}
			return fmt.Errorf("include %q has symbolic link parent %q", src, current)
		}
	}
	return nil
}

// checkOverlap reports whether sources from different places, the project or a task's output,
// include the same path, so that one would silently replace the other's files.
func checkOverlap(sources []*fileSet) error {
	for i, a := range sources {
		for _, b := range sources[i+1:] {
			if a.task == b.task {
				continue // the same files, copied once
			}
			for _, pa := range a.paths {
				for _, pb := range b.paths {
					if shared, ok := sharedPath(pa, pb); ok {
						return fmt.Errorf("sources %q and %q both include %q", a.name, b.name, shared)
					}
				}
			}
		}
	}
	return nil
}

// sharedPath returns whichever of the cleaned relative paths a and b is within the other, if either is.
func sharedPath(a, b string) (string, bool) {
	switch {
	case within(b, a):
		return b, true
	case within(a, b):
		return a, true
	}
	return "", false
}

// within reports whether the cleaned relative path p is dir or inside it.
func within(p, dir string) bool {
	return dir == "." || p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}

// lstat describes the file at path in fsys without following a final symbolic link,
// if fsys supports links.
func lstat(fsys afero.Fs, path string) (fs.FileInfo, bool, error) {
	if l, ok := fsys.(afero.Lstater); ok {
		return l.LstatIfPossible(path)
	}
	info, err := fsys.Stat(path)
	return info, false, err
}

// stringList converts an optional Starlark list into a []string.
func stringList(fnName, param string, list *starlark.List) ([]string, error) {
	if list == nil {
		return nil, nil
	}
	out := make([]string, 0, list.Len())
	for i := 0; i < list.Len(); i++ {
		s, ok := starlark.AsString(list.Index(i))
		if !ok {
			return nil, fmt.Errorf("%s: %s[%d] is %s, want string", fnName, param, i, list.Index(i).Type())
		}
		out = append(out, s)
	}
	return out, nil
}
