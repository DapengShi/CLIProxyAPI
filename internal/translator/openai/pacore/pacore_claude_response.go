package pacore

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// State machine for parsing
type State int

const (
	StateNormal State = iota
	StateInThinking
	StateInToolCall
)

type PaCoReConvertParams struct {
	State          State
	Buffer         strings.Builder
	MessageStarted bool
	MessageID      string
	Model          string

	// Tracking content blocks
	NextContentBlockIndex     int
	TextContentBlockIndex     int
	ThinkingContentBlockIndex int
	ToolCallBlockIndexes      map[string]int // Map tool ID/name to block index

	// Accumulators
	ThinkingAccumulator strings.Builder
	ToolCallAccumulator strings.Builder

	TextContentBlockStarted     bool
	ThinkingContentBlockStarted bool
}

// PaCoReToClaudeResponse translates a PaCoRe stream (XML-in-text) to Claude events.
// It implements the sdktranslator.ResponseStreamTransform signature.
func PaCoReToClaudeResponse(ctx context.Context, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) []string {
	if *param == nil {
		*param = &PaCoReConvertParams{
			State:                     StateNormal,
			TextContentBlockIndex:     -1,
			ThinkingContentBlockIndex: -1,
			ToolCallBlockIndexes:      make(map[string]int),
		}
	}
	p := (*param).(*PaCoReConvertParams)
	var results []string

	// Initialize message if not started
	if !p.MessageStarted {
		p.MessageID = "msg_" + uuid.New().String()
		p.Model = model

		messageStartJSON := `{"type":"message_start","message":{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`
		messageStartJSON, _ = sjson.Set(messageStartJSON, "message.id", p.MessageID)
		messageStartJSON, _ = sjson.Set(messageStartJSON, "message.model", p.Model)
		results = append(results, "event: message_start\ndata: "+messageStartJSON+"\n\n")
		p.MessageStarted = true
	}

	// Append new chunk to buffer
	// rawJSON is expected to be the raw text chunk from PaCoRe
	// But wait, does PaCoRe return SSE or raw bytes?
	// If PaCoRe is an OpenAI-compatible proxy, it usually returns SSE with "data: {...}".
	// If it returns raw text stream, we treat rawJSON as text.
	// We assume here rawJSON is the content of the chunk.
	// If PaCoRe wraps it in OpenAI chunk format, we need to extract "choices[0].delta.content".
	// Let's assume PaCoRe returns OpenAI-compatible chunks but the content is the raw XML-text mix.

	chunkContent := ""
	// Try to parse as OpenAI chunk
	if gjson.ValidBytes(rawJSON) {
		chunkContent = gjson.GetBytes(rawJSON, "choices.0.delta.content").String()
	} else {
		// Fallback: treat as raw text
		chunkContent = string(rawJSON)
	}

	if chunkContent == "" {
		// Check for finish reason?
		finishReason := gjson.GetBytes(rawJSON, "choices.0.finish_reason").String()
		if finishReason != "" {
			return handleFinish(p, finishReason)
		}
		return results
	}

	// Feed character by character or chunk logic
	// Since we can have split tags, we append to buffer and scan.
	p.Buffer.WriteString(chunkContent)
	processBuffer(p, &results)

	return results
}

