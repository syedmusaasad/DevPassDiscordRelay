package mirror_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/devpass/devpass-discord-relay/internal/mirror"
	"github.com/devpass/devpass-discord-relay/internal/transcript"
)

// ── Chunking tests ────────────────────────────────────────────────────────────

func TestChunkText_ShortText(t *testing.T) {
	text := "Hello, world!"
	chunks := mirror.ChunkText(text, 2000)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0] != text {
		t.Errorf("expected %q, got %q", text, chunks[0])
	}
}

func TestChunkText_ExactLimit(t *testing.T) {
	text := make([]byte, 2000)
	for i := range text {
		text[i] = 'a'
	}
	chunks := mirror.ChunkText(string(text), 2000)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
}

func TestChunkText_ParagraphBreak(t *testing.T) {
	// Build text with a paragraph break within the first 2000 chars
	part1 := repeat("a", 1000)
	part2 := repeat("b", 1500)
	text := part1 + "\n\n" + part2 // total > 2000
	chunks := mirror.ChunkText(text, 2000)
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}
	// First chunk should end at the paragraph boundary
	if !endsWith(chunks[0], "\n\n") && !endsWith(chunks[0], part1) {
		t.Errorf("first chunk should break at paragraph: got %q...", chunks[0][:min(50, len(chunks[0]))]+"..")
	}
}

func TestChunkText_ReassemblesCorrectly(t *testing.T) {
	// The chunks should reassemble to the original text
	text := repeat("word ", 600) // ~3000 chars
	chunks := mirror.ChunkText(text, 2000)
	reassembled := ""
	for _, c := range chunks {
		reassembled += c
	}
	if reassembled != text {
		t.Errorf("reassembled text differs from original; len orig=%d len reassembled=%d",
			len(text), len(reassembled))
	}
}

func TestChunkText_DefaultLimit(t *testing.T) {
	text := repeat("x", 4001)
	chunks := mirror.ChunkText(text, 0) // 0 means use default 2000
	for i, c := range chunks {
		if len([]rune(c)) > 2000 {
			t.Errorf("chunk %d exceeds 2000 chars: %d", i, len([]rune(c)))
		}
	}
}

// ── Throttle tests ────────────────────────────────────────────────────────────

func TestThrottle_AllowsUpToLimit(t *testing.T) {
	th := mirror.NewThrottle(3, 5*time.Second)
	// Should allow 3 immediate calls
	for i := 0; i < 3; i++ {
		done := make(chan struct{})
		go func() {
			th.Wait("chan1")
			close(done)
		}()
		select {
		case <-done:
			// OK
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("call %d should not have blocked", i+1)
		}
	}
}

func TestThrottle_BlocksAtLimit(t *testing.T) {
	th := mirror.NewThrottle(2, 2*time.Second)
	// Exhaust the limit
	th.Wait("ch")
	th.Wait("ch")
	// Third call should block for ~2s; check it does NOT return instantly
	done := make(chan struct{})
	go func() {
		th.Wait("ch")
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("third call returned immediately; expected throttle block")
	case <-time.After(100 * time.Millisecond):
		// Good – it's blocking
	}
}

