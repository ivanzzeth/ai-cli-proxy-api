package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestToClaude_SpecConversationShape(t *testing.T) {
	raw := []byte(`{
		"model":"claude-sonnet-4-5",
		"instructions":"be concise",
		"reasoning":{"effort":"auto"},
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},
			{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"README.md\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"file content"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}
		],
		"tools":[
			{"type":"function","name":"read_file","description":"read file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}
		],
		"tool_choice":"required",
		"max_output_tokens":2048
	}`)

	out := ConvertOpenAIResponsesRequestToClaude("claude-sonnet-4-5", raw, false)

	if got := gjson.GetBytes(out, "model").String(); got != "claude-sonnet-4-5" {
		t.Fatalf("model = %q, want %q", got, "claude-sonnet-4-5")
	}
	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 2048 {
		t.Fatalf("max_tokens = %d, want %d", got, 2048)
	}
	if got := gjson.GetBytes(out, "messages.0.role").String(); got != "user" {
		t.Fatalf("messages[0].role = %q, want %q", got, "user")
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "be concise" {
		t.Fatalf("messages[0].content = %q, want %q", got, "be concise")
	}
	if got := gjson.GetBytes(out, "messages.2.content.0.type").String(); got != "tool_use" {
		t.Fatalf("messages[2].content[0].type = %q, want %q", got, "tool_use")
	}
	if got := gjson.GetBytes(out, "messages.3.content.0.type").String(); got != "tool_result" {
		t.Fatalf("messages[3].content[0].type = %q, want %q", got, "tool_result")
	}
	if got := gjson.GetBytes(out, "tools.0.input_schema.type").String(); got != "object" {
		t.Fatalf("tools[0].input_schema.type = %q, want %q", got, "object")
	}
	if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "any" {
		t.Fatalf("tool_choice.type = %q, want %q", got, "any")
	}
}

