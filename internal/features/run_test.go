package features

import (
	"encoding/base64"
	"fmt"
	"os"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
)

// newRunWorld starts a scenario of the run feature, from its background.
// run() runs a nonempty list of command arguments in the container's current working directory.
// Its project pulls busybox.
func newRunWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// TestRun_CommandRunsInTheContainer specifies that run() runs a nonempty list of command arguments
// in the container's current working directory, whose changes the layer keeps, and whose output reaches the user.
func TestRun_CommandRunsInTheContainer(t *testing.T) {
	t.Run("A command's files stay in the image, and its output and errors reach the user", func(t *testing.T) {
		w := newRunWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="script", include=["build.sh"])
			layer(name="app", base="busybox", steps=[add("script", "/src"), run(["sh", "/src/build.sh"])])
		`)
		w.GivenTheProjectHolds(files{"build.sh": "echo compiling\necho 'built!' > /built.txt\necho 'a warning' >&2\n"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheOutputIncludes("compiling")
		w.ThenTheErrorOutputIncludes("a warning")
		w.ThenTheImageHoldsAtLeast(files{"/built.txt": "built!\n"})
	})
}

// TestRun_NoShellIsImplicit specifies that run() runs its arguments as they are, with no shell:
// use a shell explicitly for redirection, pipes, or shell variable expansion.
func TestRun_NoShellIsImplicit(t *testing.T) {
	t.Run("Arguments reach the command as they are written", func(t *testing.T) {
		w := newRunWorld(t)
		w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[run(["echo", "$HOME", "|", "*"])])`)
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheOutputIncludes("$HOME | *\n")
	})

	t.Run("A shell, run explicitly, expands variables and redirects", func(t *testing.T) {
		w := newRunWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(name="app", base="busybox", steps=[run(["sh", "-c", "echo $HOME > /home.txt"])])
		`)
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHoldsAtLeast(files{"/home.txt": "/root\n"})
	})
}

// TestRun_CommandMustSucceed specifies that run()'s command must exit successfully.
func TestRun_CommandMustSucceed(t *testing.T) {
	for _, command := range []string{`["true"]`, `["echo", "hi"]`, `["sh", "-c", "exit 0"]`} {
		t.Run("A command that exits successfully succeeds: "+command, func(t *testing.T) {
			w := newRunWorld(t)
			w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[run(` + command + `)])`)
			w.WhenISave("app")
			w.ThenItSucceeds()
		})
	}

	for _, example := range []struct {
		name    string
		command string // run()'s argument, as Starlark
		errors  []string
	}{
		{
			name:    "A command that exits with an error fails",
			command: `["false"]`,
			errors:  []string{`layer "app": run(["false"]) failed`, "exit status 1"},
		},
		{
			name:    "A command's exit status is reported",
			command: `["sh", "-c", "exit 3"]`,
			errors:  []string{`layer "app": run(["sh", "-c", "exit 3"]) failed`, "exit status 3"},
		},
		{
			name:    "A command the base doesn't have fails",
			command: `["no-such-compiler", "main.c"]`,
			errors:  []string{`layer "app": run(["no-such-compiler", "main.c"]) failed`, "no-such-compiler"},
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newRunWorld(t)
			w.GivenTheBuildFileDeclares(`layer(name="app", base="busybox", steps=[run(` + example.command + `)])`)
			w.WhenISave("app")
			w.ThenItFailsWith(example.errors...)
			w.ThenNothingIsSaved()
			w.ThenNoImageIsLeftBehind()
		})
	}

	t.Run("A failing command's errors reach the user", func(t *testing.T) {
		w := newRunWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="script", include=["app.sh"])
			layer(name="app", base="busybox", steps=[add("script", "/src"), run(["sh", "-n", "/src/app.sh"])])
		`)
		w.GivenTheProjectHolds(files{"app.sh": "if true; then\n  echo unfinished\n"})
		w.WhenISave("app")
		w.ThenItFailsWith(`layer "app": run(["sh", "-n", "/src/app.sh"]) failed`, "exit status 2")
		w.ThenTheErrorOutputIncludes("syntax error")
		w.ThenNothingIsSaved()
		w.ThenNoImageIsLeftBehind()
	})
}

// TestRun_NoMountsFromTheHost specifies that a step gets none of the host's mounts.conf mounts,
// such as subscription secrets at /run/secrets. Only on a host whose mounts.conf names /run/secrets,
// as containers-common's does, does the scenario show it; TestBuilderOptionsNoHostMounts checks it everywhere.
func TestRun_NoMountsFromTheHost(t *testing.T) {
	t.Run("A step sees no /run/secrets of the host's, nor one a secret before it was mounted in", func(t *testing.T) {
		w := newRunWorld(t)
		value := "secret " + testenv.NewNonce()
		// The step that is given the secret compares it with the value, which it holds encoded.
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(
			    name="app",
			    base="busybox",
			    steps=[
			        run(["echo", %q]),
			        run(["sh", "-c", """
			            echo "observed secrets before=$([ -e /run/secrets ] && echo there || echo none)"
			        """]),
			        run(["sh", "-c", """
			            key=$(echo %s | base64 -d)
			            mounted=$(cat /run/secrets/key)
			            echo "observed key during=$([ "$mounted" = "$key" ] && echo matches || echo differs)"
			        """], secrets=[secret("key", file="/run/secrets/key")]),
			        run(["sh", "-c", """
			            echo "observed key after=$([ -e /run/secrets/key ] && echo there || echo none)"
			        """]),
			    ],
			)
		`, testenv.NewNonce(), base64.StdEncoding.EncodeToString([]byte(value))))
		w.GivenAFileOutsideTheProjectHolds("key", value)
		w.WhenISave("app", withSecrets("id=key,src=<outside>/key"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
		w.ThenTheStepsObserved(observations{
			"secrets before": "none",
			"key during":     "matches",
			"key after":      "none",
		})
		w.ThenTheImageDoesNotContain(value)
	})
}

