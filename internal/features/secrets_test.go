package features

import (
	"cmp"
	"fmt"
	"testing"

	"github.com/Topher-the-Geek/vamos/internal/testenv"
)

// newSecretWorld starts a scenario of the secrets feature, from its background.
// A run() step that needs a credential is given it as a secret, whose value the command line gives with --secret,
// from an environment variable of the vamos process or a file.
// Its project pulls busybox.
func newSecretWorld(t *testing.T) *world {
	t.Helper()
	w := newBuildingWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// newSecretMistakeWorld starts a scenario of the secrets feature that fails before anything is built,
// so that it needs no buildah.
// Its project pulls busybox.
func newSecretMistakeWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.GivenAProjectThatPullsBusybox()
	return w
}

// stepWithSecrets declares the layer "app", whose step observes the secret "token", given as $TOKEN,
// and "key", given as the file /run/secrets/key: whether each matches the value that the scenario gives,
// which the step holds encoded, so that neither it nor its command holds the value, and who owns the file
// and whether it can be written. The next step observes whether it has either.
// Secrets are compared, never printed, since a step's output is cached and replayed.
// A step before them prints nonce, so that they run rather than come from the cache.
func stepWithSecrets(nonce string) string {
	return fmt.Sprintf(`
		layer(
		    name="app",
		    base="busybox",
		    steps=[
		        run(["echo", %q]),
		        run(["sh", "-c", """
		            token=$(echo ZW52LXZhbHVl | base64 -d)
		            key=$(echo ZmlsZS12YWx1ZQ== | base64 -d)
		            echo "observed token during=$([ "$TOKEN" = "$token" ] && echo matches || echo differs)"
		            echo "observed key during=$([ "$(cat /run/secrets/key)" = "$key" ] && echo matches || echo differs)"
		            echo "observed key owner=$(stat -c %%u:%%g /run/secrets/key)"
		            writes=$(sh -c 'echo changed > /run/secrets/key' 2>/dev/null && echo allowed || echo refused)
		            echo "observed key writes=$writes"
		        """], secrets=[secret("token", env="TOKEN"), secret("key", file="/run/secrets/key")]),
		        run(["sh", "-c", """
		            echo "observed token after=$([ -n "${TOKEN+set}" ] && echo set || echo unset)"
		            echo "observed key after=$([ -e /run/secrets/key ] && echo there || echo gone)"
		        """]),
		    ],
		)
	`, nonce)
}

// secretsSeen returns what stepWithSecrets observes when the scenario gives it env-value as the token
// and file-value as the key.
func secretsSeen() observations {
	return observations{
		"token during": "matches",
		"key during":   "matches",
		"key owner":    "0:0",
		"key writes":   "refused",
		"token after":  "unset",
		"key after":    "gone",
	}
}

// stepWithASecret declares the layer "app", whose one step echoes nonce, given the secret as secret declares.
// Scenarios change secret with GivenTheBuildFileChanges.
func stepWithASecret(nonce, secret string) string {
	return fmt.Sprintf(`layer(name="app", base="busybox", steps=[run(["echo", %q], secrets=[%s])])`, nonce, secret)
}

