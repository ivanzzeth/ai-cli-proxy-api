package claude

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
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

func TestConvertClaudeRequestToOpenAI_ClampsMaxTokensByModelCapability(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("test-openai-compat", "openai-compatibility", []*registry.ModelInfo{
		{
			ID:                  "bailian/qwen-max",
			UserDefined:         true,
			MaxCompletionTokens: 8192,
		},
	})
	defer reg.UnregisterClient("test-openai-compat")

	input := []byte(`{
		"model":"bailian/qwen-max",
		"max_tokens":32000,
		"messages":[{"role":"user","content":"hi"}]
	}`)

	got := ConvertClaudeRequestToOpenAI("bailian/qwen-max", input, false)
	gotMaxTokens := gjson.GetBytes(got, "max_tokens").Int()
	if gotMaxTokens != 8192 {
		t.Fatalf("max_tokens should be clamped to model limit 8192, got %d payload=%s", gotMaxTokens, string(got))
	}
}

func TestConvertClaudeRequestToOpenAI_ClampsMaxTokensByUnprefixedRegistryMatch(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("test-openai-compat-unprefixed", "openai-compatibility", []*registry.ModelInfo{
		{
			ID:                  "qwen-max",
			UserDefined:         true,
			MaxCompletionTokens: 8192,
		},
	})
	defer reg.UnregisterClient("test-openai-compat-unprefixed")

	input := []byte(`{
		"model":"bailian/qwen-max",
		"max_tokens":32000,
		"messages":[{"role":"user","content":"hi"}]
	}`)

	got := ConvertClaudeRequestToOpenAI("bailian/qwen-max", input, false)
	gotMaxTokens := gjson.GetBytes(got, "max_tokens").Int()
	if gotMaxTokens != 8192 {
		t.Fatalf("max_tokens should be clamped via unprefixed model capability to 8192, got %d payload=%s", gotMaxTokens, string(got))
	}
}
