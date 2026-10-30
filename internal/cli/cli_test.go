package cli

import (
	"os"
	"testing"
)

// validBuild is a BUILD file that would build, were the command line right.
var validBuild = map[string]string{
	"BUILD": pullBusybox + `layer(name="app", base="busybox", command=["echo", "hello"])`,
}

func TestMissingAuthFile(t *testing.T) {
	t.Setenv("REGISTRY_AUTH_FILE", "/nonexistent/auth.json")

	vamos(t, project(t, validBuild), "push", "app", "app", "-C", projectDir).
		failsWith(t, false, "REGISTRY_AUTH_FILE", "/nonexistent/auth.json", "no such file")
}

// assertNoFiles fails the test if dir holds anything, such as a partly written image.
func assertNoFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("unexpected file left in %s: %s", dir, e.Name())
	}
}
