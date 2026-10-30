package cli

import (
	"strings"
	"testing"
)

// TestUsesContainerStore checks which command lines need container storage, and so the re-exec that gives a root
// without capabilities a user namespace: every command but a coordinator, unless it builds locally,
// and but help and --version.
func TestUsesContainerStore(t *testing.T) {
	for args, want := range map[string]bool{
		"build app":                     true,
		"worker --coordinator http://c": true,
		"attempt":                       true,
		"coordinator --listen :8080":    false,
		"coordinator --local":           true,
		"coordinator --local=true":      true,
		"coordinator --local=false":     false,
		"-C dir coordinator":            false,
		"-C coordinator build app":      true,
		"build coordinator":             true,
		"save coordinator out.tar":      true,
		"":                              false,
		"--nonsense":                    true,
		"--version":                     false,
		"-v":                            false,
		"--help":                        false,
		"help build":                    false,
		"completion bash":               false,
		"build --help":                  false,
		"build -h app":                  false,
		"-C dir build app":              true,
	} {
		if got := UsesContainerStore(strings.Fields(args)); got != want {
			t.Errorf("UsesContainerStore(%q) = %v, want %v", args, got, want)
		}
	}
}
