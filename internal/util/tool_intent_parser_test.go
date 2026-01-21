package util

import (
	"testing"
)

func TestParseToolIntents_CompleteTag(t *testing.T) {
	text := "Some text <websearch><question>What is AI?</question></websearch> more text"
	remaining, intents := ParseToolIntents(text)

	if len(intents) != 1 {
		t.Fatalf("Expected 1 intent, got %d", len(intents))
	}

	if intents[0].Name != "websearch" {
		t.Errorf("Expected name 'websearch', got '%s'", intents[0].Name)
	}

	if intents[0].Arguments["question"] != "What is AI?" {
		t.Errorf("Expected question 'What is AI?', got '%v'", intents[0].Arguments["question"])
	}

	expected := "Some text  more text"
	if remaining != expected {
		t.Errorf("Expected remaining '%s', got '%s'", expected, remaining)
	}
}

func TestParseToolIntents_MultipleCompleteTags(t *testing.T) {
	text := "First <websearch><question>Q1</question></websearch> middle <websearch><question>Q2</question></websearch> end"
	remaining, intents := ParseToolIntents(text)

	if len(intents) != 2 {
		t.Fatalf("Expected 2 intents, got %d", len(intents))
	}

	if intents[0].Arguments["question"] != "Q1" {
		t.Errorf("Expected first question 'Q1', got '%v'", intents[0].Arguments["question"])
	}

	if intents[1].Arguments["question"] != "Q2" {
		t.Errorf("Expected second question 'Q2', got '%v'", intents[1].Arguments["question"])
	}

	expected := "First  middle  end"
	if remaining != expected {
		t.Errorf("Expected remaining '%s', got '%s'", expected, remaining)
	}
}

func TestParseToolIntents_NoTags(t *testing.T) {
	text := "Just plain text without any tags"
	remaining, intents := ParseToolIntents(text)

	if len(intents) != 0 {
		t.Errorf("Expected 0 intents, got %d", len(intents))
	}

	if remaining != text {
		t.Errorf("Expected remaining to equal original text")
	}
}

func TestParseToolIntents_InvalidTag_MissingClosing(t *testing.T) {
	text := "Text with <websearch><question>Incomplete tag"
	remaining, intents := ParseToolIntents(text)

	if len(intents) != 0 {
		t.Errorf("Expected 0 intents for incomplete tag, got %d", len(intents))
	}

	if remaining != text {
		t.Errorf("Expected remaining to equal original text for invalid tag")
	}
}

func TestParseToolIntents_InvalidTag_MissingQuestion(t *testing.T) {
	text := "Text with <websearch>No question tag</websearch>"
	_, intents := ParseToolIntents(text)

	// Should not extract intent without proper question tag
	if len(intents) != 0 {
		t.Errorf("Expected 0 intents without question tag, got %d", len(intents))
	}
}

func TestParseToolIntents_EmptyQuestion(t *testing.T) {
	text := "Text with <websearch><question></question></websearch>"
	_, intents := ParseToolIntents(text)

	// Empty question should not be extracted
	if len(intents) != 0 {
		t.Errorf("Expected 0 intents for empty question, got %d", len(intents))
	}
}

func TestParseToolIntents_QuestionWithWhitespace(t *testing.T) {
	text := "Text <websearch><question>  What is this?  </question></websearch>"
	_, intents := ParseToolIntents(text)

	if len(intents) != 1 {
		t.Fatalf("Expected 1 intent, got %d", len(intents))
	}

	// Question should be trimmed
	if intents[0].Arguments["question"] != "What is this?" {
		t.Errorf("Expected trimmed question 'What is this?', got '%v'", intents[0].Arguments["question"])
	}
}

func TestParseToolIntents_TagWithSpecialCharacters(t *testing.T) {
	text := `Text <websearch><question>What's "AI" & ML?</question></websearch>`
	_, intents := ParseToolIntents(text)

	if len(intents) != 1 {
		t.Fatalf("Expected 1 intent, got %d", len(intents))
	}

	expected := `What's "AI" & ML?`
	if intents[0].Arguments["question"] != expected {
		t.Errorf("Expected question '%s', got '%v'", expected, intents[0].Arguments["question"])
	}
}

func TestToolIntentBuffer_CompleteTag(t *testing.T) {
	buffer := NewToolIntentBuffer()

	// Feed complete tag at once
	flushable, intents := buffer.Feed("<websearch><question>Test</question></websearch>")

	if len(intents) != 1 {
		t.Fatalf("Expected 1 intent, got %d", len(intents))
	}

	if intents[0].Arguments["question"] != "Test" {
		t.Errorf("Expected question 'Test', got '%v'", intents[0].Arguments["question"])
	}

	if flushable != "" {
		t.Errorf("Expected empty flushable for complete tag, got '%s'", flushable)
	}
}

