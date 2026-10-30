package features

import (
	"testing"
)

// newEnvMistakeWorld starts a scenario of the env feature whose env() or build_env() is given wrongly,
// which loading catches before anything runs, so that it needs no buildah.
// env() and build_env() set string-valued variables, given by name, for later steps in the target,
// and their values can refer to the variables set before them.
// Its project pulls busybox.
func newEnvMistakeWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// TestEnv_EachCallSetsAVariable specifies that each call of env() or build_env() needs at least one variable.
func TestEnv_EachCallSetsAVariable(t *testing.T) {
	for _, example := range []struct {
		name  string
		step  string
		error string
	}{
		{
			name:  "An env() without variables fails",
			step:  `env()`,
			error: "/project/BUILD:2:45: env: no variables given",
		},
		{
			name:  "A build_env() without variables fails",
			step:  `build_env()`,
			error: "/project/BUILD:2:51: build_env: no variables given",
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newEnvMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[` + example.step + `])`)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestEnv_VariablesAreNamedStrings specifies that env() and build_env() set string-valued variables,
// each given by a name that is nonempty and holds no "=", and given once in a call.
func TestEnv_VariablesAreNamedStrings(t *testing.T) {
	for _, example := range []struct {
		name  string
		step  string
		error string
	}{
		{
			name:  "Variables given as a dict rather than by name fail, saying how to give them",
			step:  `env({"LANG": "C"})`,
			error: `/project/BUILD:2:45: env: give variables by name, as NAME="value", or a dict of them as **vars`,
		},
		{
			name:  "A value that isn't a string fails",
			step:  `env(PORT=8080)`,
			error: "/project/BUILD:2:45: env: PORT is int, want string",
		},
		{
			name:  "An empty name fails",
			step:  `env(**{"": "x"})`,
			error: "/project/BUILD:2:45: env: a variable's name is empty",
		},
		{
			name:  "A name holding = fails",
			step:  `build_env(**{"A=B": "x"})`,
			error: `/project/BUILD:2:51: build_env: variable name "A=B" must not contain "=" or a NUL byte`,
		},
		{
			name:  "A name given twice in a call fails",
			step:  `env(LANG="C", **{"LANG": "C.UTF-8"})`,
			error: `/project/BUILD:2:45: env: variable "LANG" is given twice`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newEnvMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[` + example.step + `])`)
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}

// TestEnv_DollarIsFollowedByANameOrBraces specifies that values, and the paths of add() and workdir(),
// expand $NAME and ${NAME}, and that any other $ syntax, including ${NAME:-default}, $5, or a trailing $,
// is an error when the BUILD file loads; $$ gives a literal dollar sign.
func TestEnv_DollarIsFollowedByANameOrBraces(t *testing.T) {
	for _, example := range []struct {
		name  string
		step  string
		error string
	}{
		{
			name: "A $ before a digit fails, saying to write $$",
			step: `env(PRICE="$5")`,
			error: `/project/BUILD:3:45: env: PRICE="$5": "$" must be followed by a variable's name or "{"; ` +
				`write "$$" for "$"`,
		},
		{
			name: "A trailing $ fails, saying to write $$",
			step: `env(PRICE="5$")`,
			error: `/project/BUILD:3:45: env: PRICE="5$": "$" must be followed by a variable's name or "{"; ` +
				`write "$$" for "$"`,
		},
		{
			name: "A default fails, saying to write $$",
			step: `build_env(A="${B:-c}")`,
			error: `/project/BUILD:3:51: build_env: A="${B:-c}": "${" must be followed by a variable's name and "}"; ` +
				`write "$$" for "$"`,
		},
		{
			name: "A default in an add() destination fails",
			step: `add("code", "${D:-/app}")`,
			error: `/project/BUILD:3:45: add: dest "${D:-/app}": "${" must be followed by a variable's name and "}"; ` +
				`write "$$" for "$"`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newEnvMistakeWorld(t)
			w.GivenTheBuildFileDeclares(`glob(name="code", include=["app.sh"])`)
			w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[` + example.step + `])`)
			w.GivenTheProjectHolds(files{"app.sh": "echo hi"})
			w.WhenISave("app")
			w.ThenItFailsWith(example.error)
			w.ThenNoTargetRan()
			w.ThenNothingIsSaved()
		})
	}
}
