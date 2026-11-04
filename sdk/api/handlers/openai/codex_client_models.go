package openai

import (
	codexmodels "github.com/router-for-me/CLIProxyAPI/v8/internal/client/codex/models"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func (h *OpenAIAPIHandler) codexClientModelsResponse(clientVersion ...string) map[string]any {
	version := ""
	if len(clientVersion) > 0 {
		version = clientVersion[0]
	}
	optimizeMultiAgentV2 := h != nil && h.Cfg != nil && h.Cfg.CodexOptimizeMultiAgentV2
	return codexmodels.BuildResponseForClient(withoutAudioOnlyModels(h.Models()), registry.GetGlobalRegistry().GetModelProviders, optimizeMultiAgentV2, version)
}

// CodexClientModelsResponse builds a Codex client model response.
func CodexClientModelsResponse(models []map[string]any) map[string]any {
	return codexmodels.BuildResponse(withoutAudioOnlyModels(models), nil, false)
}

// CodexClientModelsResponseWithMultiAgentV2 builds a Codex client model response
// and advertises multi-agent v2 for synthesized models when enabled.
func CodexClientModelsResponseWithMultiAgentV2(models []map[string]any, enabled bool) map[string]any {
	return codexmodels.BuildResponse(withoutAudioOnlyModels(models), nil, enabled)
}

// CodexClientModelsResponseForClient builds a Codex client model response
// tailored for a specific client version.
func CodexClientModelsResponseForClient(models []map[string]any, clientVersion string, enabled bool) map[string]any {
	return codexmodels.BuildResponseForClient(withoutAudioOnlyModels(models), nil, enabled, clientVersion)
}

// Codex's manifest describes text execution, not audio-only endpoints.
func withoutAudioOnlyModels(models []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(models))
	for _, model := range models {
		if model["audio_speech"] == nil {
			out = append(out, model)
		}
	}
	return out
}