func TestToolIntentBuffer_PartialTag(t *testing.T) {
	buffer := NewToolIntentBuffer()

	// Feed partial tag
	flushable, intents := buffer.Feed("Some text <webse")

	if len(intents) != 0 {
		t.Errorf("Expected 0 intents for partial tag, got %d", len(intents))
	}

	if flushable != "Some text " {
		t.Errorf("Expected flushable 'Some text ', got '%s'", flushable)
	}

	// Complete the tag
	flushable, intents = buffer.Feed("arch><question>Q</question></websearch>")

	if len(intents) != 1 {
		t.Fatalf("Expected 1 intent after completion, got %d", len(intents))
	}

	if intents[0].Arguments["question"] != "Q" {
		t.Errorf("Expected question 'Q', got '%v'", intents[0].Arguments["question"])
	}
}

func TestToolIntentBuffer_StreamingChunks(t *testing.T) {
	buffer := NewToolIntentBuffer()

	chunks := []string{
		"Before text ",
		"<webs",
		"earch>",
		"<quest",
		"ion>",
		"What ",
		"is this?",
		"</ques",
		"tion>",
		"</websearch>",
		" after",
	}

	var allFlushable string
	var allIntents []ToolIntent

	for i, chunk := range chunks {
		flushable, intents := buffer.Feed(chunk)
		t.Logf("Chunk %d: %q -> flushable: %q, intents: %d", i, chunk, flushable, len(intents))
		allFlushable += flushable
		allIntents = append(allIntents, intents...)
	}

	if len(allIntents) != 1 {
		t.Fatalf("Expected 1 intent from streaming, got %d. All flushable: %q", len(allIntents), allFlushable)
	}

	if allIntents[0].Arguments["question"] != "What is this?" {
		t.Errorf("Expected question 'What is this?', got '%v'", allIntents[0].Arguments["question"])
	}

	// The flushable content includes "Before text " and " after" (tag removed from middle)
	if allFlushable != "Before text  after" {
		t.Errorf("Expected flushable 'Before text  after', got '%s'", allFlushable)
	}
}

func TestToolIntentBuffer_InvalidTag_Recovery(t *testing.T) {
	buffer := NewToolIntentBuffer()

	// Start what looks like a tag
	_, intents := buffer.Feed("<invalid")
	if len(intents) != 0 {
		t.Errorf("Expected 0 intents for partial invalid tag, got %d", len(intents))
	}

	// Add more text that doesn't complete a valid tag
	_, intents = buffer.Feed(" text>")

	// Should eventually flush when no valid tag is found
	_, intents = buffer.Feed(" normal text")

	if len(intents) != 0 {
		t.Errorf("Expected 0 intents for invalid tag, got %d", len(intents))
	}
}

func TestToolIntentBuffer_MaxBuffer(t *testing.T) {
	buffer := NewToolIntentBuffer()

	// Feed a very long string that looks like it might be a tag but never completes
	longString := "<websearch>" + string(make([]byte, 10000))

	flushable, intents := buffer.Feed(longString)

	// Should flush to prevent unbounded growth
	if len(intents) != 0 {
		t.Errorf("Expected 0 intents for malformed long tag, got %d", len(intents))
	}

	// Should have flushed something due to max buffer
	if flushable == "" {
		t.Error("Expected flushable content due to max buffer limit")
	}
}

func TestToolIntentBuffer_MixedContent(t *testing.T) {
	buffer := NewToolIntentBuffer()

	// Mix of normal text and tag
	flushable1, intents1 := buffer.Feed("Normal text before ")
	flushable2, intents2 := buffer.Feed("<websearch><question>Query</question></websearch>")
	flushable3, intents3 := buffer.Feed(" text after")

	allFlushable := flushable1 + flushable2 + flushable3
	allIntents := append(append(intents1, intents2...), intents3...)

	if len(allIntents) != 1 {
		t.Fatalf("Expected 1 intent, got %d", len(allIntents))
	}

	if allIntents[0].Arguments["question"] != "Query" {
		t.Errorf("Expected question 'Query', got '%v'", allIntents[0].Arguments["question"])
	}

	expected := "Normal text before  text after"
	if allFlushable != expected {
		t.Errorf("Expected flushable '%s', got '%s'", expected, allFlushable)
	}
}

func TestToolIntentBuffer_EmptyFeed(t *testing.T) {
	buffer := NewToolIntentBuffer()

	flushable, intents := buffer.Feed("")

	if flushable != "" {
		t.Errorf("Expected empty flushable for empty feed, got '%s'", flushable)
	}

	if len(intents) != 0 {
		t.Errorf("Expected 0 intents for empty feed, got %d", len(intents))
	}
}
