package internal

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"slices"
	"strings"

	"go.podman.io/buildah"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// step is one action of a layer or task, made by a call to add(), run(), env(), build_env()
// or workdir() in a BUILD file. A target applies its steps in the order listed. Its String
// method describes it as the call that made it, for messages.
type step interface {
	starlark.Value
	// source returns the file set the step copies into the container, or nil if it has none.
	source() *fileSet
	// entry returns the step's part of its target's action, but for the tree of its files.
	entry() actionStep
	// apply performs the step on the target being built. files is the tree of its source,
	// stored in the cache, or nil if it has none.
	apply(st *targetState, files *blob) error
}

// targetState is a target being built, which its steps act on in turn.
type targetState struct {
	*targetRunner
	builder    *buildah.Builder
	mountPoint string // where the container's files are on the host, mounted while the target runs
	log        *logRecorder
	buildEnv   map[string]string // set by build_env(), and not since by env()
}

// stepBase gives a step the Starlark methods that all share, and no source.
// Each step type then implements String, entry and apply, as step describes.
type stepBase struct{}

func (stepBase) Type() string          { return "step" }
func (stepBase) Freeze()               {} // steps are immutable
func (stepBase) Truth() starlark.Bool  { return true }
func (stepBase) Hash() (uint32, error) { return 0, errors.New("unhashable type: step") }
func (stepBase) source() *fileSet      { return nil }

// addStep copies files from a package or a task's output into the container.
type addStep struct {
	stepBase
	ref  string   // names set, as given
	set  *fileSet // placed beneath dest at their relative paths
	dest string   // as given: absolute once its variables are expanded
}

func (s *addStep) String() string {
	return fmt.Sprintf("add(%s, %s)", starlark.String(s.ref), starlark.String(s.dest))
}

func (s *addStep) source() *fileSet  { return s.set }
func (s *addStep) entry() actionStep { return actionStep{Op: "add", Dest: s.dest} }

func (s *addStep) apply(st *targetState, files *blob) error {
	if files == nil {
		return errors.New("its files are missing from the target's action") // inputs always stores them
	}
	dest, err := st.expandPath("dest", s.dest)
	if err != nil {
		return err
	}
	return st.addSource(s.set, *files, dest)
}

// runStep runs a command in the container.
type runStep struct {
	stepBase
	argv []string
}

func (s *runStep) String() string    { return fmt.Sprintf("run(%s)", reprStrings(s.argv)) }
func (s *runStep) entry() actionStep { return actionStep{Op: "run", Argv: s.argv} }

func (s *runStep) apply(st *targetState, _ *blob) error {
	// Variables in the image's config are passed too; these override them.
	var env []string
	for _, name := range slices.Sorted(maps.Keys(st.buildEnv)) {
		env = append(env, name+"="+st.buildEnv[name])
	}
	return st.builder.Run(s.argv, buildah.RunOptions{
		Isolation: st.isolation,
		Terminal:  buildah.WithoutTerminal,
		Env:       env,
		Stdout:    st.log.writer(streamStdout, st.stdout),
		Stderr:    st.log.writer(streamStderr, st.stderr),
	})
}

// envStep sets environment variables for later runs. Those set by env() are kept in the image's
// config too, for the image's own command and the images built on it; those set by build_env() are not.
// Values refer to variables as $NAME or ${NAME}, which are expanded as the target reaches them.
type envStep struct {
	stepBase
	build bool     // made by build_env()
	vars  []string // NAME=value, in the order given
}

// op names the builtin that made s.
func (s *envStep) op() string {
	if s.build {
		return "build_env"
	}
	return "env"
}

func (s *envStep) String() string { return s.op() + "(" + reprVars(s.vars) + ")" }

// entry sorts the variables, which have different names, so that their order doesn't change the key.
func (s *envStep) entry() actionStep {
	return actionStep{Op: s.op(), Vars: slices.Sorted(slices.Values(s.vars))}
}

func (s *envStep) apply(st *targetState, _ *blob) error {
	// As in a Dockerfile's ENV, every value refers to the variables as they were before the call.
	// They are set in the order entry gives, so that targets with the same key make the same image.
	lookup := st.lookup()
	for _, v := range slices.Sorted(slices.Values(s.vars)) {
		name, value, _ := strings.Cut(v, "=")
		value, err := expandVars(value, lookup)
		if err != nil {
			return err // newEnv checks the syntax, so this can't happen
		}
		if s.build {
			st.buildEnv[name] = value
		} else {
			st.builder.SetEnv(name, value)
			delete(st.buildEnv, name) // which would otherwise override the newer value
		}
	}
	return nil
}

