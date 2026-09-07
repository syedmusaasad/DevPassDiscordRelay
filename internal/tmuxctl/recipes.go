package tmuxctl

import (
	"fmt"
)

// KeyRecipe is the exact ordered sequence sent to a devpass-code pane.
type KeyRecipe []string

// ApprovalApproveRecipe selects the default "Allow once" action then confirms.
// verified against devpass-code 1.18.11
var ApprovalApproveRecipe = KeyRecipe{"Enter"}

// ApprovalDenyRecipe selects "Reject" from "Allow once", "Allow always", "Reject" then confirms.
// verified against devpass-code 1.18.11
var ApprovalDenyRecipe = KeyRecipe{"Right", "Right", "Enter"}

// QuestionSelectRecipe moves from the currently highlighted fake picker option to
// the selected option and confirms. unverified against real TUI; verified against devpass-code 1.18.11 fake CLI picker
var QuestionSelectRecipe = KeyRecipe{"Down", "Enter"}

// SendRecipe sends each named tmux key in order. Recipes deliberately use named
// keys rather than literal escape bytes so tmux preserves terminal semantics.
func SendRecipe(target string, recipe KeyRecipe) error {
	for _, key := range recipe {
		cmd, err := NewCommand("send-keys", "-t", target, key)
		if err != nil {
			return err
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("tmuxctl: send key %q: %w: %s", key, err, out)
		}
	}
	return nil
}

// SelectQuestion sends the pinned picker recipe, repeating Down for the requested
// zero-based option index.
func SelectQuestion(target string, option int) error {
	if option < 0 {
		return fmt.Errorf("tmuxctl: negative question option %d", option)
	}
	recipe := make(KeyRecipe, 0, option+1)
	for i := 0; i < option; i++ {
		recipe = append(recipe, QuestionSelectRecipe[0])
	}
	recipe = append(recipe, QuestionSelectRecipe[len(QuestionSelectRecipe)-1])
	return SendRecipe(target, recipe)
}
