package load

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Topher-the-Geek/vamos/internal/cache"
	"github.com/Topher-the-Geek/vamos/internal/fileset"
	"github.com/Topher-the-Geek/vamos/internal/graph"
	"github.com/Topher-the-Geek/vamos/internal/tracing"

	"github.com/containerd/platforms"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/afero"
	"go.podman.io/image/v5/docker/reference"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// workspace is a project's BUILD files, each in its own directory, or package, loaded as targets name them.
// A package is named by its directory relative to the workspace's root, "" for the root itself, and a
// target by its label, "//dir:name".
type workspace struct {
	fs           afero.Fs
	root         string                // absolute: the directory holding the WORKSPACE file, or else the only package
	current      string                // the package of -C, whose targets are named without a label
	packages     map[string]*buildFile // those loaded, by package
	loading      []string              // packages being loaded, innermost last, to report a cycle
	modules      map[string]*module    // .star files loaded, by label
	execPlatform *platformValue        // the executor's, which run() steps run on without emulation, as EXEC_PLATFORM
	platform     *platformValue        // the command's, given with --platform: the named target's, unless an index
	configs      map[string]*config    // every configuration made, by key, so that equal ones are the same
	builtins     starlark.StringDict   // of BUILD and .star files, made once
}

// buildFile is the targets declared by the BUILD file of one package.
type buildFile struct {
	pkg   string
	dir   string                  // absolute: the package's directory, which globs are relative to
	path  string                  // of the BUILD file, for messages
	names []string                // of every target, globs included, in the order declared
	decls map[string]*declaration // each target, by name
}

// declaration records a target of a BUILD file: where it was declared, by which builtin, and its template, the
// target as the call declaring it describes it. In a template, targets that it uses are named by targetRefs;
// configuring the target then makes an instance of it, in which they are targets in configurations.
//
// The template is made, and checked, as the BUILD file runs, unless an attribute is given by a function of
// ctx. Then the call's arguments are kept instead, and a template is made in each configuration the target is
// needed in.
type declaration struct {
	kind      string // "glob", "pull", "layer", "task" or "index"
	pos       syntax.Position
	file      *buildFile
	params    []*param // that params declares, in order, which are known before any configuration
	hasOutput bool     // a glob, or a task given source, whose files add() can name
	static    any      // the template, if no attribute is a function: a *fileSet, *pullSpec, *targetSpec or *indexSpec

	// Otherwise, the call's arguments, of which some are functions of ctx, and how to make the template.
	fn     *starlark.Builtin
	args   starlark.Tuple
	kwargs []starlark.Tuple
	make   makeFunc
}

