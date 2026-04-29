package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeDeepSeekReasoningContentForThinking(t *testing.T) {
	t.Run("inject empty reasoning_content for assistant with tool calls", func(t *testing.T) {
		in := []byte(`{
			"model":"deepseek-v4-pro",
			"reasoning_effort":"high",
			"messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":"working","tool_calls":[{"id":"call_1","type":"function","function":{"name":"ls","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"ok"}
			]
		}`)
		out := normalizeDeepSeekReasoningContentForThinking(in)

		got := gjson.GetBytes(out, "messages.1.reasoning_content")
		if !got.Exists() {
			t.Fatalf("messages.1.reasoning_content should exist")
		}
		if got.String() != "" {
			t.Fatalf("messages.1.reasoning_content = %q, want empty string", got.String())
		}
	})

	t.Run("keep existing reasoning_content untouched", func(t *testing.T) {
		in := []byte(`{
			"reasoning_effort":"medium",
			"messages":[
				{"role":"assistant","content":"done","reasoning_content":"step1"}
			]
		}`)
		out := normalizeDeepSeekReasoningContentForThinking(in)
		got := gjson.GetBytes(out, "messages.0.reasoning_content").String()
		if got != "step1" {
			t.Fatalf("messages.0.reasoning_content = %q, want %q", got, "step1")
		}
	})

	t.Run("inject when conversation already carries assistant reasoning_content", func(t *testing.T) {
		in := []byte(`{
			"messages":[
				{"role":"assistant","content":"prior","reasoning_content":"trace"},
				{"role":"assistant","content":"done","tool_calls":[{"id":"call_1","type":"function","function":{"name":"ls","arguments":"{}"}}]}
			]
		}`)
		out := normalizeDeepSeekReasoningContentForThinking(in)
		if !gjson.GetBytes(out, "messages.1.reasoning_content").Exists() {
			t.Fatalf("messages.1.reasoning_content should be injected when assistant reasoning trace exists")
		}
	})

	t.Run("no change when neither reasoning_effort nor assistant reasoning trace exists", func(t *testing.T) {
		in := []byte(`{
			"model":"gpt-4.1",
			"messages":[
				{"role":"assistant","content":"done","tool_calls":[{"id":"call_1","type":"function","function":{"name":"ls","arguments":"{}"}}]}
			]
		}`)
		out := normalizeDeepSeekReasoningContentForThinking(in)
		if gjson.GetBytes(out, "messages.0.reasoning_content").Exists() {
			t.Fatalf("messages.0.reasoning_content should not be injected without thinking signals")
		}
	})
}
