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

// workspace is a project's BUILD files, each in its own directory, or package, loaded as targets name them.
// A package is named by its directory relative to the workspace's root, "" for the root itself, and a
// target by its label, "//dir:name".
type workspace struct {
	fs       afero.Fs
	root     string                // absolute: the directory holding the WORKSPACE file, or else the only package
	current  string                // the package of -C, whose targets are named without a label
	packages map[string]*buildFile // those loaded, by package
	loading  []string              // packages being loaded, innermost last, to report a cycle
	pulls    []*pullSpec           // of every package loaded, in the order declared
	targets  []*targetSpec         // layers and tasks of every package loaded, in the order declared
}

// buildFile is the targets declared by the BUILD file of one package.
type buildFile struct {
	pkg   string
	dir   string                 // absolute: the package's directory, which globs are relative to
	path  string                 // of the BUILD file, for messages
	names []string               // of every target, globs included, in the order declared
	decls map[string]declaration // each target, by name
}

// declaration records a target of a BUILD file: where it was declared, and by which builtin.
type declaration struct {
	kind string // "glob", "pull", "layer" or "task"
	pos  syntax.Position
	set  *fileSet // a glob's or task's files, or nil for a pull or layer
}

// scratch is the base name for an empty image, as in a Dockerfile's "FROM scratch".
const scratch = "scratch"

// pullSpec is an image to fetch from a registry, declared by a call to pull().
type pullSpec struct {
	label string
	image reference.NamedTagged
}

// fileSet is a set of files that add() can name in its source: files in a package, declared by
// glob(), or files a task makes.
type fileSet struct {
	label string      // of the glob or task
	task  *targetSpec // the task that makes the files, or nil for a glob
	dir   string      // a glob's package directory
	sel   *selection  // the files, within dir or the task's source
}