// TestSecrets_EachStepGetsItsOwn specifies that secret(id, env = ...) sets the variable env for that step alone,
// and secret(id, file = ...) mounts a read-only file, owned by root, at that absolute path for that step alone.
func TestSecrets_EachStepGetsItsOwn(t *testing.T) {
	t.Run("A step gets its secrets from the environment and a file, and the next step has neither", func(t *testing.T) {
		w := newSecretWorld(t)
		w.GivenTheBuildFileDeclares(stepWithSecrets(testenv.NewNonce()))
		w.GivenTheEnvironmentSets("VAMOS_TEST_TOKEN", "env-value")
		w.GivenAFileOutsideTheProjectHolds("key", "file-value")
		w.WhenISave("app", withSecrets("id=token,env=VAMOS_TEST_TOKEN", "id=key,src=<outside>/key"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
		w.ThenTheStepsObserved(secretsSeen())
	})
}

// TestSecrets_ValuesNeverEnterTheImage specifies that a secret's value never enters an image.
func TestSecrets_ValuesNeverEnterTheImage(t *testing.T) {
	t.Run("The image holds neither the secrets' values nor their file", func(t *testing.T) {
		w := newSecretWorld(t)
		w.GivenTheBuildFileDeclares(stepWithSecrets(testenv.NewNonce()))
		w.GivenTheEnvironmentSets("VAMOS_TEST_TOKEN", "env-value")
		w.GivenAFileOutsideTheProjectHolds("key", "file-value")
		w.WhenISave("app", withSecrets("id=token,env=VAMOS_TEST_TOKEN", "id=key,src=<outside>/key"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
		w.ThenTheStepsObserved(secretsSeen()) // so that the step had the values that the image mustn't hold
		w.ThenTheImageHoldsNothing(under("/run"))
		w.ThenTheImageEnvironmentDoesNotSet("TOKEN")
		w.ThenTheImageDoesNotContain("env-value", "file-value")
	})
}

// TestSecrets_KeyCoversHowTheStepGetsASecretNotItsValue specifies that a step's cache key covers each secret's ID and
// how the step is given it, the variable or file, but not its value, nor where --secret reads it,
// so a new value doesn't run the step again.
func TestSecrets_KeyCoversHowTheStepGetsASecretNotItsValue(t *testing.T) {
	t.Run("A new value reuses the step, and the output replayed is from the run that made it", func(t *testing.T) {
		w := newSecretWorld(t)
		nonce := testenv.NewNonce()
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(name="app", base="busybox", steps=[
			    run(
			        ["sh", "-c", 'echo %s; echo "observed token length=$(printf %%s "$TOKEN" | wc -c)"'],
			        secrets=[secret("token", env="TOKEN")],
			    ),
			])
		`, nonce))
		w.GivenTheEnvironmentSets("VAMOS_TEST_TOKEN", "first")
		w.WhenIBuild("app", withSecrets("id=token,env=VAMOS_TEST_TOKEN"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
		w.ThenTheStepsObserved(observations{"token length": "5"})
		w.GivenTheEnvironmentSets("VAMOS_TEST_TOKEN", "rotated")
		w.WhenIBuild("app", withSecrets("id=token,env=VAMOS_TEST_TOKEN"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): reused})
		w.ThenTheStepsObserved(observations{"token length": "5"}) // replayed from the run that made it
	})

	t.Run("A value read from elsewhere reuses the step", func(t *testing.T) {
		w := newSecretWorld(t)
		w.GivenTheBuildFileDeclares(stepWithASecret(testenv.NewNonce(), `secret("token", env="TOKEN")`))
		w.GivenTheEnvironmentSets("VAMOS_TEST_TOKEN", "value")
		w.GivenAFileOutsideTheProjectHolds("token", "value")
		w.WhenIBuild("app", withSecrets("id=token,env=VAMOS_TEST_TOKEN"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
		w.WhenISave("app", withSecrets("id=token,src=<outside>/token"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): reused})
	})

	for _, example := range []struct {
		name     string
		old, new string // how the step is given the secret, and then
		id       string // the secret's ID then, if not token
	}{
		{
			name: "A secret under another ID runs the step again",
			old:  `secret("token", env="TOKEN")`,
			new:  `secret("api", env="TOKEN")`,
			id:   "api",
		},
		{
			name: "A secret given in another variable runs the step again",
			old:  `secret("token", env="TOKEN")`,
			new:  `secret("token", env="API_TOKEN")`,
		},
		{
			name: "A secret given as a file rather than a variable runs the step again",
			old:  `secret("token", env="TOKEN")`,
			new:  `secret("token", file="/run/token")`,
		},
		{
			name: "A secret given at another path runs the step again",
			old:  `secret("token", file="/run/token")`,
			new:  `secret("token", file="/run/other")`,
		},
	} {
		t.Run(example.name, func(t *testing.T) {
			w := newSecretWorld(t)
			w.GivenTheBuildFileDeclares(stepWithASecret(testenv.NewNonce(), example.old))
			w.GivenTheEnvironmentSets("VAMOS_TEST_TOKEN", "value")
			w.WhenIBuild("app", withSecrets("id=token,env=VAMOS_TEST_TOKEN"))
			w.ThenItSucceeds()
			w.ThenTheTargetsWere(statuses{layer("app"): ran})
			w.GivenTheBuildFileChanges(example.old, example.new)
			id := cmp.Or(example.id, "token")
			w.WhenIBuild("app", withSecrets("id="+id+",env=VAMOS_TEST_TOKEN"))
			w.ThenItSucceeds()
			w.ThenTheTargetsWere(statuses{layer("app"): ran})
		})
	}
}

// TestSecrets_VariableMustBeSet specifies that env= reads an environment variable of the vamos process,
// which must be set, though it may be empty.
func TestSecrets_VariableMustBeSet(t *testing.T) {
	t.Run("A secret from an unset variable fails", func(t *testing.T) {
		w := newSecretMistakeWorld(t)
		w.GivenTheBuildFileDeclares(`
			layer(name="app", base="busybox", steps=[run(["true"], secrets=[secret("token", env="TOKEN")])])
		`)
		w.GivenTheEnvironmentDoesNotSet("VAMOS_TEST_UNSET")
		w.WhenISave("app", withSecrets("id=token,env=VAMOS_TEST_UNSET"))
		w.ThenItFailsWith("--secret id=token,env=VAMOS_TEST_UNSET: $VAMOS_TEST_UNSET isn't set")
		w.ThenNothingIsSaved()
	})

	t.Run("A secret from an empty variable is empty", func(t *testing.T) {
		w := newSecretWorld(t)
		w.GivenTheBuildFileDeclares(fmt.Sprintf(`
			layer(name="app", base="busybox", steps=[
			    run(
			        ["sh", "-c", 'echo %s; echo "observed token=${TOKEN+set}, $(printf %%s "$TOKEN" | wc -c) bytes"'],
			        secrets=[secret("token", env="TOKEN")],
			    ),
			])
		`, testenv.NewNonce()))
		w.GivenTheEnvironmentSets("VAMOS_TEST_TOKEN", "")
		w.WhenISave("app", withSecrets("id=token,env=VAMOS_TEST_TOKEN"))
		w.ThenItSucceeds()
		w.ThenTheTargetsWere(statuses{layer("app"): ran})
		w.ThenTheStepsObserved(observations{"token": "set, 0 bytes"})
	})
}
