package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_PreservesReasoningContent(t *testing.T) {
	raw := []byte(`{
		"model":"deepseek-v4-flash",
		"reasoning":{"effort":"high"},
		"input":[
			{
				"type":"message",
				"role":"assistant",
				"reasoning_content":"keep-this-reasoning",
				"content":[{"type":"output_text","text":"tool plan"}]
			}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("deepseek-v4-flash", raw, false)
	got := gjson.GetBytes(out, "messages.0.reasoning_content")
	if !got.Exists() {
		t.Fatalf("messages.0.reasoning_content should exist")
	}
	if got.String() != "keep-this-reasoning" {
		t.Fatalf("messages.0.reasoning_content = %q, want %q", got.String(), "keep-this-reasoning")
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_FunctionCallReasoningContent(t *testing.T) {
	raw := []byte(`{
		"model":"deepseek-v4-flash",
		"reasoning":{"effort":"high"},
		"input":[
			{
				"type":"function_call",
				"call_id":"call_1",
				"name":"read_file",
				"arguments":"{}",
				"reasoning_content":"tool-call-reasoning"
			}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("deepseek-v4-flash", raw, false)
	got := gjson.GetBytes(out, "messages.0.reasoning_content")
	if !got.Exists() {
		t.Fatalf("messages.0.reasoning_content should exist")
	}
	if got.String() != "tool-call-reasoning" {
		t.Fatalf("messages.0.reasoning_content = %q, want %q", got.String(), "tool-call-reasoning")
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_ReasoningItemToAssistantReasoningContent(t *testing.T) {
	raw := []byte(`{
		"model":"deepseek-v4-flash",
		"reasoning":{"effort":"high"},
		"input":[
			{
				"type":"reasoning",
				"summary":[{"type":"summary_text","text":"reasoning-summary"}]
			}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("deepseek-v4-flash", raw, false)
	if gotRole := gjson.GetBytes(out, "messages.0.role").String(); gotRole != "assistant" {
		t.Fatalf("messages.0.role = %q, want %q", gotRole, "assistant")
	}
	got := gjson.GetBytes(out, "messages.0.reasoning_content")
	if !got.Exists() {
		t.Fatalf("messages.0.reasoning_content should exist")
	}
	if got.String() != "reasoning-summary" {
		t.Fatalf("messages.0.reasoning_content = %q, want %q", got.String(), "reasoning-summary")
	}
}

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_SpecConversationShape(t *testing.T) {
	raw := []byte(`{
		"model":"deepseek-v4-flash",
		"instructions":"you are helpful",
		"reasoning":{"effort":"high"},
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},
			{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"README.md\"}","reasoning_content":"need to read"},
			{"type":"function_call_output","call_id":"call_1","output":"file content"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"post tool reasoning"}]},
			{"type":"message","role":"assistant","reasoning_content":"final-reasoning","content":[{"type":"output_text","text":"done"}]}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("deepseek-v4-flash", raw, false)

	if got := gjson.GetBytes(out, "model").String(); got != "deepseek-v4-flash" {
		t.Fatalf("model = %q, want %q", got, "deepseek-v4-flash")
	}
	if got := gjson.GetBytes(out, "reasoning_effort").String(); got != "high" {
		t.Fatalf("reasoning_effort = %q, want %q", got, "high")
	}

	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 6 {
		t.Fatalf("messages length = %d, want 6; messages=%s", len(messages), gjson.GetBytes(out, "messages").Raw)
	}

	if got := messages[0].Get("role").String(); got != "system" {
		t.Fatalf("messages[0].role = %q, want %q", got, "system")
	}
	if got := messages[0].Get("content").String(); got != "you are helpful" {
		t.Fatalf("messages[0].content = %q, want %q", got, "you are helpful")
	}
	if got := messages[2].Get("reasoning_content").String(); got != "need to read" {
		t.Fatalf("messages[2].reasoning_content = %q, want %q", got, "need to read")
	}
	if got := messages[3].Get("role").String(); got != "tool" {
		t.Fatalf("messages[3].role = %q, want %q", got, "tool")
	}
	if got := messages[4].Get("reasoning_content").String(); got != "post tool reasoning" {
		t.Fatalf("messages[4].reasoning_content = %q, want %q", got, "post tool reasoning")
	}
	if got := messages[5].Get("reasoning_content").String(); got != "final-reasoning" {
		t.Fatalf("messages[5].reasoning_content = %q, want %q", got, "final-reasoning")
	}
}