// targetSpec runs on another target's image, declared by a call to layer() or task(). A layer's
// image becomes the base of later targets; a task's image is discarded once its output, if any, is
// collected. A task without output, such as one that runs tests, is run only for its steps to succeed.
type targetSpec struct {
	label   string
	task    bool     // declared by task(), not layer()
	base    string   // label of a pull or layer declared before it, or scratch
	steps   []step   // applied in order
	command []string // a layer's default command
	source  string   // a task's directory of output
	output  *fileSet // a task's output, found in source; nil for a layer, or a task without output
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

// loadError is an error in a BUILD file, which already says where. When loading a package's
// BUILD file fails as another names one of its targets, the error is reported as it is, rather
// than as an error of the call that named the target.
type loadError struct{ err error }

func (e *loadError) Error() string { return e.err.Error() }
func (e *loadError) Unwrap() error { return e.err }

// openWorkspace returns the workspace that holds dir, the directory given with -C: the nearest
// directory at or above dir with a WORKSPACE file, or else dir alone. It loads no BUILD file yet.
func openWorkspace(fsys afero.Fs, dir string) (*workspace, error) {
	info, err := fsys.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("directory %s does not exist", dir)
	} else if err != nil {
		return nil, err
	} else if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	root := dir
	for d := dir; ; d = filepath.Dir(d) {
		// Any error but a missing file stops the search, rather than choosing another root.
		if _, err := fsys.Stat(filepath.Join(d, "WORKSPACE")); err == nil {
			root = d
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("looking for a WORKSPACE file: %w", err)
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	current, err := filepath.Rel(root, dir)
	if err != nil {
		return nil, err
	}
	if current == "." {
		current = ""
	}
	return &workspace{
		fs:       fsys,
		root:     root,
		current:  filepath.ToSlash(current),
		packages: map[string]*buildFile{},
	}, nil
}

// label returns the label of the target name in package pkg.
func label(pkg, name string) string {
	return "//" + pkg + ":" + name
}

// parseLabel returns the package and name of the target that ref names from package pkg: "name"
// or ":name" for a target of pkg itself, or "//dir:name" for one in the package at dir. The name
// of a target in pkg isn't checked, since it is only looked up there.
func parseLabel(ref, pkg string) (string, string, error) {
	rest, ok := strings.CutPrefix(ref, "//")
	if !ok {
		return pkg, strings.TrimPrefix(ref, ":"), nil
	}
	dir, name, ok := strings.Cut(rest, ":")
	if !ok {
		return "", "", fmt.Errorf(`label %q must end with ":" and a target's name`, ref)
	}
	if dir != "" && (dir == "." || path.Clean(dir) != dir || !filepath.IsLocal(dir)) {
		return "", "", fmt.Errorf("label %q: %q is not a clean path to a directory within the workspace", ref, dir)
	}
	if err := checkName(name); err != nil {
		return "", "", fmt.Errorf("label %q: %w", ref, err)
	}
	return dir, name, nil
}

// checkName reports whether name can name a target.
func checkName(name string) error {
	switch {
	case name == "":
		return errors.New("name must not be empty")
	case name == scratch:
		return fmt.Errorf("name %q is reserved for the empty base image", name)
	case strings.ContainsAny(name, ":/"):
		return fmt.Errorf(`name %q must not contain ":" or "/", which labels use`, name)
	}
	return nil
}

// display names the target with label l for messages: by its name alone if it is in the package of -C.
func (w *workspace) display(l string) string {
	rest := strings.TrimPrefix(l, "//")
	i := strings.LastIndexByte(rest, ':')
	if rest[:i] == w.current {
		return rest[i+1:]
	}
	return l
}

// load returns the BUILD file of package pkg, loading it if it hasn't been. Loading it loads the
// packages whose targets it names too, before the targets that name them are declared, so every
// target is declared after those it uses.
func (w *workspace) load(pkg string) (*buildFile, error) {
	if f, ok := w.packages[pkg]; ok {
		return f, nil
	}
	if i := slices.Index(w.loading, pkg); i >= 0 {
		var cycle []string
		for _, p := range append(slices.Clone(w.loading[i:]), pkg) {
			cycle = append(cycle, "//"+p+":")
		}
		return nil, fmt.Errorf("packages name each other's targets in a cycle: %s", strings.Join(cycle, " -> "))
	}
	if err := w.checkPackageDir(pkg); err != nil {
		return nil, err
	}
	dir := filepath.Join(w.root, filepath.FromSlash(pkg))
	buildPath := filepath.Join(dir, "BUILD")
	src, err := afero.ReadFile(w.fs, buildPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no BUILD file in %s", dir)
	} else if err != nil {
		return nil, err
	}

	w.loading = append(w.loading, pkg)
	defer func() { w.loading = w.loading[:len(w.loading)-1] }()
	f := &buildFile{pkg: pkg, dir: dir, path: buildPath, decls: map[string]declaration{}}
	if err := w.exec(f, src); err != nil {
		var nested *loadError
		if !errors.As(err, &nested) {
			nested = &loadError{err}
		}
		return nil, nested
	}
	w.packages[pkg] = f
	return f, nil
}

// checkPackageDir reports a symbolic link on the way from the workspace's root to package pkg, which could
// lead outside the workspace, as checkInclude does for a glob's files. The root itself may be reached
// through a link, as from a linked $PWD.
func (w *workspace) checkPackageDir(pkg string) error {
	if pkg == "" {
		return nil
	}
	current := w.root
	for _, component := range strings.Split(pkg, "/") {
		current = filepath.Join(current, component)
		info, _, err := lstat(w.fs, current)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // reported as a missing BUILD file
		} else if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("package %q: %s is a symbolic link; a package must be a directory within the workspace",
				pkg, current)
		}
	}
	return nil
}

// lookup returns the BUILD file that declares the target ref names from f, a BUILD file being
// loaded, and the target's name there, which it may lack: f itself has only the targets declared
// so far. Another package is loaded if it hasn't been.
func (w *workspace) lookup(f *buildFile, ref string) (*buildFile, string, error) {
	pkg, name, err := parseLabel(ref, f.pkg)
	if err != nil {
		return nil, "", err
	}
	if pkg == f.pkg {
		return f, name, nil
	}
	other, err := w.load(pkg)
	return other, name, err
}

// labelHint suggests the label meant by name, which names no target since it has a ":", as a label
// missing its leading "//". A name can't contain ":", so this is likely.
func labelHint(name string) string {
	return fmt.Sprintf(`; a label starts with "//", as in "//%s"`, name)
}

