package registry

import (
	"testing"
	"time"
)

func TestGetAvailableModelsReturnsClonedSnapshots(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "OpenAI", []*ModelInfo{{ID: "m1", OwnedBy: "team-a", DisplayName: "Model One"}})

	first := r.GetAvailableModels("openai")
	if len(first) != 1 {
		t.Fatalf("expected 1 model, got %d", len(first))
	}
	first[0]["id"] = "mutated"
	first[0]["display_name"] = "Mutated"

	second := r.GetAvailableModels("openai")
	if got := second[0]["id"]; got != "m1" {
		t.Fatalf("expected cached snapshot to stay isolated, got id %v", got)
	}
	if got := second[0]["display_name"]; got != "Model One" {
		t.Fatalf("expected cached snapshot to stay isolated, got display_name %v", got)
	}
}

func TestGetAvailableModelsClaudeIncludesTokenLimits(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "Claude", []*ModelInfo{
		{ID: "claude-sonnet-4-6", OwnedBy: "anthropic", Type: "claude", Created: 1771372800, ContextLength: 200000, MaxCompletionTokens: 64000},
		{ID: "claude-no-limits", OwnedBy: "anthropic", Type: "claude"},
	})

	models := r.GetAvailableModels("claude")
	byID := make(map[string]map[string]any, len(models))
	for _, m := range models {
		id, _ := m["id"].(string)
		byID[id] = m
	}

	withLimits, ok := byID["claude-sonnet-4-6"]
	if !ok {
		t.Fatalf("expected claude-sonnet-4-6 in available models, got %v", byID)
	}
	if got := withLimits["max_input_tokens"]; got != 200000 {
		t.Fatalf("expected max_input_tokens 200000, got %v", got)
	}
	if got := withLimits["max_tokens"]; got != 64000 {
		t.Fatalf("expected max_tokens 64000, got %v", got)
	}
	if got := withLimits["created_at"]; got != "2026-02-18T00:00:00Z" {
		t.Fatalf("expected created_at as RFC 3339 string, got %v", got)
	}

	withDefaults, ok := byID["claude-no-limits"]
	if !ok {
		t.Fatalf("expected claude-no-limits in available models, got %v", byID)
	}
	if got := withDefaults["max_input_tokens"]; got != DefaultClaudeMaxInputTokens {
		t.Fatalf("expected fallback max_input_tokens %d, got %v", DefaultClaudeMaxInputTokens, got)
	}
	if got := withDefaults["max_tokens"]; got != DefaultClaudeMaxOutputTokens {
		t.Fatalf("expected fallback max_tokens %d, got %v", DefaultClaudeMaxOutputTokens, got)
	}
	if got := withDefaults["display_name"]; got != "claude-no-limits" {
		t.Fatalf("expected display_name to fall back to id, got %v", got)
	}
	if got := withDefaults["type"]; got != "model" {
		t.Fatalf("expected type to default to model, got %v", got)
	}
}

func TestGetAvailableModelsInvalidatesCacheOnRegistryChanges(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("client-1", "OpenAI", []*ModelInfo{{ID: "m1", OwnedBy: "team-a", DisplayName: "Model One"}})

	models := r.GetAvailableModels("openai")
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if got := models[0]["display_name"]; got != "Model One" {
		t.Fatalf("expected initial display_name Model One, got %v", got)
	}

	r.RegisterClient("client-1", "OpenAI", []*ModelInfo{{ID: "m1", OwnedBy: "team-a", DisplayName: "Model One Updated"}})
	models = r.GetAvailableModels("openai")
	if got := models[0]["display_name"]; got != "Model One Updated" {
		t.Fatalf("expected updated display_name after cache invalidation, got %v", got)
	}

	r.SuspendClientModel("client-1", "m1", "manual")
	models = r.GetAvailableModels("openai")
	if len(models) != 0 {
		t.Fatalf("expected no available models after suspension, got %d", len(models))
	}

	r.ResumeClientModel("client-1", "m1")
	models = r.GetAvailableModels("openai")
	if len(models) != 1 {
		t.Fatalf("expected model to reappear after resume, got %d", len(models))
	}
}