// workdirStep sets the working directory in the image's config, for later runs and the image's
// own command, and in the images built on it. Like a Dockerfile's WORKDIR, it makes the directory.
type workdirStep struct {
	stepBase
	dir string // as given: absolute once its variables are expanded
}

func (s *workdirStep) String() string {
	return fmt.Sprintf("workdir(%s)", starlark.String(s.dir))
}

func (s *workdirStep) entry() actionStep {
	return actionStep{Op: "workdir", Dir: s.dir}
}

func (s *workdirStep) apply(st *targetState, _ *blob) error {
	dir, err := st.expandPath("path", s.dir)
	if err != nil {
		return err
	}
	// As a Dockerfile's WORKDIR does, a new directory belongs to the image's user, so later runs can write
	// to it. buildah works within the container's root, so a link in the image can't lead outside it.
	mode := os.FileMode(0o755)
	if err := st.builder.EnsureContainerPathAs(dir, st.builder.User(), &mode); err != nil {
		return err
	}
	st.builder.SetWorkDir(dir)
	return nil
}

// lookup returns a function that gives the value of a variable as a run in the target would see it now:
// set by build_env(), or else in the image's config. An unset variable is empty.
func (st *targetState) lookup() func(name string) string {
	env := map[string]string{}
	for _, v := range st.builder.Env() {
		name, value, _ := strings.Cut(v, "=")
		env[name] = value
	}
	maps.Copy(env, st.buildEnv)
	return func(name string) string { return env[name] }
}

// expandPath expands the variables in p, a step's argument param, which checkPath has checked,
// and returns it cleaned. It must then be absolute.
func (st *targetState) expandPath(param, p string) (string, error) {
	expanded, err := expandVars(p, st.lookup())
	if err != nil {
		return "", err // checkPath checks the syntax, so this can't happen
	}
	if !path.IsAbs(expanded) {
		return "", fmt.Errorf("%s %q is %q, which must be an absolute path", param, p, expanded)
	}
	return path.Clean(expanded), nil
}

// stepBuiltins are the BUILD file's builtins that make steps without naming other targets.
var stepBuiltins = starlark.StringDict{
	"run":       starlark.NewBuiltin("run", newRun),
	"env":       starlark.NewBuiltin("env", newEnv),
	"build_env": starlark.NewBuiltin("build_env", newEnv),
	"workdir":   starlark.NewBuiltin("workdir", newWorkdir),
}

// newRun implements run(argv).
func newRun(
	_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (starlark.Value, error) {
	var list *starlark.List
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "argv", &list); err != nil {
		return nil, err
	}
	argv, err := stringList(fn.Name(), "argv", list)
	if err != nil {
		return nil, err
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("%s: argv must not be empty", fn.Name())
	}
	return &runStep{argv: argv}, nil
}