// makeFunc makes the template of a target from the arguments of the call to fn that declares it, checking
// them, and returns the target's name too. It runs in c: as the BUILD file declares the target, or in a
// configuration, with what the functions of ctx among the arguments return in their place. The arguments don't
// include params, which the declaration holds.
type makeFunc func(c *evalContext, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (
	string, any, error)

// module is a .star file, run once as load() first names it.
type module struct {
	globals starlark.StringDict
	err     error
	loading bool // while it runs, to report a cycle of loads
}

// evalMode says what Starlark code is running, which decides what builtins may do.
type evalMode int

const (
	declaring     evalMode = iota // a BUILD file, declaring its targets
	configuring                   // a function of ctx, giving an attribute of a target being configured
	loadingModule                 // the top level of a .star file, as load() runs it
)

// evalContext is what builtins know of the thread calling them, kept in its locals under contextKey.
type evalContext struct {
	mode     evalMode
	file     *buildFile    // the BUILD file running, or declaring the target being configured; nil in a module
	made     []madeStep    // while declaring: every step made, in order
	listed   map[step]bool // while declaring: steps in some target's list
	deferred bool          // while declaring: some target's steps are given by a function of ctx
}

// contextKey is the thread local holding a thread's evalContext.
const contextKey = "vamos.context"

// contextOf returns the evalContext of thread.
func contextOf(thread *starlark.Thread) *evalContext {
	return thread.Local(contextKey).(*evalContext)
}

// scratch is the base name for an empty image, as in a Dockerfile's "FROM scratch".
const scratch = "scratch"

// pullSpec is an image to fetch from a registry, declared by a call to pull().
type pullSpec struct {
	label string
	id    confLabel // of an instance: the pull in its configuration
	image reference.NamedTagged
}

// fileSet is a set of files that add() can name in its source: files in a package, declared by
// glob(), or files a task makes.
type fileSet struct {
	label       string             // of the glob or task
	task        *targetSpec        // the task that makes the files, or nil for a glob
	dir         string             // a glob's package directory
	sel         *fileset.Selection // the files, within dir or the task's source
	configuring string             // a glob given by a function of ctx: the target and configuration, for errors
}

// targetSpec runs on another target's image, declared by a call to layer() or task(). A layer's
// image becomes the base of later targets; a task's image is discarded once its output, if any, is
// collected. A task without output, such as one that runs tests, is run only for its steps to succeed.
type targetSpec struct {
	label   string
	id      confLabel // of an instance: the target in its configuration
	task    bool      // declared by task(), not layer()
	baseRef targetRef // of a template: a pull or layer, or scratch
	base    confLabel // of an instance: the base in its configuration
	steps   []step    // applied in order
	command []string  // a layer's default command
	source  string    // a task's directory of output
	output  *fileSet  // a task's output, found in source; nil for a layer, or a task without output
}

// indexSpec is an index of images for several platforms, declared by a call to index(): its manifests, each a
// pull or layer, named with the platform it is for.
type indexSpec struct {
	manifests []targetRef // in the order given, which the index keeps
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
// directory at or above dir with a WORKSPACE file, or else dir alone. A WORKSPACE file only marks the
// workspace's root. It loads no BUILD file yet. exec is the executor's platform, which targets are built for
// unless --platform gives another.
func openWorkspace(fsys afero.Fs, dir string, exec imgspecv1.Platform) (*workspace, error) {
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
	// The graph's platforms must be normalized, and an executor's may not be.
	exec = platforms.Normalize(exec)
	return &workspace{
		fs:           fsys,
		root:         root,
		current:      filepath.ToSlash(current),
		packages:     map[string]*buildFile{},
		modules:      map[string]*module{},
		execPlatform: &platformValue{exec},
		platform:     &platformValue{exec},
		configs:      map[string]*config{},
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
	case strings.Contains(name, "\x00"):
		return fmt.Errorf("name %q must not contain a NUL byte", name)
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
	f := &buildFile{pkg: pkg, dir: dir, path: buildPath, decls: map[string]*declaration{}}
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

// lookup returns the BUILD file that declares the target ref names from package from, and the target's name
// there, which it may lack: f, a BUILD file that may be being loaded, has only the targets declared so far.
// Another package is loaded if it hasn't been.
func (w *workspace) lookup(f *buildFile, ref, from string) (*buildFile, string, error) {
	pkg, name, err := parseLabel(ref, from)
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
	hint := "; its targets are " + fileset.QuoteNames(f.names)
	if strings.Contains(name, ":") {
		hint = labelHint(name)
	} else if len(f.names) == 0 {
		hint = "; it declares none"
	} else if closest := closestName(name, f.names); closest != "" {
		hint = fmt.Sprintf("; did you mean %q?", closest)
	}
	return fmt.Errorf("%s has no target %q%s", f.path, name, hint)
}

// exec executes the BUILD file f, whose source is src, declaring its targets in f. The files named by its
// globs are read only once a build needs them, by readGlobs.
func (w *workspace) exec(f *buildFile, src []byte) error {
	c := &evalContext{mode: declaring, file: f, listed: map[step]bool{}}
	thread := &starlark.Thread{Name: f.path, Load: w.loadModule}
	thread.SetLocal(contextKey, c)
	if _, err := starlark.ExecFileOptions(&syntax.FileOptions{}, thread, f.path, src, w.predeclared()); err != nil {
		return describeExecError(err)
	}

	// A step made as the file runs may be listed only by a function of ctx giving steps, which runs later.
	if c.deferred {
		return nil
	}
	for _, m := range c.made {
		if !c.listed[m.step] {
			return fmt.Errorf("%s: %s: %s is not in the steps of any layer() or task()", m.pos, m.kind, m.step)
		}
	}
	return nil
}

// describeExecError returns err, from running a BUILD or .star file, as it is reported: as one line saying
// where. An error in another package's BUILD file already says where it is.
func describeExecError(err error) error {
	var nested *loadError
	if errors.As(err, &nested) {
		return nested
	}
	if evalErr, ok := err.(*starlark.EvalError); ok {
		return errors.New(describeEvalError(evalErr))
	}
	return err
}

// loadModule implements load(): it returns the globals of the .star file that label names, such as
// "//:defs.star", running it the first time it is named.
func (w *workspace) loadModule(_ *starlark.Thread, label string) (starlark.StringDict, error) {
	if !strings.HasPrefix(label, "//") || !strings.HasSuffix(label, ".star") {
		return nil, fmt.Errorf(`a .star file is named by its label, which starts with "//", such as "//:defs.star"`)
	}
	pkg, name, err := parseLabel(label, "")
	if err != nil {
		return nil, err
	}
	if m, ok := w.modules[label]; ok {
		if m.loading {
			return nil, fmt.Errorf("%s loads itself, through the files it loads", label)
		}
		return m.globals, m.err
	}
	if err := w.checkPackageDir(pkg); err != nil {
		return nil, err
	}
	path := filepath.Join(w.root, filepath.FromSlash(pkg), name)
	src, err := afero.ReadFile(w.fs, path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no file %s", path)
	} else if err != nil {
		return nil, err
	}

	m := &module{loading: true}
	w.modules[label] = m
	thread := &starlark.Thread{Name: path, Load: w.loadModule}
	thread.SetLocal(contextKey, &evalContext{mode: loadingModule})
	m.globals, m.err = starlark.ExecFileOptions(&syntax.FileOptions{}, thread, path, src, w.predeclared())
	if m.err != nil {
		m.err = describeExecError(m.err)
	}
	m.loading = false
	return m.globals, m.err
}

// predeclared returns the builtins of BUILD and .star files, which take what they need to know of the code
// calling them from the thread's evalContext.
func (w *workspace) predeclared() starlark.StringDict {
	if w.builtins != nil {
		return w.builtins
	}
	selectionNames := func(required bool) []string {
		params := selectionParams(fileset.DefaultSelection(), new(*starlark.List), new(*starlark.List), required)
		var names []string
		for i := 0; i < len(params); i += 2 {
			names = append(names, params[i].(string))
		}
		return names
	}
	globParams := append([]string{"name"}, selectionNames(true)...)
	taskParams := append([]string{"name", "base", "steps?", "source?"}, selectionNames(false)...)
	w.builtins = starlark.StringDict{
		"glob":          w.declarer("glob", globParams, w.makeGlob),
		"pull":          w.declarer("pull", []string{"name", "image", "tag?"}, w.makePull),
		"layer":         w.declarer("layer", []string{"name", "base", "steps?", "command?"}, w.makeLayer),
		"task":          w.declarer("task", taskParams, w.makeTask),
		"index":         w.declarer("index", []string{"name", "layer", "platforms?"}, w.makeIndex),
		"add":           track(starlark.NewBuiltin("add", w.newAdd)),
		"dep":           starlark.NewBuiltin("dep", w.newDep),
		"arg":           starlark.NewBuiltin("arg", newArg),
		"param":         starlark.NewBuiltin("param", newParamBuiltin),
		"EXEC_PLATFORM": execPlatformValue{},
	}
	for name, b := range stepBuiltins {
		w.builtins[name] = track(b.(*starlark.Builtin))
	}
	return w.builtins
}

// track wraps a builtin that makes steps, recording each one it makes as a BUILD file runs, with where, so
// that one left out of every target's steps is reported rather than silently ignored.
func track(b *starlark.Builtin) *starlark.Builtin {
	return starlark.NewBuiltin(b.Name(), func(
		thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
	) (starlark.Value, error) {
		v, err := b.CallInternal(thread, args, kwargs)
		if in, ok := v.(step); ok {
			if c := contextOf(thread); c.mode == declaring {
				c.made = append(c.made, madeStep{step: in, kind: b.Name(), pos: thread.CallFrame(1).Pos})
			}
		}
		return v, err
	})
}

// declarer returns the builtin kind, which declares a target, taking params, as starlark.UnpackArgs names
// them, of which the first is the target's name, and the target's own params, unless it is a pull. makeTemplate
// makes the target's template from the call's other arguments: as it is called, or, if an argument is a function
// of ctx, in each configuration the target is needed in.
func (w *workspace) declarer(kind string, params []string, makeTemplate makeFunc) *starlark.Builtin {
	return starlark.NewBuiltin(kind, func(
		thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
	) (starlark.Value, error) {
		c := contextOf(thread)
		switch c.mode {
		case configuring:
			return nil, fmt.Errorf("%s: a function of ctx can't declare a target; declare it as the BUILD file runs",
				fn.Name())
		case loadingModule:
			return nil, fmt.Errorf("%s: a .star file can't declare a target as it is loaded; "+
				"declare it in a function that a BUILD file calls", fn.Name())
		}
		d := &declaration{kind: kind, pos: thread.CallFrame(1).Pos, file: c.file}
		var err error
		if kwargs, d.params, err = takeParams(fn.Name(), kwargs); err != nil {
			return nil, err
		}
		if kind == "pull" && d.params != nil {
			return nil, fmt.Errorf("%s: a pull takes no parameters; only its platform configures it", fn.Name())
		}
		var name string
		if hasFunction(args, kwargs) {
			// Check what can be checked now: the parameters given, and the name.
			values := make([]starlark.Value, len(params))
			pairs := make([]any, 0, 2*len(params))
			for i, p := range params {
				pairs = append(pairs, p, &values[i])
			}
			if err := starlark.UnpackArgs(fn.Name(), args, kwargs, pairs...); err != nil {
				return nil, err
			}
			n, ok := values[0].(starlark.String)
			if !ok {
				return nil, fmt.Errorf("%s: name is %s, want string; a target's name can't depend on ctx",
					fn.Name(), values[0].Type())
			}
			name = string(n)
			// A base given as it is, not by a function, must name an earlier target, as when nothing is a function.
			if i := slices.Index(params, "base"); i >= 0 {
				if _, ok := values[i].(starlark.Callable); !ok {
					if _, err := w.makeBase(c, fn, values[i]); err != nil {
						return nil, err
					}
				}
			}
			d.hasOutput = kind == "glob"
			// A task has output given both source and include, as functions or not: makeTask refuses one
			// without the other, and include can't be None.
			if i := slices.Index(params, "source?"); i >= 0 {
				d.hasOutput = values[i] != nil && values[slices.Index(params, "include?")] != nil
			}
			// Steps listed by a function are known only once it runs; those in a list, now.
			if i := slices.Index(params, "steps?"); i >= 0 {
				switch v := values[i].(type) {
				case starlark.Callable:
					c.deferred = true
				case *starlark.List:
					for e := range v.Elements() {
						if in, ok := e.(step); ok {
							c.listed[in] = true
						}
					}
				}
			}
			d.fn, d.args, d.kwargs, d.make = fn, args, kwargs, makeTemplate
		} else {
			if name, d.static, err = makeTemplate(c, fn, args, kwargs); err != nil {
				return nil, err
			}
			if s, ok := d.static.(*targetSpec); ok {
				d.hasOutput = s.output != nil
				for _, in := range s.steps {
					c.listed[in] = true
				}
			} else {
				d.hasOutput = kind == "glob"
			}
		}
		// The arg()s given by functions are checked as they run, in template.
		var values []starlark.Value
		for _, v := range append(slices.Clone(args), kwargValues(kwargs)...) {
			if _, ok := v.(starlark.Callable); !ok {
				values = append(values, v)
			}
		}
		if err := checkArgNames(w.display(label(c.file.pkg, name)), d.params, values...); err != nil {
			return nil, err
		}
		if err := w.declare(c.file, name, d); err != nil {
			return nil, fmt.Errorf("%s: %w", fn.Name(), err)
		}
		return starlark.None, nil
	})
}

// takeParams returns kwargs, the keyword arguments of a call to fnName, without params, and the parameters that
// params declares, if given.
func takeParams(fnName string, kwargs []starlark.Tuple) ([]starlark.Tuple, []*param, error) {
	i := slices.IndexFunc(kwargs, func(kv starlark.Tuple) bool { return kv[0] == starlark.String("params") })
	if i < 0 {
		return kwargs, nil, nil
	}
	params, err := parseParams(fnName, kwargs[i][1])
	return slices.Delete(slices.Clone(kwargs), i, i+1), params, err
}

// kwargValues returns the values of a call's keyword arguments.
func kwargValues(kwargs []starlark.Tuple) []starlark.Value {
	values := make([]starlark.Value, len(kwargs))
	for i, kv := range kwargs {
		values[i] = kv[1]
	}
	return values
}

// hasFunction reports whether one of a call's arguments is a function, as of ctx.
func hasFunction(args starlark.Tuple, kwargs []starlark.Tuple) bool {
	for _, v := range args {
		if _, ok := v.(starlark.Callable); ok {
			return true
		}
	}
	for _, kv := range kwargs {
		if _, ok := kv[1].(starlark.Callable); ok {
			return true
		}
	}
	return false
}

// declare records d, the target name, in f.
func (w *workspace) declare(f *buildFile, name string, d *declaration) error {
	if err := checkName(name); err != nil {
		return err
	}
	if earlier, ok := f.decls[name]; ok {
		return fmt.Errorf("name %q is already used at %s", name, earlier.pos)
	}
	f.decls[name] = d
	f.names = append(f.names, name)
	return nil
}

// makeGlob makes the template of a glob(): its files.
func (w *workspace) makeGlob(
	c *evalContext, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (string, any, error) {
	var name string
	var include, exclude *starlark.List
	sel := fileset.DefaultSelection()
	params := append([]any{"name", &name}, selectionParams(sel, &include, &exclude, true)...)
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, params...); err != nil {
		return "", nil, err
	}
	if err := selectFiles(sel, fn.Name(), include, exclude, c.file.dir); err != nil {
		return "", nil, err
	}
	return name, &fileSet{label: label(c.file.pkg, name), dir: c.file.dir, sel: sel}, nil
}

// makePull makes the template of a pull(): the image to pull.
func (w *workspace) makePull(
	c *evalContext, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (string, any, error) {
	var name, image string
	tag := "latest"
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
		"name", &name,
		"image", &image,
		"tag?", &tag,
	); err != nil {
		return "", nil, err
	}
	ref, err := parsePullImage(image, tag)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", fn.Name(), err)
	}
	return name, &pullSpec{label: label(c.file.pkg, name), image: ref}, nil
}

// resolve finds the target that ref, the argument param of a call to fn, names, setting ref.label: a pull or
// layer if image is set, or else a glob or task, returning its declaration. It checks the arguments ref gives the
// target too. As a BUILD file runs, targets of its own must be declared earlier, so targets it names outside
// functions of ctx can't form a cycle.
func (w *workspace) resolve(
	c *evalContext, fn *starlark.Builtin, param string, ref *targetRef, image bool,
) (*declaration, error) {
	kinds, listed, scratchHint := []string{"glob", "task"}, "globs and tasks", ""
	if image {
		kinds, listed, scratchHint = []string{"pull", "layer"}, "targets", `"scratch" or `
	}
	want := kinds[0] + "() or " + kinds[1] + "()"
	from, name, err := w.lookup(c.file, ref.ref, ref.pkg)
	if err != nil {
		return nil, fmt.Errorf("%s: %s %q: %w", fn.Name(), param, ref.ref, err)
	}
	d, ok := from.decls[name]
	declaring := from == c.file && c.mode == declaring
	switch {
	case !ok && declaring && !image && strings.Contains(name, ":"):
		return nil, fmt.Errorf("%s: %s %q is not an earlier %s%s",
			fn.Name(), param, ref.ref, want, labelHint(name))
	case !ok && declaring:
		return nil, fmt.Errorf("%s: %s %q is not %san earlier %s%s",
			fn.Name(), param, ref.ref, scratchHint, want, describeNames(listed, from.namesOf(kinds...)))
	case !ok:
		return nil, fmt.Errorf("%s: %s %q: %w", fn.Name(), param, ref.ref, from.missing(name))
	case !slices.Contains(kinds, d.kind):
		return nil, fmt.Errorf("%s: %s %q is %s %s(), not a %s",
			fn.Name(), param, ref.ref, article(d.kind), d.kind, want)
	case !image && !d.hasOutput:
		return nil, fmt.Errorf("%s: %s %q is a task() without output; give it source and include",
			fn.Name(), param, ref.ref)
	}
	if err := d.checkArgs(ref); err != nil {
		return nil, fmt.Errorf("%s: %s: %w", fn.Name(), param, err)
	}
	ref.label = label(from.pkg, name)
	return d, nil
}

// newAdd implements add(source, dest, owner, group), checking the target it names as it is called.
func (w *workspace) newAdd(
	thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (starlark.Value, error) {
	var source starlark.Value
	var dest, owner, group string
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
		"source", &source,
		"dest", &dest,
		"owner?", &owner,
		"group?", &group,
	); err != nil {
		return nil, err
	}
	c := contextOf(thread)
	if c.file == nil {
		return nil, fmt.Errorf("%s: a .star file can't name a target as it is loaded; "+
			"call add() in a function that a BUILD file calls", fn.Name())
	}

	ref, err := targetRefOf(c, fn.Name(), "source", source)
	if err != nil {
		return nil, err
	}
	if _, err := w.resolve(c, fn, "source", &ref, false); err != nil {
		return nil, err
	}
	if err := graph.CheckPath("dest", dest); err != nil {
		return nil, fmt.Errorf("%s: %w", fn.Name(), err)
	}
	for _, id := range [][2]string{{"owner", owner}, {"group", group}} {
		if strings.Contains(id[1], ":") {
			return nil, fmt.Errorf(`%s: %s %q must not contain ":"; give owner and group apart`,
				fn.Name(), id[0], id[1])
		}
		if strings.Contains(id[1], "\x00") {
			return nil, fmt.Errorf("%s: %s %q must not contain a NUL byte", fn.Name(), id[0], id[1])
		}
	}
	return &addStep{target: ref, display: source.String(), dest: dest, owner: owner, group: group}, nil
}

// makeBase returns the base of a target made by a call to fn, as base, the argument, gives it.
func (w *workspace) makeBase(c *evalContext, fn *starlark.Builtin, base starlark.Value) (targetRef, error) {
	if base == starlark.String(scratch) {
		return targetRef{ref: scratch, label: scratch}, nil
	}
	ref, err := targetRefOf(c, fn.Name(), "base", base)
	if err != nil {
		return targetRef{}, err
	}
	_, err = w.resolve(c, fn, "base", &ref, true)
	return ref, err
}

// makeLayer makes the template of a layer().
func (w *workspace) makeLayer(
	c *evalContext, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (string, any, error) {
	var name string
	var base starlark.Value
	var steps, command *starlark.List
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
		"name", &name,
		"base", &base,
		"steps?", &steps,
		"command?", &command,
	); err != nil {
		return "", nil, err
	}

	s := &targetSpec{label: label(c.file.pkg, name)}
	var err error
	if s.steps, err = stepList(fn.Name(), steps); err != nil {
		return "", nil, err
	}
	if s.command, err = stringList(fn.Name(), "command", command); err != nil {
		return "", nil, err
	}
	if s.baseRef, err = w.makeBase(c, fn, base); err != nil {
		return "", nil, err
	}
	return name, s, nil
}

// makeTask makes the template of a task().
func (w *workspace) makeTask(
	c *evalContext, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (string, any, error) {
	var name, source string
	var base starlark.Value
	var steps, include, exclude *starlark.List
	sel := fileset.DefaultSelection()
	params := append([]any{
		"name", &name,
		"base", &base,
		"steps?", &steps,
		"source?", &source,
	}, selectionParams(sel, &include, &exclude, false)...)
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, params...); err != nil {
		return "", nil, err
	}

	s := &targetSpec{label: label(c.file.pkg, name), task: true, source: source}
	var err error
	if s.steps, err = stepList(fn.Name(), steps); err != nil {
		return "", nil, err
	}
	// A task makes files for add() only if it says where they are.
	switch {
	case source == "" && include != nil:
		return "", nil, fmt.Errorf("%s: include needs source, the directory it is relative to", fn.Name())
	case source != "" && include == nil:
		return "", nil, fmt.Errorf("%s: source needs include, the files to collect from it", fn.Name())
	case source != "":
		if !path.IsAbs(source) {
			return "", nil, fmt.Errorf("%s: source %q must be an absolute path", fn.Name(), source)
		}
		if strings.Contains(source, "\x00") {
			return "", nil, fmt.Errorf("%s: source %q must not contain a NUL byte", fn.Name(), source)
		}
		if err := selectFiles(sel, fn.Name(), include, exclude, source); err != nil {
			return "", nil, err
		}
		s.output = &fileSet{label: s.label, sel: sel}
	default:
		for _, kv := range kwargs {
			if param := string(kv[0].(starlark.String)); isSelectionParam(param) {
				return "", nil, fmt.Errorf("%s: %s needs source and include, the files to collect", fn.Name(), param)
			}
		}
	}
	if s.baseRef, err = w.makeBase(c, fn, base); err != nil {
		return "", nil, err
	}
	return name, s, nil
}

// makeIndex makes the template of an index(): the pulls and layers whose images it lists, each named for its
// platform. layer is one target, for each of platforms, or a dict from each platform to its own target.
func (w *workspace) makeIndex(
	c *evalContext, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (string, any, error) {
	var name string
	var layer starlark.Value
	var platformList *starlark.List
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
		"name", &name,
		"layer", &layer,
		"platforms?", &platformList,
	); err != nil {
		return "", nil, err
	}
	s := &indexSpec{}
	var where []string // says where each manifest's platform is given, for errors
	add := func(param string, v starlark.Value, platform, at string) error {
		ref, err := targetRefOf(c, fn.Name(), param, v)
		if err != nil {
			return err
		}
		if ref.exec || ref.platform != nil {
			return fmt.Errorf("%s: %s: an index's dep() can't give platform, since %s says what each "+
				"manifest is for", fn.Name(), param, at)
		}
		if _, err := w.resolve(c, fn, param, &ref, true); err != nil {
			return err
		}
		if ref.platform, err = parsePlatform(platform); err != nil {
			return fmt.Errorf("%s: %s: %w", fn.Name(), at, err)
		}
		for i, other := range s.manifests {
			if other.platform.String() == ref.platform.String() {
				return fmt.Errorf("%s: %s and %s are both %s; an index has one image for each platform",
					fn.Name(), where[i], at, ref.platform)
			}
		}
		s.manifests, where = append(s.manifests, ref), append(where, at)
		return nil
	}

	if dict, ok := layer.(*starlark.Dict); ok {
		if platformList != nil {
			return "", nil, fmt.Errorf("%s: give platforms, or a dict for layer, whose keys are the platforms, "+
				"not both", fn.Name())
		}
		if dict.Len() == 0 {
			return "", nil, fmt.Errorf("%s: layer must not be empty", fn.Name())
		}
		for _, item := range dict.Items() {
			platform, ok := starlark.AsString(item[0])
			if !ok {
				return "", nil, fmt.Errorf("%s: layer has a key of type %s, want a platform, such as \"linux/arm64\"",
					fn.Name(), item[0].Type())
			}
			param := fmt.Sprintf("layer[%s]", item[0])
			if err := add(param, item[1], platform, param+"'s key"); err != nil {
				return "", nil, err
			}
		}
		return name, s, nil
	}

	if platformList == nil {
		return "", nil, fmt.Errorf("%s: platforms is missing: the platforms to build layer for, such as "+
			"[\"linux/amd64\", \"linux/arm64\"]; or give layer as a dict from each platform to its target", fn.Name())
	}
	platforms, err := stringList(fn.Name(), "platforms", platformList)
	if err != nil {
		return "", nil, err
	}
	if len(platforms) == 0 {
		return "", nil, fmt.Errorf("%s: platforms must not be empty", fn.Name())
	}
	for i, platform := range platforms {
		if err := add("layer", layer, platform, fmt.Sprintf("platforms[%d]", i)); err != nil {
			return "", nil, err
		}
	}
	return name, s, nil
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

// buildTargets returns the labels and kinds of the targets to build that names, given on the command line,
// name, without repeats.
func (w *workspace) buildTargets(names []string) ([]string, []string, error) {
	var labels, kinds []string
	for _, name := range names {
		l, kind, err := w.target(name)
		if err != nil {
			return nil, nil, err
		}
		if kind == "glob" {
			return nil, nil, fmt.Errorf("%q is a glob(), whose files are read by the targets that add them; "+
				"name a pull(), layer(), task() or index() to build", name)
		}
		if !slices.Contains(labels, l) {
			labels, kinds = append(labels, l), append(kinds, kind)
		}
	}
	return labels, kinds, nil
}

// imageTarget returns the label and kind of the target whose image, or index of images, to send that name,
// given on the command line, names.
func (w *workspace) imageTarget(name string) (string, string, error) {
	l, kind, err := w.target(name)
	if err != nil {
		return "", "", err
	}
	switch kind {
	case "glob":
		return "", "", fmt.Errorf("%q is a glob(), whose files are read by the targets that add them, not an image; "+
			"name a pull(), layer() or index() to send", name)
	case "task":
		hint := ""
		if w.declaration(l).hasOutput {
			hint = `, or export the task's files with "vamos export"`
		}
		return "", "", fmt.Errorf("%q is a task(), whose image is discarded once it runs; "+
			"name a pull(), layer() or index() to send%s", name, hint)
	}
	return l, kind, nil
}

// filesTarget returns the label of the task whose files to export that name, given on the command line, names.
func (w *workspace) filesTarget(name string) (string, error) {
	l, kind, err := w.target(name)
	if err != nil {
		return "", err
	}
	switch kind {
	case "task":
		if !w.declaration(l).hasOutput {
			return "", fmt.Errorf("%q is a task() without source, so it has no files to export; "+
				"%q runs it", name, "vamos build "+name)
		}
		return l, nil
	case "glob":
		return "", fmt.Errorf("%q is a glob(), whose files are already in the project; export writes a task's files",
			name)
	case "index":
		return "", fmt.Errorf("%q is an index(), not a task(); export writes a task's files", name)
	}
	return "", fmt.Errorf(`%q is %s %s(), not a task(); export writes a task's files, and "vamos save" writes an image`,
		name, article(kind), kind)
}

// declaration returns the declaration of the target with label l, whose package is loaded.
func (w *workspace) declaration(l string) *declaration {
	rest := strings.TrimPrefix(l, "//")
	i := strings.LastIndexByte(rest, ':')
	return w.packages[rest[:i]].decls[rest[i+1:]]
}

// readGlobs reads the files of each glob in g, whose files are globs, by node, with store, which returns their tree,
// setting the trees of g's Source nodes. A glob's files are read once, before anything is built, so that every
// target that adds them gets the same files, as the build cache stored them, even if they change during the build.
// Each glob is a span of ctx's trace, which those that add its files point back to.
func (w *workspace) readGlobs(
	ctx context.Context, g *graph.Graph, globs map[graph.Ref]*fileSet,
	store func(dir string, sel *fileset.Selection) (cache.Blob, fileset.Stats, error),
) error {
	for _, ref := range slices.Sorted(maps.Keys(globs)) {
		set := globs[ref]
		_, span := tracing.Start(ctx, g.Node(ref).Desc,
			"dir", set.dir,
			"include", set.sel.Include,
			"exclude", set.sel.Exclude)
		tree, stats, err := store(set.dir, set.sel)
		if err != nil {
			err = fmt.Errorf("%s: glob: %w", w.declaration(set.label).pos, err)
			if set.configuring != "" {
				err = fmt.Errorf("%w (configuring %s)", err, set.configuring)
			}
			span.EndWith(err)
			return err
		}
		span.Set(stats.Args()...)
		span.Set("tree", tree.Digest.String(), "tree bytes", tree.Size)
		span.Produce(graph.FlowKey(ref))
		span.End()
		g.Node(ref).Source.Tree = tree
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

// article returns the indefinite article for a builtin's name: "an" for index, "a" for the others.
func article(kind string) string {
	if kind == "index" {
		return "an"
	}
	return "a"
}

// describeNames lists the targets of a kind, such as "globs", declared so far, for an error
// about an unknown name.
func describeNames(kind string, names []string) string {
	if len(names) == 0 {
		return "; there are none before this"
	}
	return "; earlier " + kind + " are " + fileset.QuoteNames(names)
}

// describeEvalError reports e at the innermost position within a BUILD file, in the
// one-line "file:line:col: message" form of compiler errors.
func describeEvalError(e *starlark.EvalError) string {
	var at *posError
	if errors.As(e, &at) {
		return at.Error()
	}
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
			return nil, fmt.Errorf("%s: %s[%d] is %s, want string%s",
				fnName, param, i, list.Index(i).Type(), functionHint(list.Index(i)))
		}
		if strings.Contains(s, "\x00") {
			return nil, fmt.Errorf("%s: %s[%d] must not contain a NUL byte", fnName, param, i)
		}
		out = append(out, s)
	}
	return out, nil
}