func TestTemporarySuspensionExpiresAcrossRegistryReadSurfaces(t *testing.T) {
	r := newTestModelRegistry()
	const id, model, provider = "deadline-client", "deadline-model", "deadline-provider"
	r.RegisterClient(id, provider, []*ModelInfo{{ID: model}})
	_, epoch := r.GetModelsAndEpochForClient(id)
	until := time.Now().Add(500 * time.Millisecond)
	if !r.ApplyClientModelProjections(id, epoch, 1, []ClientModelProjection{{ModelID: model, Suspended: true, SuspendReason: "transient_error", SuspendUntil: until}}) {
		t.Fatal("projection rejected")
	}
	if len(r.GetAvailableModels("openai")) != 0 {
		t.Fatal("active cooldown must hide model")
	}
	if !r.availableModelsCache["openai"].expiresAt.Equal(until) {
		t.Fatal("hidden cache must carry exact auth deadline")
	}
	time.Sleep(time.Until(until) + 10*time.Millisecond)
	// The only trigger is a read; no config change, new registration or result.
	if len(r.GetAvailableModels("openai")) != 1 || len(r.GetAvailableModelInfos()) != 1 || len(r.GetAvailableModelsByProvider(provider)) != 1 || r.GetModelCount(model) != 1 || r.IsModelSuspendedForClient(id, model) {
		t.Fatal("read surfaces did not recover consistently")
	}
}

func TestSuspensionDeadlineChangesInvalidateSameReasonCache(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("c", "p", []*ModelInfo{{ID: "m"}})
	_, epoch := r.GetModelsAndEpochForClient("c")
	first, extended := time.Now().Add(time.Minute), time.Now().Add(2*time.Minute)
	project := func(generation uint64, until time.Time) bool {
		return r.ApplyClientModelProjections("c", epoch, generation, []ClientModelProjection{{ModelID: "m", Suspended: true, SuspendReason: "transient_error", SuspendUntil: until}})
	}
	if !project(1, first) {
		t.Fatal("initial projection rejected")
	}
	r.GetAvailableModels("openai")
	if !project(2, extended) {
		t.Fatal("extended projection rejected")
	}
	r.GetAvailableModels("openai")
	if !r.availableModelsCache["openai"].expiresAt.Equal(extended) {
		t.Fatal("same reason deadline extension retained stale cache")
	}
	if project(1, time.Time{}) {
		t.Fatal("stale generation accepted")
	}
	if !r.models["m"].SuspensionDeadlines["c"].Equal(extended) {
		t.Fatal("stale write changed deadline")
	}
	if !project(3, first) {
		t.Fatal("shortened projection rejected")
	}
	r.GetAvailableModels("openai")
	if !r.availableModelsCache["openai"].expiresAt.Equal(first) {
		t.Fatal("same reason deadline shortening retained stale cache")
	}
}

