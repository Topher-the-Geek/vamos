package features

import (
	"testing"
)

// newEnvWorld starts a scenario of the env feature that builds, from its background, which it skips where buildah
// can't. Its project pulls busybox.
func newEnvWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

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

// TestEnv_ValuesExpandTheVariablesBeforeTheCall specifies that values expand $NAME and ${NAME} using the environment
// before that call, that unset variables expand to an empty string, and that $$ produces a literal dollar sign;
// add() destinations and workdir() paths expand them too.
func TestEnv_ValuesExpandTheVariablesBeforeTheCall(t *testing.T) {
	t.Run("Each value sees the variables as they were before its call", func(t *testing.T) {
		w := newEnvWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(
			    name="app",
			    base="busybox",
			    steps=[
			        env(MODE="image"),
			        build_env(MODE="build", TOOL="cc"),
			        run(["sh", "-c", "echo $MODE $TOOL > /during.txt"]),
			        env(MODE="final"),
			        env(PATH="/app/bin:$PATH", MODE="${MODE}2", LABEL="$MODE-$TOOL-$UNSET-$$5"),
			        run(["sh", "-c", "echo $PATH $MODE $LABEL > /expanded.txt"]),
			    ],
			)
		`)
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHoldsAtLeast(files{
			"/during.txt": "build cc\n",
			"/expanded.txt": "/app/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin " +
				"final2 final-cc--$5\n",
		})
		w.ThenTheImageEnvironmentSets(environment{"MODE": "final2", "LABEL": "final-cc--$5"})
		w.ThenTheImageEnvironmentDoesNotSet("TOOL")
	})

	t.Run("Variables given as a dict, by names it can't give as arguments, are set too", func(t *testing.T) {
		w := newEnvWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(
			    name="app",
			    base="busybox",
			    steps=[
			        env(GREETING="hello", **{"NAME-WITH-DASHES": "value"}),
			        run(["sh", "-c", "printenv NAME-WITH-DASHES > /dashes.txt"]),
			    ],
			)
		`)
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHoldsAtLeast(files{"/dashes.txt": "value\n"})
		w.ThenTheImageEnvironmentSets(environment{"GREETING": "hello", "NAME-WITH-DASHES": "value"})
	})

	t.Run("An add() destination and a workdir() path expand variables", func(t *testing.T) {
		w := newEnvWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="conf", include=["app.conf"])
			task(
			    name="generated",
			    base="busybox",
			    steps=[run(["sh", "-c", "mkdir /out && echo generated > /out/app.conf"])],
			    source="/out",
			    include=["app.conf"],
			)
			layer(
			    name="app",
			    base="busybox",
			    steps=[
			        run(["test", "!", "-e", "/etc/app"]),  # so that what is there, add() put there
			        build_env(CONF="/etc", ROOT="/srv/"),
			        add("conf", "${CONF}/app"),
			        add("generated", "${CONF}/app"),
			        workdir("$ROOT/app/"),
			        run(["sh", "-c", "pwd > where.txt"]),
			    ],
			)
		`)
		w.GivenTheProjectHolds(files{"app.conf": "from the project\n"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHolds(under("/etc/app"), files{"/etc/app/app.conf": "generated\n"})
		w.ThenTheImageHolds(under("/srv"), files{"/srv/app/where.txt": "/srv/app\n"})
		w.ThenTheImageWorkingDirectoryIs("/srv/app")
	})
}