// newEnv implements env(NAME="value", ...) and build_env(NAME="value", ...). Variables are given
// by name, so that a dict of them can be passed as **vars. Values may refer to variables, as expandVars
// describes.
func newEnv(
	_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (starlark.Value, error) {
	if len(args) > 0 {
		return nil, fmt.Errorf(`%s: give variables by name, as NAME="value", or a dict of them as **vars`, fn.Name())
	}
	if len(kwargs) == 0 {
		return nil, fmt.Errorf("%s: no variables given", fn.Name())
	}
	vars := make([]string, 0, len(kwargs))
	seen := map[string]bool{}
	for _, kv := range kwargs {
		name := string(kv[0].(starlark.String)) // Starlark requires keywords to be strings
		value, ok := starlark.AsString(kv[1])
		switch {
		case !ok:
			return nil, fmt.Errorf("%s: %s is %s, want string", fn.Name(), name, kv[1].Type())
		case name == "":
			return nil, fmt.Errorf("%s: a variable's name is empty", fn.Name())
		case strings.ContainsAny(name, "=\x00"):
			return nil, fmt.Errorf(`%s: variable name %q must not contain "=" or a NUL byte`, fn.Name(), name)
		case strings.Contains(value, "\x00"):
			return nil, fmt.Errorf("%s: %s's value must not contain a NUL byte", fn.Name(), name)
		case seen[name]:
			// Starlark allows this when one comes from **vars.
			return nil, fmt.Errorf("%s: variable %q is given twice", fn.Name(), name)
		}
		// Check the syntax of references now, rather than when the target runs.
		if _, err := expandVars(value, func(string) string { return "" }); err != nil {
			return nil, fmt.Errorf("%s: %s=%q: %w", fn.Name(), name, value, err)
		}
		seen[name] = true
		vars = append(vars, name+"="+value)
	}
	return &envStep{build: fn.Name() == "build_env", vars: vars}, nil
}

// expandVars replaces $NAME and ${NAME} in s with lookup(NAME), and $$ with $. Any other $ is an error,
// so that a Dockerfile's ${NAME:-default}, which isn't supported, can't silently become empty.
func expandVars(s string, lookup func(string) string) (string, error) {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '$')
		if i < 0 {
			return b.String() + s, nil
		}
		b.WriteString(s[:i])
		s = s[i+1:]
		var name string
		switch {
		case strings.HasPrefix(s, "$"):
			b.WriteByte('$')
			s = s[1:]
			continue
		case strings.HasPrefix(s, "{"):
			end := strings.IndexByte(s, '}')
			if end < 0 || !isVarName(s[1:end]) {
				return "", errors.New(`"${" must be followed by a variable's name and "}"; write "$$" for "$"`)
			}
			name, s = s[1:end], s[end+1:]
		default:
			n := 0
			for n < len(s) && isVarName(s[:n+1]) {
				n++
			}
			if n == 0 {
				return "", errors.New(`"$" must be followed by a variable's name or "{"; write "$$" for "$"`)
			}
			name, s = s[:n], s[n:]
		}
		b.WriteString(lookup(name))
	}
}

// isVarName reports whether name is a variable's name that expandVars recognizes: a letter or
// underscore, then letters, digits and underscores.
func isVarName(name string) bool {
	for i, r := range name {
		letter := r == '_' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z'
		if !letter && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return name != ""
}

// newWorkdir implements workdir(path).
func newWorkdir(
	_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (starlark.Value, error) {
	var dir string
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "path", &dir); err != nil {
		return nil, err
	}
	if err := checkPath("path", dir); err != nil {
		return nil, fmt.Errorf("%s: %w", fn.Name(), err)
	}
	return &workdirStep{dir: dir}, nil
}

// checkPath checks p, a step's argument param, which may refer to variables: that they are
// written as expandVars expects, and that p is absolute, unless it starts with one, which is only
// known once it is expanded.
func checkPath(param, p string) error {
	if _, err := expandVars(p, func(string) string { return "" }); err != nil {
		return fmt.Errorf("%s %q: %w", param, p, err)
	}
	if !path.IsAbs(p) && !strings.HasPrefix(p, "$") {
		return fmt.Errorf("%s %q must be an absolute path", param, p)
	}
	return nil
}

// stepList converts an optional steps argument of a call to fnName.
func stepList(fnName string, list *starlark.List) ([]step, error) {
	if list == nil {
		return nil, nil
	}
	out := make([]step, 0, list.Len())
	for i := 0; i < list.Len(); i++ {
		in, ok := list.Index(i).(step)
		if !ok {
			return nil, fmt.Errorf("%s: steps[%d] is %s, want a step, such as add() or run()",
				fnName, i, list.Index(i).Type())
		}
		out = append(out, in)
	}
	return out, nil
}

// reprStrings formats ss as a Starlark list.
func reprStrings(ss []string) string {
	items := make([]starlark.Value, len(ss))
	for i, s := range ss {
		items[i] = starlark.String(s)
	}
	return starlark.NewList(items).String()
}

// reprVars formats NAME=value pairs as Starlark keyword arguments, and those whose names aren't
// identifiers as a dict passed as **vars.
func reprVars(vars []string) string {
	var args, others []string
	for _, v := range vars {
		name, value, _ := strings.Cut(v, "=")
		if isIdentifier(name) {
			args = append(args, name+"="+starlark.String(value).String())
		} else {
			others = append(others, starlark.String(name).String()+": "+starlark.String(value).String())
		}
	}
	if len(others) > 0 {
		args = append(args, "**{"+strings.Join(others, ", ")+"}")
	}
	return strings.Join(args, ", ")
}

// isIdentifier reports whether name can be a Starlark keyword argument as it is.
func isIdentifier(name string) bool {
	expr, err := syntax.ParseExpr("", name, 0)
	ident, ok := expr.(*syntax.Ident)
	return err == nil && ok && ident.Name == name
}