func processBuffer(p *PaCoReConvertParams, results *[]string) {
	// Simple lookahead parsing loop
	// We check if buffer contains tags.
	// Optimally, we want to emit text as soon as possible.

	const (
		tagThinkingStart = "<thinking>"
		tagThinkingEnd   = "</thinking>"
		tagToolCallStart = "<tool_call>"
		tagToolCallEnd   = "</tool_call>"
	)

	for p.Buffer.Len() > 0 {
		content := p.Buffer.String()

		switch p.State {
		case StateNormal:
			// Look for start tags
			thinkIdx := strings.Index(content, tagThinkingStart)
			toolIdx := strings.Index(content, tagToolCallStart)

			// Determine which tag comes first
			firstTagIdx := -1
			tagType := "" // "thinking" or "tool"

			if thinkIdx != -1 && (toolIdx == -1 || thinkIdx < toolIdx) {
				firstTagIdx = thinkIdx
				tagType = "thinking"
			} else if toolIdx != -1 {
				firstTagIdx = toolIdx
				tagType = "tool"
			}

			if firstTagIdx != -1 {
				// Flush text before tag
				if firstTagIdx > 0 {
					text := content[:firstTagIdx]
					emitTextDelta(p, results, text)
				}

				// Switch state and advance buffer past tag
				if tagType == "thinking" {
					p.State = StateInThinking
					p.Buffer.Reset()
					p.Buffer.WriteString(content[firstTagIdx+len(tagThinkingStart):])
					startThinkingBlock(p, results)
				} else {
					p.State = StateInToolCall
					p.Buffer.Reset()
					p.Buffer.WriteString(content[firstTagIdx+len(tagToolCallStart):])
					// We don't start tool block yet, we wait for full XML
				}
			} else {
				// No full tag found.
				// Check for partial tag at end.
				if isPartialTag(content) {
					// Keep the partial part, flush the rest.
					// Conservative: keep last 15 chars.
					if len(content) > 15 {
						toFlush := content[:len(content)-15]
						emitTextDelta(p, results, toFlush)
						p.Buffer.Reset()
						p.Buffer.WriteString(content[len(content)-15:])
					}
					return // Wait for more data
				} else {
					// Safe to flush all
					emitTextDelta(p, results, content)
					p.Buffer.Reset()
					return
				}
			}

		case StateInThinking:
			endIdx := strings.Index(content, tagThinkingEnd)
			if endIdx != -1 {
				// Flush thinking content
				text := content[:endIdx]
				emitThinkingDelta(p, results, text)

				// Stop thinking block
				stopThinkingBlock(p, results)

				// Reset state
				p.State = StateNormal
				p.Buffer.Reset()
				p.Buffer.WriteString(content[endIdx+len(tagThinkingEnd):])
			} else {
				// No end tag. Check partial.
				if isPartialTag(content) {
					if len(content) > 15 {
						toFlush := content[:len(content)-15]
						emitThinkingDelta(p, results, toFlush)
						p.Buffer.Reset()
						p.Buffer.WriteString(content[len(content)-15:])
					}
					return
				} else {
					emitThinkingDelta(p, results, content)
					p.Buffer.Reset()
					return
				}
			}

		case StateInToolCall:
			endIdx := strings.Index(content, tagToolCallEnd)
			if endIdx != -1 {
				// Full tool call XML found
				xmlStr := content[:endIdx]
				// Need to prepend the start tag because xml.Unmarshal expects it?
				// Our XML struct matches the content inside?
				// No, usually <tool_call>...</tool_call>.
				// But we stripped the start tag.
				// Let's reconstruct or parse inner.
				fullXML := tagToolCallStart + xmlStr + tagToolCallEnd

				var toolCall ToolCallXML
				if err := xml.Unmarshal([]byte(fullXML), &toolCall); err == nil {
					emitToolCall(p, results, toolCall)
				}

				p.State = StateNormal
				p.Buffer.Reset()
				p.Buffer.WriteString(content[endIdx+len(tagToolCallEnd):])
			} else {
				// Buffer everything until end tag is found
				// Do not flush partial tool calls as text!
				// Just return and wait for more data.
				return
			}
		}
	}
}

