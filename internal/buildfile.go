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

// buildFile is the sequence of targets declared by a BUILD file. Its last layer makes the image.
type buildFile struct {
	path    string            // of the BUILD file, for messages
	names   []string          // of every target, globs included, in the order declared
	kinds   map[string]string // each target's kind, by name: "glob", "pull", "layer" or "task"
	pulls   []*pullSpec       // in the order declared
	targets []*targetSpec     // layers and tasks, in the order declared; each uses only earlier targets
}

// final returns the index in targets of the last layer, which makes the image.
func (f *buildFile) final() int {
	for i := len(f.targets) - 1; ; i-- {
		if f.targets[i].output == nil {
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

// fileSet is a set of files that add() can name in its source: files in the project,
// declared by glob(), or files a task makes.
type fileSet struct {
	name  string
	task  *targetSpec // the task that makes the files, or nil for a glob
	paths []string    // cleaned, relative to the BUILD file or the task's source; directories include their contents
}

// targetSpec runs on another target's image, declared by a call to layer() or task(). A layer's
// image becomes the base of later targets; a task's image is discarded once its output is collected.
type targetSpec struct {
	name    string
	base    string   // name of an earlier pull or layer, or scratch
	steps   []step   // applied in order
	command []string // a layer's default command
	source  string   // a task's directory of output
	output  *fileSet // a task's output, found in source; nil for a layer
}

// sets returns the file sets that s's steps add, in order.
func (s *targetSpec) sets() []*fileSet {
	var sets []*fileSet
	for _, in := range s.steps {
		if set := in.source(); set != nil {
			sets = append(sets, set)
		}
	}
	return sets
}

// madeStep is a step made by a call to the builtin kind at pos.
type madeStep struct {
	step step
	kind string
	pos  syntax.Position
}

// loadBuildFile executes dir/BUILD from fsys, which must call layer() at least once.
// Every target but the last layer must be used by a later target, as its base or added by it,
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

	file := &buildFile{path: buildPath, kinds: map[string]string{}}
	fileSets := map[string]*fileSet{}    // globs and tasks, by name
	declared := map[string]declaration{} // each target's name
	used := map[string]bool{}            // names of targets that are a base or added
	var images, files []string           // names of pulls and layers, and of globs and tasks, in order
	var made []madeStep                  // every step made, in order
	listed := map[step]bool{}            // steps in some target's list

	// declare records name for the target being declared by the builtin running in thread.
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
		file.names = append(file.names, name)
		file.kinds[name] = fn
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

	// add implements add(source, dest), checking the targets it names as it is called.
	add := starlark.NewBuiltin("add", func(
		_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
	) (starlark.Value, error) {
		var name, dest string
		if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
			"source", &name,
			"dest", &dest,
		); err != nil {
			return nil, err
		}

		// Sources must come earlier, so targets can't form a cycle.
		set, ok := fileSets[name]
		if !ok {
			if d, ok := declared[name]; ok {
				return nil, fmt.Errorf("%s: source %q is a %s(), not a glob() or task()", fn.Name(), name, d.kind)
			}
			return nil, fmt.Errorf("%s: source %q is not an earlier glob() or task()%s",
				fn.Name(), name, describeNames("globs and tasks", files))
		}
		if err := checkPath("dest", dest); err != nil {
			return nil, fmt.Errorf("%s: %w", fn.Name(), err)
		}
		return &addStep{set: set, dest: dest}, nil
	})

	// addTarget checks and declares s, made by a call to fn.
	addTarget := func(thread *starlark.Thread, fn *starlark.Builtin, s *targetSpec) error {
		// Bases must come earlier, so targets can't form a cycle.
		if d, ok := declared[s.base]; ok && fileSets[s.base] != nil {
			return fmt.Errorf("%s: base %q is a %s(), not a pull() or layer()", fn.Name(), s.base, d.kind)
		} else if !ok && s.base != scratch {
			return fmt.Errorf("%s: base %q is not \"scratch\" or an earlier pull() or layer()%s",
				fn.Name(), s.base, describeNames("targets", images))
		}
		if err := declare(thread, s.name); err != nil {
			return fmt.Errorf("%s: %w", fn.Name(), err)
		}
		used[s.base] = true
		for _, in := range s.steps {
			listed[in] = true
		}
		for _, set := range s.sets() {
			used[set.name] = true
		}
		file.targets = append(file.targets, s)
		return nil
	}

	layer := starlark.NewBuiltin("layer", func(
		thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
	) (starlark.Value, error) {
		var name, base string
		var steps, command *starlark.List
		if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
			"name", &name,
			"base", &base,
			"steps?", &steps,
			"command?", &command,
		); err != nil {
			return nil, err
		}

		s := &targetSpec{name: name, base: base}
		var err error
		if s.steps, err = stepList(fn.Name(), steps); err != nil {
			return nil, err
		}
		if s.command, err = stringList(fn.Name(), "command", command); err != nil {
			return nil, err
		}
		if err := addTarget(thread, fn, s); err != nil {
			return nil, err
		}
		return starlark.None, nil
	})

	task := starlark.NewBuiltin("task", func(
		thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
	) (starlark.Value, error) {
		var name, base, source string
		var steps, include *starlark.List
		if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
			"name", &name,
			"base", &base,
			"source", &source,
			"include", &include,
			"steps?", &steps,
		); err != nil {
			return nil, err
		}

		s := &targetSpec{name: name, base: base, source: source}
		var err error
		if s.steps, err = stepList(fn.Name(), steps); err != nil {
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
		if err := addTarget(thread, fn, s); err != nil {
			return nil, err
		}
		fileSets[name] = s.output
		return starlark.None, nil
	})

	// track wraps a builtin that makes steps, recording each one it makes with where, so that
	// one left out of every target's steps is reported rather than silently ignored.
	track := func(b *starlark.Builtin) *starlark.Builtin {
		return starlark.NewBuiltin(b.Name(), func(
			thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
		) (starlark.Value, error) {
			v, err := b.CallInternal(thread, args, kwargs)
			if in, ok := v.(step); ok {
				made = append(made, madeStep{step: in, kind: b.Name(), pos: thread.CallFrame(1).Pos})
			}
			return v, err
		})
	}

	thread := &starlark.Thread{Name: "BUILD"}
	predeclared := starlark.StringDict{"glob": glob, "pull": pull, "add": track(add), "layer": layer, "task": task}
	for name, b := range stepBuiltins {
		predeclared[name] = track(b.(*starlark.Builtin))
	}
	if _, err := starlark.ExecFileOptions(&syntax.FileOptions{}, thread, buildPath, src, predeclared); err != nil {
		if evalErr, ok := err.(*starlark.EvalError); ok {
			return nil, errors.New(describeEvalError(evalErr))
		}
		return nil, err
	}
	if !slices.ContainsFunc(file.targets, func(s *targetSpec) bool { return s.output == nil }) {
		return nil, fmt.Errorf("%s: layer() was never called", buildPath)
	}

	for _, m := range made {
		if !listed[m.step] {
			return nil, fmt.Errorf("%s: %s: %s is not in the steps of any layer() or task()",
				m.pos, m.kind, m.step)
		}
	}

	// A target nothing uses would be silently left out of the image.
	final := file.targets[file.final()].name
	for _, name := range files {
		if !used[name] {
			d := declared[name]
			return nil, fmt.Errorf("%s: %s: %q is not added by any later layer() or task()",
				d.pos, d.kind, name)
		}
	}
	for _, p := range file.pulls {
		if !used[p.name] {
			return nil, fmt.Errorf("%s: pull: %q is not the base of any layer() or task()",
				declared[p.name].pos, p.name)
		}
	}
	for _, s := range file.targets {
		if s.output == nil && !used[s.name] && s.name != final {
			return nil, fmt.Errorf("%s: layer: %q is not the base of any later layer() or task(); "+
				"only the last layer() makes the image", declared[s.name].pos, s.name)
		}
	}
	return file, nil
}