func TestSuspensionDeadlineLifecycleAndExistingPolicies(t *testing.T) {
	for _, action := range []string{"resume", "reregister", "unregister", "legacy_permanent"} {
		t.Run(action, func(t *testing.T) {
			r := newTestModelRegistry()
			r.RegisterClient("c", "p", []*ModelInfo{{ID: "m"}})
			_, epoch := r.GetModelsAndEpochForClient("c")
			if !r.ApplyClientModelProjections("c", epoch, 1, []ClientModelProjection{{ModelID: "m", Suspended: true, SuspendReason: "transient_error", SuspendUntil: time.Now().Add(time.Minute)}}) {
				t.Fatal("projection rejected")
			}
			switch action {
			case "resume":
				r.ResumeClientModel("c", "m")
			case "reregister":
				r.RegisterClient("c", "p", []*ModelInfo{{ID: "m"}})
			case "unregister":
				r.UnregisterClient("c")
				r.RegisterClient("c", "p", []*ModelInfo{{ID: "m"}})
			case "legacy_permanent":
				r.SuspendClientModel("c", "m", "disabled")
			}
			if _, remains := r.models["m"].SuspensionDeadlines["c"]; remains {
				t.Fatal("deadline leaked through lifecycle")
			}
			_, suspended, _ := activeModelSuspension(r.models["m"], "c", time.Now().Add(2*time.Minute))
			if suspended != (action == "legacy_permanent") {
				t.Fatal("permanent restriction or cleanup changed")
			}
			if action == "reregister" || action == "unregister" {
				if r.ApplyClientModelProjections("c", epoch, 99, []ClientModelProjection{{ModelID: "m", Suspended: true, SuspendUntil: time.Now().Add(time.Minute)}}) {
					t.Fatal("old epoch resurrected a deadline")
				}
			}
		})
	}
	// Preserve native quota policy: catalog visibility is not scheduling permission.
	r := newTestModelRegistry()
	r.RegisterClient("quota", "p", []*ModelInfo{{ID: "m"}})
	_, epoch := r.GetModelsAndEpochForClient("quota")
	r.ApplyClientModelProjections("quota", epoch, 1, []ClientModelProjection{{ModelID: "m", Suspended: true, SuspendReason: "quota", SuspendUntil: time.Now().Add(-time.Minute), QuotaExceeded: true}})
	if len(r.GetAvailableModels("openai")) != 1 || r.GetModelCount("m") != 0 {
		t.Fatal("existing quota catalog/count semantics changed")
	}
	// At least one ready credential keeps a model visible.
	r.RegisterClient("ready", "p", []*ModelInfo{{ID: "m"}})
	r.SuspendClientModel("quota", "m", "disabled")
	if len(r.GetAvailableModels("openai")) != 1 || r.GetModelCount("m") != 1 {
		t.Fatal("ready sibling must keep model visible")
	}
}

func TestPlanSpeechCapabilityCacheSnapshotIsIndependent(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("speech-A", "openai", []*ModelInfo{{ID: "source-A/doubao-seed-tts-2.0", UpstreamProtocol: "volcengine-tts-v3-http", AudioSpeech: ArkPlanSpeechProfile("ark-agent-plan-tts-2-v1")}})
	first := r.GetAvailableModels("openai")
	if len(first) != 1 {
		t.Fatal("missing speech model")
	}
	first[0]["audio_speech"].(*AudioSpeechSupport).Voices[0] = "mutated"
	second := r.GetAvailableModels("openai")
	if second[0]["audio_speech"].(*AudioSpeechSupport).Voices[0] != "zh_female_vv_uranus_bigtts" {
		t.Fatal("cached speech metadata leaked mutations")
	}
	if ArkPlanSpeechProfile("unknown") != nil {
		t.Fatal("unknown profile acquired capability")
	}
}

func TestXAISpeechRegistryProjectionIsCredentialScopedAndCloned(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("speech-A", "xai", []*ModelInfo{xaiBuiltinSpeechModelInfo()})
	r.RegisterClient("text-B", "xai", []*ModelInfo{{ID: "other-model", OwnedBy: "xai"}})
	scoped := r.GetModelsForClient("text-B")
	if len(scoped) != 1 || scoped[0].AudioSpeech != nil {
		t.Fatal("speech metadata leaked to a different credential")
	}
	first := r.GetAvailableModels("openai")
	var speech *AudioSpeechSupport
	for _, item := range first {
		if item["id"] == "grok-tts" {
			speech, _ = item["audio_speech"].(*AudioSpeechSupport)
		}
	}
	if speech == nil {
		t.Fatal("public speech metadata missing")
	}
	speech.Formats[0] = "mutated"
	speech.UsageSource = "forged"
	for _, item := range r.GetAvailableModels("openai") {
		if item["id"] == "grok-tts" {
			copy := item["audio_speech"].(*AudioSpeechSupport)
			if copy.Formats[0] != "mp3" || copy.UsageSource != "request_estimated_characters" {
				t.Fatal("cached model metadata shared mutable state")
			}
		}
	}
}