// TestRun_HostnameAndHostsStayOutOfTheImage specifies that a step gets a hostname of its own
// and an /etc/hosts that names it and host.containers.internal, neither of which stays in the image.
func TestRun_HostnameAndHostsStayOutOfTheImage(t *testing.T) {
	t.Run("A step sees its own hostname and hosts, which the image doesn't keep", func(t *testing.T) {
		host, err := os.Hostname()
		if err != nil {
			t.Fatal(err)
		}
		w := newRunWorld(t)
		// The step compares its hostname with the host's, which it is given encoded, since only it sees its own.
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(
			    name="app",
			    base="busybox",
			    steps=[
			        run(["echo", %q]),
			        run(["sh", "-c", """
			            name=$(hostname)
			            host=$(echo %s | base64 -d)
			            if [ -z "$name" ]; then seen=none
			            elif [ "$name" = localhost ]; then seen=localhost
			            elif [ "$name" = "$host" ]; then seen="the host's"
			            else seen="its own"; fi
			            echo "observed hostname=$seen"
			            echo "observed hosts name it=$(grep -qwF "$name" /etc/hosts && echo yes || echo no)"
			            internal=$(grep -qwF host.containers.internal /etc/hosts && echo yes || echo no)
			            echo "observed hosts name the host=$internal"
			        """]),
			    ],
			)
		`, testenv.NewNonce(), base64.StdEncoding.EncodeToString([]byte(host))))
		w.WhenISave("busybox")
		w.ThenItSucceeds()
		w.ThenTheImageDoesNotHold("/etc/hosts", "/etc/hostname")
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
		w.ThenTheStepsObserved(observations{
			"hostname":            "its own",
			"hosts name it":       "yes",
			"hosts name the host": "yes",
		})
		w.ThenTheImageDoesNotHold("/etc/hosts", "/etc/hostname")
	})

	t.Run("A base's own hostname and hosts stay as they were", func(t *testing.T) {
		// A step's own are mounted over the base's, so another step can't write them, but add() can.
		w := newRunWorld(t)
		w.GivenTheBuildFileDeclares(`
			glob(name="etc", include=["etc/hosts", "etc/hostname"])
			layer(name="hosted", base="busybox", steps=[add("etc", "/")])
			layer(name="app", base="hosted", steps=[run(["cat", "/etc/hosts"])])
		`)
		w.GivenTheProjectHolds(files{"etc/hosts": "base-hosts\n", "etc/hostname": "base-name\n"})
		w.WhenISave("app")
		w.ThenItSucceeds()
		w.ThenTheImageHoldsAtLeast(files{"/etc/hosts": "base-hosts\n", "/etc/hostname": "base-name\n"})
	})
}