// namesOf returns the names of f's targets of the given kinds, in the order declared.
func (f *buildFile) namesOf(kinds ...string) []string {
	var names []string
	for _, name := range f.names {
		if slices.Contains(kinds, f.decls[name].kind) {
			names = append(names, name)
		}
	}
	return names
}

// missing reports that f has no target name, suggesting the one meant.
func (f *buildFile) missing(name string) error {
	hint := "; its targets are " + quoteNames(f.names)
	if strings.Contains(name, ":") {
		hint = labelHint(name)
	} else if len(f.names) == 0 {
		hint = "; it declares none"
	} else if closest := closestName(name, f.names); closest != "" {
		hint = fmt.Sprintf("; did you mean %q?", closest)
	}
	return fmt.Errorf("%s has no target %q%s", f.path, name, hint)
}

// exec executes the BUILD file f, whose source is src, declaring its targets in f and w. The files
// named by its globs are checked only once they are needed, by checkSources.
func (w *workspace) exec(f *buildFile, src []byte) error {
	var made []madeStep       // every step made, in order
	listed := map[step]bool{} // steps in some target's list

	// declare records name for the target being declared by the builtin running in thread.
	declare := func(thread *starlark.Thread, name string, set *fileSet) error {
		if err := checkName(name); err != nil {
			return err
		}
		if d, ok := f.decls[name]; ok {
			return fmt.Errorf("name %q is already used at %s", name, d.pos)
		}
		f.decls[name] = declaration{kind: thread.CallFrame(0).Name, pos: thread.CallFrame(1).Pos, set: set}
		f.names = append(f.names, name)
		return nil
	}

	glob := starlark.NewBuiltin("glob", func(
		thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
	) (starlark.Value, error) {
		var name string
		var include, exclude *starlark.List
		sel := defaultSelection()
		params := append([]any{"name", &name}, sel.params(&include, &exclude, true)...)
		if err := starlark.UnpackArgs(fn.Name(), args, kwargs, params...); err != nil {
			return nil, err
		}
		if err := sel.selectFiles(fn.Name(), include, exclude, f.dir); err != nil {
			return nil, err
		}
		set := &fileSet{label: label(f.pkg, name), dir: f.dir, sel: sel}
		if err := declare(thread, name, set); err != nil {
			return nil, fmt.Errorf("%s: %w", fn.Name(), err)
		}
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
		if err := declare(thread, name, nil); err != nil {
			return nil, fmt.Errorf("%s: %w", fn.Name(), err)
		}
		w.pulls = append(w.pulls, &pullSpec{label: label(f.pkg, name), image: ref})
		return starlark.None, nil
	})

	// resolve returns the label and declaration of the target that ref, the argument param of a call to fn,
	// names: a pull or layer if image is set, or else a glob or task. Targets of f must be declared earlier,
	// so targets can't form a cycle.
	resolve := func(fn *starlark.Builtin, param, ref string, image bool) (string, declaration, error) {
		kinds, listed, scratchHint := []string{"glob", "task"}, "globs and tasks", ""
		if image {
			kinds, listed, scratchHint = []string{"pull", "layer"}, "targets", `"scratch" or `
		}
		want := kinds[0] + "() or " + kinds[1] + "()"
		from, name, err := w.lookup(f, ref)
		if err != nil {
			return "", declaration{}, fmt.Errorf("%s: %s %q: %w", fn.Name(), param, ref, err)
		}
		d, ok := from.decls[name]
		switch {
		case !ok && from == f && !image && strings.Contains(name, ":"):
			return "", declaration{}, fmt.Errorf("%s: %s %q is not an earlier %s%s",
				fn.Name(), param, ref, want, labelHint(name))
		case !ok && from == f:
			return "", declaration{}, fmt.Errorf("%s: %s %q is not %san earlier %s%s",
				fn.Name(), param, ref, scratchHint, want, describeNames(listed, f.namesOf(kinds...)))
		case !ok:
			return "", declaration{}, fmt.Errorf("%s: %s %q: %w", fn.Name(), param, ref, from.missing(name))
		case !slices.Contains(kinds, d.kind):
			return "", declaration{}, fmt.Errorf("%s: %s %q is a %s(), not a %s", fn.Name(), param, ref, d.kind, want)
		case !image && d.set == nil:
			return "", declaration{}, fmt.Errorf("%s: %s %q is a task() without output; give it source and include",
				fn.Name(), param, ref)
		}
		return label(from.pkg, name), d, nil
	}

	// add implements add(source, dest, owner, group), checking the target it names as it is called.
	add := starlark.NewBuiltin("add", func(
		_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
	) (starlark.Value, error) {
		var ref, dest, owner, group string
		if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
			"source", &ref,
			"dest", &dest,
			"owner?", &owner,
			"group?", &group,
		); err != nil {
			return nil, err
		}

		_, d, err := resolve(fn, "source", ref, false)
		if err != nil {
			return nil, err
		}
		if err := checkPath("dest", dest); err != nil {
			return nil, fmt.Errorf("%s: %w", fn.Name(), err)
		}
		for _, id := range [][2]string{{"owner", owner}, {"group", group}} {
			if strings.Contains(id[1], ":") {
				return nil, fmt.Errorf(`%s: %s %q must not contain ":"; give owner and group apart`,
					fn.Name(), id[0], id[1])
			}
		}
		return &addStep{ref: ref, set: d.set, dest: dest, owner: owner, group: group}, nil
	})

	// addTarget checks and declares s, named name, made by a call to fn. Its base is resolved to a label.
	addTarget := func(thread *starlark.Thread, fn *starlark.Builtin, name string, s *targetSpec) error {
		if s.base != scratch {
			l, _, err := resolve(fn, "base", s.base, true)
			if err != nil {
				return err
			}
			s.base = l
		}
		if err := declare(thread, name, s.output); err != nil {
			return fmt.Errorf("%s: %w", fn.Name(), err)
		}
		for _, in := range s.steps {
			listed[in] = true
		}
		w.targets = append(w.targets, s)
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

		s := &targetSpec{label: label(f.pkg, name), base: base}
		var err error
		if s.steps, err = stepList(fn.Name(), steps); err != nil {
			return nil, err
		}
		if s.command, err = stringList(fn.Name(), "command", command); err != nil {
			return nil, err
		}
		if err := addTarget(thread, fn, name, s); err != nil {
			return nil, err
		}
		return starlark.None, nil
	})

	task := starlark.NewBuiltin("task", func(
		thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
	) (starlark.Value, error) {
		var name, base, source string
		var steps, include, exclude *starlark.List
		sel := defaultSelection()
		params := append([]any{
			"name", &name,
			"base", &base,
			"steps?", &steps,
			"source?", &source,
		}, sel.params(&include, &exclude, false)...)
		if err := starlark.UnpackArgs(fn.Name(), args, kwargs, params...); err != nil {
			return nil, err
		}

		s := &targetSpec{label: label(f.pkg, name), task: true, base: base, source: source}
		var err error
		if s.steps, err = stepList(fn.Name(), steps); err != nil {
			return nil, err
		}
		// A task makes files for add() only if it says where they are.
		switch {
		case source == "" && include != nil:
			return nil, fmt.Errorf("%s: include needs source, the directory it is relative to", fn.Name())
		case source != "" && include == nil:
			return nil, fmt.Errorf("%s: source needs include, the files to collect from it", fn.Name())
		case source != "":
			if !path.IsAbs(source) {
				return nil, fmt.Errorf("%s: source %q must be an absolute path", fn.Name(), source)
			}
			if err := sel.selectFiles(fn.Name(), include, exclude, source); err != nil {
				return nil, err
			}
			s.output = &fileSet{label: s.label, task: s, sel: sel}
		default:
			for _, kv := range kwargs {
				if param := string(kv[0].(starlark.String)); isSelectionParam(param) {
					return nil, fmt.Errorf("%s: %s needs source and include, the files to collect", fn.Name(), param)
				}
			}
		}
		if err := addTarget(thread, fn, name, s); err != nil {
			return nil, err
		}
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

	thread := &starlark.Thread{Name: f.path}
	predeclared := starlark.StringDict{"glob": glob, "pull": pull, "add": track(add), "layer": layer, "task": task}
	for name, b := range stepBuiltins {
		predeclared[name] = track(b.(*starlark.Builtin))
	}
	if _, err := starlark.ExecFileOptions(&syntax.FileOptions{}, thread, f.path, src, predeclared); err != nil {
		// An error in another package's BUILD file already says where it is.
		var nested *loadError
		if errors.As(err, &nested) {
			return nested
		}
		if evalErr, ok := err.(*starlark.EvalError); ok {
			return errors.New(describeEvalError(evalErr))
		}
		return err
	}

	for _, m := range made {
		if !listed[m.step] {
			return fmt.Errorf("%s: %s: %s is not in the steps of any layer() or task()", m.pos, m.kind, m.step)
		}
	}
	return nil
}

// target returns the label and kind of the target that ref, given on the command line, names from
// the package of -C, loading the BUILD file that declares it.
func (w *workspace) target(ref string) (string, string, error) {
	pkg, name, err := parseLabel(ref, w.current)
	if err != nil {
		return "", "", err
	}
	f, err := w.load(pkg)
	if err != nil {
		return "", "", err
	}
	d, ok := f.decls[name]
	if !ok {
		return "", "", f.missing(name)
	}
	return label(pkg, name), d.kind, nil
}

// buildTargets returns the labels of the targets to build that names, given on the command line,
// name, without repeats.
func (w *workspace) buildTargets(names []string) ([]string, error) {
	var labels []string
	for _, name := range names {
		l, kind, err := w.target(name)
		if err != nil {
			return nil, err
		}
		if kind == "glob" {
			return nil, fmt.Errorf("%q is a glob(), whose files are read by the targets that add them; "+
				"name a pull(), layer() or task() to build", name)
		}
		if !slices.Contains(labels, l) {
			labels = append(labels, l)
		}
	}
	return labels, nil
}

// imageTarget returns the label of the target whose image to send that name, given on the
// command line, names.
func (w *workspace) imageTarget(name string) (string, error) {
	l, kind, err := w.target(name)
	if err != nil {
		return "", err
	}
	switch kind {
	case "glob":
		return "", fmt.Errorf("%q is a glob(), whose files are read by the targets that add them, not an image; "+
			"name a pull() or layer() to send", name)
	case "task":
		return "", fmt.Errorf("%q is a task(), whose image is discarded once it runs; "+
			"name a pull() or layer() to send", name)
	}
	return l, nil
}

// declaration returns the declaration of the target with label l, whose package is loaded.
func (w *workspace) declaration(l string) declaration {
	rest := strings.TrimPrefix(l, "//")
	i := strings.LastIndexByte(rest, ':')
	return w.packages[rest[:i]].decls[rest[i+1:]]
}

// needs returns the labels of the pulls, layers and tasks that building targets runs:
// the targets themselves, and the targets they use, directly or not.
func (w *workspace) needs(targets []string) map[string]bool {
	needed := map[string]bool{}
	for _, l := range targets {
		needed[l] = true
	}
	// Targets use only targets declared before them, so one pass from the last finds everything.
	for _, s := range slices.Backward(w.targets) {
		if !needed[s.label] {
			continue
		}
		needed[s.base] = true
		for _, set := range s.sets() {
			if set.task != nil {
				needed[set.task.label] = true
			}
		}
	}
	return needed
}

// checkSources reports a glob added by a target in needed whose patterns' prefixes aren't in its package,
// before anything is built. Whether its patterns match files is found as they are read.
func (w *workspace) checkSources(needed map[string]bool) error {
	checked := map[*fileSet]bool{}
	for _, s := range w.targets {
		if !needed[s.label] {
			continue
		}
		for _, set := range s.sets() {
			if set.task != nil || checked[set] {
				continue
			}
			checked[set] = true
			for _, p := range set.sel.include {
				if err := checkInclude(w.fs, set.dir, p); err != nil {
					return fmt.Errorf("%s: glob: %w", w.declaration(set.label).pos, err)
				}
			}
		}
	}
	return nil
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
