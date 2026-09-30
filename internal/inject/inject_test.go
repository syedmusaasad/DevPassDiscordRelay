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
