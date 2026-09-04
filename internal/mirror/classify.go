package mirror

import (
	"encoding/json"

	"github.com/devpass/devpass-discord-relay/internal/transcript"
)

// PartKind classifies a transcript part.
type PartKind int

const (
	PartKindUnknown    PartKind = iota
	PartKindText                // assistant or user text
	PartKindToolCall            // tool-call (type:"tool" or "tool-call")
	PartKindStepStart           // type:"step-start"
	PartKindStepFinish          // type:"step-finish"
	PartKindPreamble            // harness/agent-injected preamble (user message, but not the typed prompt)
)

// ClassifiedPart is a transcript.Part with its classification and decoded data.
type ClassifiedPart struct {
	transcript.Part
	Kind    PartKind
	Text    string // for text parts
	ToolID  string // for tool parts
	ToolName string
	// IsPreamble is true when this is a user text part that was harness-injected
	IsPreamble bool
}

// partDataRaw holds the raw decoded fields from part.data.
type partDataRaw struct {
	Type   string          `json:"type"`
	Text   string          `json:"text,omitempty"`
	Tool   string          `json:"tool,omitempty"`
	CallID string          `json:"callID,omitempty"`
	Reason string          `json:"reason,omitempty"`
	State  json.RawMessage `json:"state,omitempty"`
}

// ClassifyPart classifies a part and returns a ClassifiedPart.
// messageRole is the role of the containing message ("user" or "assistant").
// isPreambleMsg is true when the user message containing this part is a
// harness-injected preamble message (has agent/model fields but no typed prompt).
func ClassifyPart(p transcript.Part, messageRole string, isPreambleMsg bool) ClassifiedPart {
	cp := ClassifiedPart{Part: p}

	var pd partDataRaw
	if err := json.Unmarshal(p.Data, &pd); err != nil {
		cp.Kind = PartKindUnknown
		return cp
	}

	switch pd.Type {
	case "text":
		cp.Text = pd.Text
		if isPreambleMsg {
			cp.Kind = PartKindPreamble
			cp.IsPreamble = true
		} else {
			cp.Kind = PartKindText
		}
	case "tool", "tool-call":
		cp.Kind = PartKindToolCall
		cp.ToolName = pd.Tool
		cp.ToolID = pd.CallID
	case "step-start":
		cp.Kind = PartKindStepStart
	case "step-finish":
		cp.Kind = PartKindStepFinish
	default:
		cp.Kind = PartKindUnknown
	}
	return cp
}

// MessageData is the decoded message.data JSON.
type MessageData struct {
	Role    string `json:"role"`
	Finish  string `json:"finish,omitempty"`
	Agent   string `json:"agent,omitempty"`
	// ParentID is set on subagent sessions' messages.
	ParentID string `json:"parentID,omitempty"`
}

// ParseMessageData decodes a message's data field.
func ParseMessageData(data json.RawMessage) (MessageData, error) {
	var md MessageData
	err := json.Unmarshal(data, &md)
	return md, err
}

// IsPreambleMessage returns true if the message is a harness/agent-injected
// preamble (user message that has agent/model context but was not typed by the
// human at the terminal).
//
// Heuristic: a user message with an "agent" field in data is a preamble.
// This is the PART-level classification, never applied on joined text.
func IsPreambleMessage(data json.RawMessage) bool {
	// Check for agent field on user message – indicates harness injection
	var raw struct {
		Role  string `json:"role"`
		Agent string `json:"agent,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	return raw.Role == "user" && raw.Agent != ""
}
