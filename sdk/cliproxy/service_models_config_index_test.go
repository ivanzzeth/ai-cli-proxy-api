package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOpenAICompatibilityRegistrationCacheUsesConfigIndex(t *testing.T) {
	service := &Service{cfg: &config.Config{OpenAICompatibility: []config.OpenAICompatibility{
		{Name: "shared", Models: []config.OpenAICompatibilityModel{{Name: "first"}}},
		{Name: "shared", Models: []config.OpenAICompatibilityModel{{Name: "second"}}},
	}}}
	cache := service.newOpenAICompatibilityRegistrationCache()
	auth := &coreauth.Auth{Attributes: map[string]string{
		coreauth.AttributeSource:      "config:shared[token-1]",
		coreauth.AttributeConfigIndex: "1",
	}}
	entry, ok := cache.lookup(auth, "shared")
	if !ok || entry == nil || len(entry.models) != 1 || entry.models[0].ID != "second" {
		t.Fatalf("cached config entry = %+v, want second model", entry)
	}
}

func TestResolveConfigClaudeKeyUsesConfigIndex(t *testing.T) {
	service := &Service{cfg: &config.Config{ClaudeKey: []config.ClaudeKey{
		{APIKey: "shared-key", Models: []config.ClaudeModel{{Name: "first"}}},
		{APIKey: "shared-key", Models: []config.ClaudeModel{{Name: "second"}}},
	}}}
	auth := &coreauth.Auth{Attributes: map[string]string{
		coreauth.AttributeAPIKey:      "shared-key",
		coreauth.AttributeSource:      "config:claude[token-1]",
		coreauth.AttributeConfigIndex: "1",
	}}

	entry := service.resolveConfigClaudeKey(auth)
	if entry == nil || len(entry.Models) != 1 || entry.Models[0].Name != "second" {
		t.Fatalf("resolved config entry = %+v, want second entry", entry)
	}
}

func TestPlanSpeechRegistrationHasOnlyExplicitAudioCapability(t *testing.T) {
	models := buildOpenAICompatibilityConfigModels(&config.OpenAICompatibility{Name: "plan", Models: []config.OpenAICompatibilityModel{{Name: "text-model"}, {Name: "doubao-seedream-5.0-flash", Image: true, ImageProfile: "ark-agent-plan-seedream-5flash-v1"}, {Name: "doubao-seed-tts-2.0", SpeechProfile: "ark-agent-plan-tts-2-v1", UpstreamProtocol: "volcengine-tts-v3-http"}}})
	if len(models) != 3 {
		t.Fatalf("models=%d", len(models))
	}
	if models[0].AudioSpeech != nil || models[1].AudioSpeech != nil {
		t.Fatal("profile invented on existing models")
	}
	m := models[2]
	if m.AudioSpeech == nil || m.Thinking != nil || len(m.SupportedOutputModalities) != 1 || m.SupportedOutputModalities[0] != "audio" || m.AudioSpeech.SupportedEndpoints[0] != "/v1/audio/speech" {
		t.Fatalf("speech registration=%#v", m)
	}
}
