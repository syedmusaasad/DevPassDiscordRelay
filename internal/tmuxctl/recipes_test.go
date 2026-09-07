package tmuxctl

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestInvariant5_RecipesVersionPinned makes the researched CLI version an
// explicit maintenance gate: adding a keystroke recipe requires recording the
// version against which it was verified (or fake-verified for the picker).
func TestInvariant5_RecipesVersionPinned(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate recipes test source")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "recipes.go"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if !strings.Contains(line, "Recipe =") {
			continue
		}
		start := i - 3
		if start < 0 {
			start = 0
		}
		if !strings.Contains(strings.Join(lines[start:i+1], "\n"), "verified against devpass-code 1.18.11") {
			t.Fatalf("recipe declaration at line %d lacks devpass-code 1.18.11 pin", i+1)
		}
	}
}
