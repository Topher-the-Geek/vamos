package load

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Topher-the-Geek/vamos/internal/graph"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// step is one action of a layer or task, made by a call to add(),
// run(), env(), build_env() or workdir() in a BUILD file.
// A target applies its steps in the order listed.
// Its String method describes it as the call that made it, for messages.
type step interface {
	starlark.Value
	// source returns the file set the step copies into the container, or nil if it has none.
	source() *fileSet
	// lower returns the step as its target's node holds it, but for From, the node of its source.
	lower() graph.Step
}

// stepBase gives a step the Starlark methods that all share, and no source.
// Each step type then implements String and lower, as step describes.
type stepBase struct{}

func (stepBase) Type() string          { return "step" }
func (stepBase) Freeze()               {} // steps are immutable
func (stepBase) Truth() starlark.Bool  { return true }
func (stepBase) Hash() (uint32, error) { return 0, errors.New("unhashable type: step") }
func (stepBase) source() *fileSet      { return nil }

// addStep copies files from a package or a task's output into the container.
type addStep struct {
	stepBase
	target  targetRef // the glob or task whose files to add
	display string    // target, as given, for messages
	set     *fileSet  // of an instance, configured: the files, placed beneath dest at their relative paths
	dest    string    // as given: absolute once its variables are expanded
	owner   string    // user or ID that owns the files, as named in the image; root if empty
	group   string    // group or ID; the owner's group if empty, or root's if owner is too
}

func (s *addStep) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "add(%s, %s", s.display, starlark.String(s.dest))
	if s.owner != "" {
		fmt.Fprintf(&b, ", owner=%s", starlark.String(s.owner))
	}
	if s.group != "" {
		fmt.Fprintf(&b, ", group=%s", starlark.String(s.group))
	}
	b.WriteString(")")
	return b.String()
}

func (s *addStep) source() *fileSet { return s.set }

func (s *addStep) lower() graph.Step {
	return graph.Step{Op: graph.OpAdd, Display: s.String(), Dest: s.dest, Owner: s.owner, Group: s.group}
}

// runStep runs a command in the container, given its secrets while it runs.
type runStep struct {
	stepBase
	argv    []string
	secrets []graph.Secret
}

func (s *runStep) String() string {
	if len(s.secrets) == 0 {
		return fmt.Sprintf("run(%s)", reprStrings(s.argv))
	}
	secrets := make([]string, len(s.secrets))
	for i, secret := range s.secrets {
		secrets[i] = (&secretValue{secret}).String()
	}
	return fmt.Sprintf("run(%s, secrets=[%s])", reprStrings(s.argv), strings.Join(secrets, ", "))
}

func (s *runStep) lower() graph.Step {
	return graph.Step{Op: graph.OpRun, Display: s.String(), Argv: s.argv, Secrets: s.secrets}
}

// secretValue is a secret that a run() step is given, made by secret(), which names it and says how the step gets
// it; the command line gives its value.
type secretValue struct {
	secret graph.Secret
}

func (s *secretValue) String() string {
	if s.secret.Env != "" {
		return fmt.Sprintf("secret(%s, env=%s)", starlark.String(s.secret.ID), starlark.String(s.secret.Env))
	}
	return fmt.Sprintf("secret(%s, file=%s)", starlark.String(s.secret.ID), starlark.String(s.secret.File))
}
func (s *secretValue) Type() string          { return "secret" }
func (s *secretValue) Freeze()               {} // immutable
func (s *secretValue) Truth() starlark.Bool  { return true }
func (s *secretValue) Hash() (uint32, error) { return 0, errors.New("unhashable type: secret") }

// envStep sets environment variables for later runs.
// Those set by env() are kept in the image's config too, for the image's own command and the images built on it;
// those set by build_env() are not.
// Values refer to variables as $NAME or ${NAME}, which are expanded as the target reaches them.
type envStep struct {
	stepBase
	build bool     // made by build_env()
	vars  []string // NAME=value, in the order given
}

// op names the builtin that made s.
func (s *envStep) op() string {
	if s.build {
		return graph.OpBuildEnv
	}
	return graph.OpEnv
}

func (s *envStep) String() string { return s.op() + "(" + reprVars(s.vars) + ")" }

func (s *envStep) lower() graph.Step {
	return graph.Step{Op: s.op(), Display: s.String(), Vars: s.vars}
}

