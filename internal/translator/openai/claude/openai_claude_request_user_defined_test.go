package claude

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToOpenAI_UserDefinedModelSkipsReasoningEffortAutoMapping(t *testing.T) {
	input := []byte(`{
		"model":"bailian/qwen-max",
		"max_tokens":32000,
		"thinking":{"type":"enabled","budget_tokens":32768},
		"messages":[{"role":"user","content":"hi"}]
	}`)

	got := ConvertClaudeRequestToOpenAI("bailian/qwen-max", input, false)

	if gjson.GetBytes(got, "reasoning_effort").Exists() {
		t.Fatalf("reasoning_effort should not be auto-mapped for user-defined model, got: %s", string(got))
	}
}

