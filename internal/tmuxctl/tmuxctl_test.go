package tmuxctl_test

import (
	"testing"
)

// Unit tests for tmuxctl are limited because tmux requires a terminal.
// Integration tests in test/integration cover actual pane operations.

func TestImport(t *testing.T) {
	// Just ensure the package compiles.
	_ = t
}