// workdirStep sets the working directory in the image's config, for later runs and the image's own command,
// and in the images built on it.
// Like a Dockerfile's WORKDIR, it makes the directory.
type workdirStep struct {
	stepBase
	dir string // as given: absolute once its variables are expanded
}

func (s *workdirStep) String() string {
	return fmt.Sprintf("workdir(%s)", starlark.String(s.dir))
}

func (s *workdirStep) lower() graph.Step {
	return graph.Step{Op: graph.OpWorkdir, Display: s.String(), Dir: s.dir}
}

// stepBuiltins are the BUILD file's builtins that make steps without naming other targets.
var stepBuiltins = starlark.StringDict{
	"run":       starlark.NewBuiltin("run", newRun),
	"secret":    starlark.NewBuiltin("secret", newSecret),
	"env":       starlark.NewBuiltin("env", newEnv),
	"build_env": starlark.NewBuiltin("build_env", newEnv),
	"workdir":   starlark.NewBuiltin("workdir", newWorkdir),
}

// newRun implements run(argv, secrets=[]).
func newRun(
	_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (starlark.Value, error) {
	var list, secretList *starlark.List
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "argv", &list, "secrets?", &secretList); err != nil {
		return nil, err
	}
	argv, err := stringList(fn.Name(), "argv", list)
	if err != nil {
		return nil, err
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("%s: argv must not be empty", fn.Name())
	}
	var secrets []graph.Secret
	if secretList != nil {
		for i := 0; i < secretList.Len(); i++ {
			s, ok := secretList.Index(i).(*secretValue)
			if !ok {
				return nil, fmt.Errorf(`%s: secrets[%d] is %s, want a secret, such as secret("token", env="TOKEN")`,
					fn.Name(), i, secretList.Index(i).Type())
			}
			secrets = append(secrets, s.secret)
		}
	}
	if err := graph.CheckSecrets(secrets); err != nil {
		return nil, fmt.Errorf("%s: %w", fn.Name(), err)
	}
	return &runStep{argv: argv, secrets: secrets}, nil
}

// newSecret implements secret(id, env="", file=""): the secret id, which --secret gives a value,
// given to a run() step as the variable env or the file file.
func newSecret(
	_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (starlark.Value, error) {
	var s graph.Secret
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "id", &s.ID, "env?", &s.Env, "file?", &s.File); err != nil {
		return nil, err
	}
	if err := graph.CheckSecret(s); err != nil {
		return nil, fmt.Errorf("%s: %w", fn.Name(), err)
	}
	return &secretValue{s}, nil
}

// newEnv implements env(NAME="value", ...)
// and build_env(NAME="value", ...).
// Variables are given by name, so that a dict of them can be passed as **vars.
// Values may refer to variables, as ExpandVars describes.
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
		if _, err := graph.ExpandVars(value, func(string) string { return "" }); err != nil {
			return nil, fmt.Errorf("%s: %s=%q: %w", fn.Name(), name, value, err)
		}
		seen[name] = true
		vars = append(vars, name+"="+value)
	}
	return &envStep{build: fn.Name() == "build_env", vars: vars}, nil
}

// newWorkdir implements workdir(path).
func newWorkdir(
	_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (starlark.Value, error) {
	var dir string
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs, "path", &dir); err != nil {
		return nil, err
	}
	if err := graph.CheckPath("path", dir); err != nil {
		return nil, fmt.Errorf("%s: %w", fn.Name(), err)
	}
	return &workdirStep{dir: dir}, nil
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
			return nil, fmt.Errorf("%s: steps[%d] is %s, want a step, such as add() or run()%s",
				fnName, i, list.Index(i).Type(), functionHint(list.Index(i)))
		}
		out = append(out, in)
	}
	return out, nil
}

// functionHint explains, if v is a function, that a function of ctx must give a whole attribute, not an element of one.
func functionHint(v starlark.Value) string {
	if _, ok := v.(starlark.Callable); ok {
		return "; a function of ctx must give the whole list"
	}
	return ""
}

// reprStrings formats ss as a Starlark list.
func reprStrings(ss []string) string {
	items := make([]starlark.Value, len(ss))
	for i, s := range ss {
		items[i] = starlark.String(s)
	}
	return starlark.NewList(items).String()
}

// reprVars formats NAME=value pairs as Starlark keyword arguments,
// and those whose names aren't identifiers as a dict passed as **vars.
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