func TestThrottle_BackoffDelays(t *testing.T) {
	th := mirror.NewThrottle(5, 5*time.Second)
	th.SetBackoff("ch", 500*time.Millisecond)

	done := make(chan struct{})
	start := time.Now()
	go func() {
		th.Wait("ch")
		close(done)
	}()
	select {
	case <-done:
		elapsed := time.Since(start)
		if elapsed < 400*time.Millisecond {
			t.Errorf("backoff resolved too fast: %v", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("backoff never resolved")
	}
}

func TestThrottle_IndependentChannels(t *testing.T) {
	th := mirror.NewThrottle(1, 5*time.Second)
	th.Wait("ch-a")
	// ch-a is now at limit; ch-b should still be immediately available
	done := make(chan struct{})
	go func() {
		th.Wait("ch-b")
		close(done)
	}()
	select {
	case <-done:
		// Good
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ch-b should not have been throttled by ch-a's limit")
	}
}

// ── Part classification tests ─────────────────────────────────────────────────

func TestClassifyPart_AssistantText(t *testing.T) {
	p := transcript.Part{
		ID:        "prt1",
		MessageID: "msg1",
		SessionID: "ses1",
		Data:      json.RawMessage(`{"type":"text","text":"Hello from assistant"}`),
	}
	cp := mirror.ClassifyPart(p, "assistant", false)
	if cp.Kind != mirror.PartKindText {
		t.Errorf("expected PartKindText, got %v", cp.Kind)
	}
	if cp.Text != "Hello from assistant" {
		t.Errorf("expected text %q, got %q", "Hello from assistant", cp.Text)
	}
	if cp.IsPreamble {
		t.Error("expected IsPreamble=false")
	}
}

func TestClassifyPart_UserTextNotPreamble(t *testing.T) {
	p := transcript.Part{
		ID:        "prt2",
		MessageID: "msg2",
		SessionID: "ses1",
		Data:      json.RawMessage(`{"type":"text","text":"ping"}`),
	}
	// isPreambleMsg=false → this is the user's typed prompt
	cp := mirror.ClassifyPart(p, "user", false)
	if cp.Kind != mirror.PartKindText {
		t.Errorf("expected PartKindText, got %v", cp.Kind)
	}
	if cp.IsPreamble {
		t.Error("expected IsPreamble=false")
	}
}

func TestClassifyPart_PreambleBearingUserMessage(t *testing.T) {
	// A user message with isPreambleMsg=true must be classified as Preamble,
	// NOT as PartKindText. This is the core invariant: preamble-bearing user
	// messages must NOT count as a second prompt.
	p := transcript.Part{
		ID:        "prt3",
		MessageID: "msg3",
		SessionID: "ses1",
		Data:      json.RawMessage(`{"type":"text","text":"system context preamble"}`),
	}
	cp := mirror.ClassifyPart(p, "user", true) // isPreambleMsg=true
	if cp.Kind != mirror.PartKindPreamble {
		t.Errorf("expected PartKindPreamble, got %v", cp.Kind)
	}
	if !cp.IsPreamble {
		t.Error("expected IsPreamble=true")
	}
	if cp.Kind == mirror.PartKindText {
		t.Error("preamble-bearing user part must not be classified as PartKindText (second prompt)")
	}
}

func TestClassifyPart_ToolCall(t *testing.T) {
	p := transcript.Part{
		ID:        "prt4",
		MessageID: "msg4",
		SessionID: "ses1",
		Data:      json.RawMessage(`{"type":"tool","tool":"bash","callID":"call_abc123","state":{"status":"running"}}`),
	}
	cp := mirror.ClassifyPart(p, "assistant", false)
	if cp.Kind != mirror.PartKindToolCall {
		t.Errorf("expected PartKindToolCall, got %v", cp.Kind)
	}
	if cp.ToolName != "bash" {
		t.Errorf("expected tool name 'bash', got %q", cp.ToolName)
	}
}

func TestClassifyPart_StepFinish(t *testing.T) {
	p := transcript.Part{
		ID:        "prt5",
		MessageID: "msg5",
		SessionID: "ses1",
		Data:      json.RawMessage(`{"reason":"stop","type":"step-finish","tokens":{"total":100},"cost":0.001}`),
	}
	cp := mirror.ClassifyPart(p, "assistant", false)
	if cp.Kind != mirror.PartKindStepFinish {
		t.Errorf("expected PartKindStepFinish, got %v", cp.Kind)
	}
}

func TestClassifyPart_StepStart(t *testing.T) {
	p := transcript.Part{
		ID:        "prt6",
		MessageID: "msg6",
		SessionID: "ses1",
		Data:      json.RawMessage(`{"type":"step-start"}`),
	}
	cp := mirror.ClassifyPart(p, "assistant", false)
	if cp.Kind != mirror.PartKindStepStart {
		t.Errorf("expected PartKindStepStart, got %v", cp.Kind)
	}
}

func TestIsPreambleMessage_WithAgent(t *testing.T) {
	data := json.RawMessage(`{"role":"user","agent":"pl-interactive","model":{"providerID":"test"}}`)
	if !mirror.IsPreambleMessage(data) {
		t.Error("expected user message with agent to be preamble")
	}
}

func TestIsPreambleMessage_WithoutAgent(t *testing.T) {
	data := json.RawMessage(`{"role":"user","time":{"created":12345}}`)
	if mirror.IsPreambleMessage(data) {
		t.Error("expected user message without agent to NOT be preamble")
	}
}

func TestIsPreambleMessage_Assistant(t *testing.T) {
	data := json.RawMessage(`{"role":"assistant","agent":"pl-interactive"}`)
	if mirror.IsPreambleMessage(data) {
		t.Error("expected assistant message to NOT be preamble")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func repeat(s string, n int) string {
	result := ""
	for i := 0; i < n; i++ {
		result += s
	}
	return result
}

func endsWith(s, suffix string) bool {
	if len(s) < len(suffix) {
		return false
	}
	return s[len(s)-len(suffix):] == suffix
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
