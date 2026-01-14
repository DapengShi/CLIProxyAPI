package executor

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"
)

func TestApplyClaudeToolPrefix(t *testing.T) {
	input := []byte(`{"tools":[{"name":"alpha"},{"name":"proxy_bravo"}],"tool_choice":{"type":"tool","name":"charlie"},"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"delta","id":"t1","input":{}}]}]}`)
	out := applyClaudeToolPrefix(input, "proxy_")

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "proxy_alpha" {
		t.Fatalf("tools.0.name = %q, want %q", got, "proxy_alpha")
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "proxy_bravo" {
		t.Fatalf("tools.1.name = %q, want %q", got, "proxy_bravo")
	}
	if got := gjson.GetBytes(out, "tool_choice.name").String(); got != "proxy_charlie" {
		t.Fatalf("tool_choice.name = %q, want %q", got, "proxy_charlie")
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != "proxy_delta" {
		t.Fatalf("messages.0.content.0.name = %q, want %q", got, "proxy_delta")
	}
}

func TestStripClaudeToolPrefixFromResponse(t *testing.T) {
	input := []byte(`{"content":[{"type":"tool_use","name":"proxy_alpha","id":"t1","input":{}},{"type":"tool_use","name":"bravo","id":"t2","input":{}}]}`)
	out := stripClaudeToolPrefixFromResponse(input, "proxy_")

	if got := gjson.GetBytes(out, "content.0.name").String(); got != "alpha" {
		t.Fatalf("content.0.name = %q, want %q", got, "alpha")
	}
	if got := gjson.GetBytes(out, "content.1.name").String(); got != "bravo" {
		t.Fatalf("content.1.name = %q, want %q", got, "bravo")
	}
}

func TestStripClaudeToolPrefixFromStreamLine(t *testing.T) {
	line := []byte(`data: {"type":"content_block_start","content_block":{"type":"tool_use","name":"proxy_alpha","id":"t1"},"index":0}`)
	out := stripClaudeToolPrefixFromStreamLine(line, "proxy_")

	payload := bytes.TrimSpace(out)
	if bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[len("data:"):])
	}
	if got := gjson.GetBytes(payload, "content_block.name").String(); got != "alpha" {
		t.Fatalf("content_block.name = %q, want %q", got, "alpha")
	}
}

func TestEnsureAssistantMessagesHaveThinkingBlock(t *testing.T) {
	tests := []struct {
		name           string
		input          string
		expectModified bool
		wantFirstType  string
	}{
		{
			name:           "no thinking enabled",
			input:          `{"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"test"}]}]}`,
			expectModified: false,
			wantFirstType:  "tool_use",
		},
		{
			name:           "thinking enabled, already has thinking block",
			input:          `{"thinking":{"type":"enabled"},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"test"},{"type":"tool_use","name":"test"}]}]}`,
			expectModified: false,
			wantFirstType:  "thinking",
		},
		{
			name:           "thinking enabled, tool_use first needs injection",
			input:          `{"thinking":{"type":"enabled"},"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"test"}]}]}`,
			expectModified: true,
			wantFirstType:  "redacted_thinking",
		},
		{
			name:           "thinking enabled, text first needs injection",
			input:          `{"thinking":{"type":"enabled"},"messages":[{"role":"assistant","content":[{"type":"text","text":"hello"}]}]}`,
			expectModified: true,
			wantFirstType:  "redacted_thinking",
		},
		{
			name:           "thinking enabled, redacted_thinking is valid",
			input:          `{"thinking":{"type":"enabled"},"messages":[{"role":"assistant","content":[{"type":"redacted_thinking"},{"type":"tool_use","name":"test"}]}]}`,
			expectModified: false,
			wantFirstType:  "redacted_thinking",
		},
		{
			name:           "user message not modified",
			input:          `{"thinking":{"type":"enabled"},"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`,
			expectModified: false,
			wantFirstType:  "text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := []byte(tt.input)
			output := ensureAssistantMessagesHaveThinkingBlock(input)

			// Check if first content type matches expected
			firstType := gjson.GetBytes(output, "messages.0.content.0.type").String()
			if firstType != tt.wantFirstType {
				t.Errorf("first content type = %q, want %q", firstType, tt.wantFirstType)
			}

			// Verify modification happened as expected
			modified := !bytes.Equal(input, output)
			if modified != tt.expectModified {
				t.Errorf("modified = %v, want %v", modified, tt.expectModified)
			}
		})
	}
}

func TestEnsureAssistantMessagesHaveThinkingBlock_MultipleAssistants(t *testing.T) {
	input := `{"thinking":{"type":"enabled"},"messages":[
		{"role":"user","content":[{"type":"text","text":"hello"}]},
		{"role":"assistant","content":[{"type":"tool_use","name":"test1"}]},
		{"role":"user","content":[{"type":"text","text":"continue"}]},
		{"role":"assistant","content":[{"type":"text","text":"ok"}]}
	]}`

	output := ensureAssistantMessagesHaveThinkingBlock([]byte(input))

	// Check first assistant message (index 1)
	firstType := gjson.GetBytes(output, "messages.1.content.0.type").String()
	if firstType != "redacted_thinking" {
		t.Errorf("messages[1] first content type = %q, want %q", firstType, "redacted_thinking")
	}

	// Check second assistant message (index 3)
	secondType := gjson.GetBytes(output, "messages.3.content.0.type").String()
	if secondType != "redacted_thinking" {
		t.Errorf("messages[3] first content type = %q, want %q", secondType, "redacted_thinking")
	}

	// Verify original content is preserved after the injected block
	toolType := gjson.GetBytes(output, "messages.1.content.1.type").String()
	if toolType != "tool_use" {
		t.Errorf("messages[1] second content type = %q, want %q", toolType, "tool_use")
	}
}
