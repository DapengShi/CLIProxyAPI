package util

import (
	"strings"
)

type ToolIntent struct {
	Name      string
	Arguments map[string]any
	Raw       string
}

// ParseToolIntents extracts tool intents embedded as tags in a text blob.
// It returns the remaining text with tags removed and a list of extracted intents.
func ParseToolIntents(text string) (string, []ToolIntent) {
	remaining := text
	intents := []ToolIntent{}

	for {
		start, end, raw := findTagBlock(remaining, "websearch")
		if start == -1 || end == -1 {
			break
		}
		question := extractTagValue(raw, "question")
		if question != "" {
			intents = append(intents, ToolIntent{
				Name: "websearch",
				Arguments: map[string]any{
					"question": strings.TrimSpace(question),
				},
				Raw: raw,
			})
		}
		remaining = remaining[:start] + remaining[end:]
	}

	return remaining, intents
}

// ToolIntentBuffer handles streaming-safe parsing of tag-based tool intents.
// It buffers partial tags and emits only valid tool intents.
type ToolIntentBuffer struct {
	buffer    strings.Builder
	maxBuffer int
}

func NewToolIntentBuffer() *ToolIntentBuffer {
	return &ToolIntentBuffer{maxBuffer: 8192}
}

// Feed ingests new text and returns flushable text plus any detected tool intents.
func (b *ToolIntentBuffer) Feed(text string) (string, []ToolIntent) {
	if text == "" {
		return "", nil
	}
	b.buffer.WriteString(text)
	combined := b.buffer.String()
	remaining, intents := ParseToolIntents(combined)

	flushable, keep := splitFlushable(remaining)
	b.buffer.Reset()
	b.buffer.WriteString(keep)

	// Avoid unbounded growth if tags are malformed.
	if b.buffer.Len() > b.maxBuffer {
		over := b.buffer.String()
		b.buffer.Reset()
		return over, intents
	}

	return flushable, intents
}

func splitFlushable(text string) (string, string) {
	// Check if there's an incomplete websearch tag pair
	websearchStart := strings.Index(text, "<websearch>")
	if websearchStart != -1 {
		// Found opening tag, check for closing tag after it
		websearchEnd := strings.Index(text[websearchStart:], "</websearch>")
		if websearchEnd == -1 {
			// Incomplete websearch tag pair, keep everything from the opening tag
			return text[:websearchStart], text[websearchStart:]
		}
		// Complete websearch tag pair exists, but there might be more after it
		// Check if there's another incomplete websearch after this one
		afterComplete := websearchStart + websearchEnd + len("</websearch>")
		if afterComplete < len(text) {
			remaining := text[afterComplete:]
			nextWebsearchStart := strings.Index(remaining, "<websearch>")
			if nextWebsearchStart != -1 {
				// Found another websearch tag
				return text[:afterComplete+nextWebsearchStart], text[afterComplete+nextWebsearchStart:]
			}
		}
	}

	// Fall back to checking for incomplete single tag
	idx := strings.LastIndex(text, "<")
	if idx == -1 {
		return text, ""
	}
	if strings.Contains(text[idx:], ">") {
		return text, ""
	}
	return text[:idx], text[idx:]
}

func extractTagValue(raw, tag string) string {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	start := strings.Index(raw, open)
	if start == -1 {
		return ""
	}
	start += len(open)
	end := strings.Index(raw[start:], close)
	if end == -1 {
		return ""
	}
	return raw[start : start+end]
}

func findTagBlock(input, tag string) (int, int, string) {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	start := strings.Index(input, open)
	if start == -1 {
		return -1, -1, ""
	}
	end := strings.Index(input[start:], close)
	if end == -1 {
		return -1, -1, ""
	}
	end = start + end + len(close)
	return start, end, input[start:end]
}