func emitTextDelta(p *PaCoReConvertParams, results *[]string, text string) {
	if text == "" {
		return
	}
	// Start text block if needed
	if !p.TextContentBlockStarted {
		if p.TextContentBlockIndex == -1 {
			p.TextContentBlockIndex = p.NextContentBlockIndex
			p.NextContentBlockIndex++
		}
		contentBlockStartJSON := `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
		contentBlockStartJSON, _ = sjson.Set(contentBlockStartJSON, "index", p.TextContentBlockIndex)
		*results = append(*results, "event: content_block_start\ndata: "+contentBlockStartJSON+"\n\n")
		p.TextContentBlockStarted = true
	}

	contentDeltaJSON := `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`
	contentDeltaJSON, _ = sjson.Set(contentDeltaJSON, "index", p.TextContentBlockIndex)
	contentDeltaJSON, _ = sjson.Set(contentDeltaJSON, "delta.text", text)
	*results = append(*results, "event: content_block_delta\ndata: "+contentDeltaJSON+"\n\n")
}

func startThinkingBlock(p *PaCoReConvertParams, results *[]string) {
	// Stop text block if active
	if p.TextContentBlockStarted {
		stopTextBlock(p, results)
	}

	if p.ThinkingContentBlockIndex == -1 {
		p.ThinkingContentBlockIndex = p.NextContentBlockIndex
		p.NextContentBlockIndex++
	}
	contentBlockStartJSON := `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`
	contentBlockStartJSON, _ = sjson.Set(contentBlockStartJSON, "index", p.ThinkingContentBlockIndex)
	*results = append(*results, "event: content_block_start\ndata: "+contentBlockStartJSON+"\n\n")
	p.ThinkingContentBlockStarted = true
}

func emitThinkingDelta(p *PaCoReConvertParams, results *[]string, text string) {
	if text == "" {
		return
	}
	// Ensure started (should be called after startThinkingBlock)
	if !p.ThinkingContentBlockStarted {
		startThinkingBlock(p, results)
	}
	thinkingDeltaJSON := `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`
	thinkingDeltaJSON, _ = sjson.Set(thinkingDeltaJSON, "index", p.ThinkingContentBlockIndex)
	thinkingDeltaJSON, _ = sjson.Set(thinkingDeltaJSON, "delta.thinking", text)
	*results = append(*results, "event: content_block_delta\ndata: "+thinkingDeltaJSON+"\n\n")
}

func stopThinkingBlock(p *PaCoReConvertParams, results *[]string) {
	if !p.ThinkingContentBlockStarted {
		return
	}
	contentBlockStopJSON := `{"type":"content_block_stop","index":0}`
	contentBlockStopJSON, _ = sjson.Set(contentBlockStopJSON, "index", p.ThinkingContentBlockIndex)
	*results = append(*results, "event: content_block_stop\ndata: "+contentBlockStopJSON+"\n\n")
	p.ThinkingContentBlockStarted = false
	p.ThinkingContentBlockIndex = -1 // Reset? Or keep unique? Usually reset or increment.
}

func stopTextBlock(p *PaCoReConvertParams, results *[]string) {
	if !p.TextContentBlockStarted {
		return
	}
	contentBlockStopJSON := `{"type":"content_block_stop","index":0}`
	contentBlockStopJSON, _ = sjson.Set(contentBlockStopJSON, "index", p.TextContentBlockIndex)
	*results = append(*results, "event: content_block_stop\ndata: "+contentBlockStopJSON+"\n\n")
	p.TextContentBlockStarted = false
	p.TextContentBlockIndex = -1
}

func emitToolCall(p *PaCoReConvertParams, results *[]string, toolCall ToolCallXML) {
	// Stop any active blocks
	if p.TextContentBlockStarted {
		stopTextBlock(p, results)
	}
	if p.ThinkingContentBlockStarted {
		stopThinkingBlock(p, results)
	}

	blockIndex := p.NextContentBlockIndex
	p.NextContentBlockIndex++

	// content_block_start
	contentBlockStartJSON := `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"","name":"","input":{}}}`
	contentBlockStartJSON, _ = sjson.Set(contentBlockStartJSON, "index", blockIndex)
	// Generate ID if missing
	id := "call_" + uuid.New().String()
	contentBlockStartJSON, _ = sjson.Set(contentBlockStartJSON, "content_block.id", id)
	contentBlockStartJSON, _ = sjson.Set(contentBlockStartJSON, "content_block.name", toolCall.Name)
	*results = append(*results, "event: content_block_start\ndata: "+contentBlockStartJSON+"\n\n")

	// content_block_delta (args)
	argsJSON, _ := json.Marshal(toolCall.Parameters)
	inputDeltaJSON := `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`
	inputDeltaJSON, _ = sjson.Set(inputDeltaJSON, "index", blockIndex)
	inputDeltaJSON, _ = sjson.Set(inputDeltaJSON, "delta.partial_json", string(argsJSON))
	*results = append(*results, "event: content_block_delta\ndata: "+inputDeltaJSON+"\n\n")

	// content_block_stop
	contentBlockStopJSON := `{"type":"content_block_stop","index":0}`
	contentBlockStopJSON, _ = sjson.Set(contentBlockStopJSON, "index", blockIndex)
	*results = append(*results, "event: content_block_stop\ndata: "+contentBlockStopJSON+"\n\n")
}

func handleFinish(p *PaCoReConvertParams, reason string) []string {
	var results []string
	if p.ThinkingContentBlockStarted {
		stopThinkingBlock(p, &results)
	}
	if p.TextContentBlockStarted {
		stopTextBlock(p, &results)
	}

	// message_delta
	messageDeltaJSON := `{"type":"message_delta","delta":{"stop_reason":"","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0}}`
	// Map reason if needed
	stopReason := "end_turn"
	if reason == "tool_calls" {
		stopReason = "tool_use"
	}
	messageDeltaJSON, _ = sjson.Set(messageDeltaJSON, "delta.stop_reason", stopReason)
	results = append(results, "event: message_delta\ndata: "+messageDeltaJSON+"\n\n")

	// message_stop
	results = append(results, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")

	return results
}

func isPartialTag(s string) bool {
	tags := []string{"<thinking>", "</thinking>", "<tool_call>", "</tool_call>"}
	for _, tag := range tags {
		for i := 1; i < len(tag); i++ {
			if strings.HasSuffix(s, tag[:i]) {
				return true
			}
		}
	}
	return false
}

type ToolCallXML struct {
	Name       string            `xml:"name"`
	Parameters map[string]string `xml:"parameters>parameter"`
}
