package inject

import "testing"

func TestShouldAcceptAuthor_EmptyIDRejectsAll(t *testing.T) {
	for _, author := range []string{"", "123", "user-allowed-001"} {
		for _, bot := range []bool{false, true} {
			if ShouldAcceptAuthor(author, bot, "") {
				t.Fatalf("ShouldAcceptAuthor(%q, bot=%v, \"\") = true, want false", author, bot)
			}
		}
	}
	if !ShouldAcceptAuthor("u1", false, "u1") {
		t.Fatal("configured user must still be accepted")
	}
	if ShouldAcceptAuthor("u2", false, "u1") {
		t.Fatal("other user must be rejected")
	}
}

// Live captures from devpass-code 1.18.11 (see docs/REMEDIATION-24.md).
const freshPane = `
     ┃
     ┃  Ask anything... "Fix broken tests"
     ┃
     ┃  Pl-Interactive auto · GLM-5.3 LLM Gateway DevPass
     ╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀
     tab agents  ctrl+p commands
`

const postTurnPane = `
  ┃
  ┃  reply with just the word ok
  ┃
     ok
     ▣  Pl-Interactive · GLM-5.3 · 2.7s
  ┃
  ┃
  ┃
  ┃  Pl-Interactive auto · GLM-5.3 LLM Gateway DevPass
  ╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀
   /tmp/devpass-code/live                                                           5.0K (0%) · $0.00  ctrl+p commands
`

func TestPaneReady(t *testing.T) {
	if !PaneReady(freshPane) {
		t.Fatal("fresh pane (Ask anything...) must be ready")
	}
	if !PaneReady(postTurnPane) {
		t.Fatal("post-turn pane (no placeholder) must be ready via the input row")
	}
	// The assistant footer "▣  Pl-Interactive · GLM-5.3 · 2.7s" alone is not an input row.
	if PaneReady("     ok\n     ▣  Pl-Interactive · GLM-5.3 · 2.7s\n") {
		t.Fatal("assistant footer without the input bar must not be ready")
	}
	if PaneReady("") || PaneReady("loading...\n") {
		t.Fatal("blank/booting pane must not be ready")
	}
}