// parseTargets checks the names of targets to build, given on the command line, and returns them
// without repeats.
func (f *buildFile) parseTargets(names []string) ([]string, error) {
	var targets []string
	for _, name := range names {
		switch f.kinds[name] {
		case "":
			hint := "; its targets are " + quoteNames(f.names)
			if closest := closestName(name, f.names); closest != "" {
				hint = fmt.Sprintf("; did you mean %q?", closest)
			}
			return nil, fmt.Errorf("%s has no target %q%s", f.path, name, hint)
		case "glob":
			return nil, fmt.Errorf("%q is a glob(), whose files are read by every build; "+
				"name a pull(), layer() or task() to build", name)
		}
		if !slices.Contains(targets, name) {
			targets = append(targets, name)
		}
	}
	return targets, nil
}

// needs returns the names of the pulls, layers and tasks that building targets runs:
// the targets themselves, and the targets they use, directly or not.
func (f *buildFile) needs(targets []string) map[string]bool {
	needed := map[string]bool{}
	for _, name := range targets {
		needed[name] = true
	}
	// Targets use only earlier targets, so one pass from the last finds everything.
	for _, s := range slices.Backward(f.targets) {
		if !needed[s.name] {
			continue
		}
		needed[s.base] = true
		for _, set := range s.sets() {
			if set.task != nil {
				needed[set.task.name] = true
			}
		}
	}
	return needed
}

// closestName returns the name in names nearest to name, as a likely misspelling of it, or
// "" if none is near enough.
func closestName(name string, names []string) string {
	closest, best := "", max(2, len(name)/3)+1
	for _, candidate := range names {
		if d := editDistance(name, candidate); d < best {
			closest, best = candidate, d
		}
	}
	return closest
}

// editDistance returns the number of single-byte insertions, deletions and substitutions that
// turn a into b.
func editDistance(a, b string) int {
	row := make([]int, len(b)+1)
	for j := range row {
		row[j] = j
	}
	for i := 1; i <= len(a); i++ {
		diagonal := row[0]
		row[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			diagonal, row[j] = row[j], min(row[j]+1, row[j-1]+1, diagonal+cost)
		}
	}
	return row[len(b)]
}

// declaration records where a target's name was declared, and by which builtin.
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

// describeNames lists the targets of a kind, such as "globs", declared so far, for an error
// about an unknown name.
func describeNames(kind string, names []string) string {
	if len(names) == 0 {
		return "; there are none before this"
	}
	return "; earlier " + kind + " are " + quoteNames(names)
}

// quoteNames lists names, quoted, for a message.
func quoteNames(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = strconv.Quote(name)
	}
	return strings.Join(quoted, ", ")
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
