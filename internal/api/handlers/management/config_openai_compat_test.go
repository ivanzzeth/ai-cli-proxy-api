package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestPatchOpenAICompatRemoveCredentialPreservesConcurrentKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("openai-compatibility: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name: "managed", Description: "gateway-provider/v1:managed", BaseURL: "https://upstream.example/v1",
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "old"}, {APIKey: "keep"}, {APIKey: "added-by-ops"}},
	}}}, configPath, nil)
	patch := func(value string) int {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/openai-compatibility", strings.NewReader(value))
		ctx.Request.Header.Set("Content-Type", "application/json")
		h.PatchOpenAICompat(ctx)
		return recorder.Code
	}
	if status := patch(`{"name":"managed","value":{"remove-api-key":"old","expected-description":"gateway-provider/v1:managed"}}`); status != http.StatusOK {
		t.Fatalf("removal returned %d", status)
	}
	keys := h.cfg.OpenAICompatibility[0].APIKeyEntries
	if len(keys) != 2 || keys[0].APIKey != "keep" || keys[1].APIKey != "added-by-ops" {
		t.Fatalf("other keys were overwritten: %+v", keys)
	}
	if status := patch(`{"name":"managed","value":{"remove-api-key":"keep","expected-description":"wrong"}}`); status != http.StatusConflict {
		t.Fatalf("ownership drift accepted: %d", status)
	}
	if status := patch(`{"name":"managed","value":{"remove-api-key":"keep","expected-description":"gateway-provider/v1:managed","api-key-entries":[]}}`); status != http.StatusBadRequest {
		t.Fatalf("mixed patch accepted: %d", status)
	}
}

func TestPatchOpenAICompatAppendCredentialPreservesExistingPool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("openai-compatibility: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	weight := 3
	h := NewHandler(&config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name: "managed", Description: "gateway-provider/v1:managed", BaseURL: "https://upstream.example/v1",
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "old", Weight: &weight, ProxyURL: "https://proxy.example"}, {APIKey: "added-by-ops"}},
	}}}, configPath, nil)
	patch := func(value string) int {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/openai-compatibility", strings.NewReader(value))
		ctx.Request.Header.Set("Content-Type", "application/json")
		h.PatchOpenAICompat(ctx)
		return recorder.Code
	}
	if status := patch(`{"name":"managed","value":{"append-api-key-entries":[{"api-key":"new"}],"expected-description":"gateway-provider/v1:managed"}}`); status != http.StatusOK {
		t.Fatalf("append returned %d", status)
	}
	keys := h.cfg.OpenAICompatibility[0].APIKeyEntries
	if len(keys) != 3 || keys[0].APIKey != "old" || keys[0].Weight == nil || *keys[0].Weight != 3 || keys[0].ProxyURL != "https://proxy.example" || keys[1].APIKey != "added-by-ops" || keys[2].APIKey != "new" {
		t.Fatalf("append lost current pool: %+v", keys)
	}
	if status := patch(`{"name":"managed","value":{"append-api-key-entries":[{"api-key":"new"}],"expected-description":"gateway-provider/v1:managed"}}`); status != http.StatusBadRequest {
		t.Fatalf("duplicate credential accepted: %d", status)
	}
	if status := patch(`{"name":"managed","value":{"append-api-key-entries":[{"api-key":"another"}],"expected-description":"wrong"}}`); status != http.StatusConflict {
		t.Fatalf("unmanaged append accepted: %d", status)
	}
	if status := patch(`{"name":"managed","value":{"append-api-key-entries":[{"api-key":"another"}],"api-key-entries":[],"expected-description":"gateway-provider/v1:managed"}}`); status != http.StatusBadRequest {
		t.Fatalf("mixed append and replace accepted: %d", status)
	}
}

func TestPatchOpenAICompatReplaceChecksManagedOwnership(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("openai-compatibility: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name: "managed", Description: "owned-by-operations", BaseURL: "https://upstream.example/v1",
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "old"}},
	}}}, configPath, nil)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/openai-compatibility", strings.NewReader(`{"name":"managed","value":{"api-key-entries":[{"api-key":"new"}],"base-url":"https://new.example/v1","expected-description":"gateway-provider/v1:managed"}}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PatchOpenAICompat(ctx)
	if recorder.Code != http.StatusConflict || h.cfg.OpenAICompatibility[0].BaseURL != "https://upstream.example/v1" || h.cfg.OpenAICompatibility[0].APIKeyEntries[0].APIKey != "old" {
		t.Fatalf("unmanaged provider was overwritten: status=%d provider=%+v", recorder.Code, h.cfg.OpenAICompatibility[0])
	}
}

func TestPatchOpenAICompatReplaceRejectsStaleCredentialPool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("openai-compatibility: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	weight := 2
	h := NewHandler(&config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name: "managed", Description: "gateway-provider/v1:managed", BaseURL: "https://upstream.example/v1",
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "old", Weight: &weight}, {APIKey: "added-by-ops"}},
	}}}, configPath, nil)
	patch := func(value string) int {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/openai-compatibility", strings.NewReader(value))
		ctx.Request.Header.Set("Content-Type", "application/json")
		h.PatchOpenAICompat(ctx)
		return recorder.Code
	}
	base := `{"name":"managed","value":{"api-key-entries":[{"api-key":"new"}],"expected-description":"gateway-provider/v1:managed","expected-api-key-entries":`
	if status := patch(base + `[{"api-key":"old","weight":2}]}}`); status != http.StatusConflict {
		t.Fatalf("stale pool was replaced: %d", status)
	}
	if status := patch(base + `[{"api-key":"old","weight":1},{"api-key":"added-by-ops"}]}}`); status != http.StatusConflict {
		t.Fatalf("changed weight was replaced: %d", status)
	}
	if keys := h.cfg.OpenAICompatibility[0].APIKeyEntries; len(keys) != 2 || keys[1].APIKey != "added-by-ops" {
		t.Fatalf("stale write lost operations credential: %+v", keys)
	}
	if status := patch(base + `[{"api-key":"old","weight":2},{"api-key":"added-by-ops"}]}}`); status != http.StatusOK {
		t.Fatalf("current pool replacement returned %d", status)
	}
}

func TestPatchOpenAICompatReplaceRejectsStaleEmptyPool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("openai-compatibility: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name: "managed", Description: "gateway-provider/v1:managed", BaseURL: "https://upstream.example/v1",
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "added-by-ops"}},
	}}}, configPath, nil)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/openai-compatibility", strings.NewReader(`{"name":"managed","value":{"api-key-entries":[{"api-key":"new"}],"expected-description":"gateway-provider/v1:managed","expected-api-key-entries":[]}}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PatchOpenAICompat(ctx)
	if recorder.Code != http.StatusConflict || h.cfg.OpenAICompatibility[0].APIKeyEntries[0].APIKey != "added-by-ops" {
		t.Fatalf("stale empty snapshot replaced operations credential: status=%d keys=%+v", recorder.Code, h.cfg.OpenAICompatibility[0].APIKeyEntries)
	}
}

func TestPatchOpenAICompatRejectsSharedModelPrefix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("openai-compatibility: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{OpenAICompatibility: []config.OpenAICompatibility{
		{Name: "managed", Prefix: "gateway-owned", BaseURL: "https://managed.example/v1"},
		{Name: "operations", Prefix: "ops-owned", BaseURL: "https://ops.example/v1"},
	}}, configPath, nil)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/openai-compatibility", strings.NewReader(`{"name":"operations","value":{"prefix":"gateway-owned"}}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PatchOpenAICompat(ctx)
	if recorder.Code != http.StatusConflict || h.cfg.OpenAICompatibility[1].Prefix != "ops-owned" {
		t.Fatalf("shared model prefix was accepted: status=%d providers=%+v", recorder.Code, h.cfg.OpenAICompatibility)
	}
}

func TestPutOpenAICompatRejectsSharedModelPrefix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("openai-compatibility: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "managed", Prefix: "gateway-owned", BaseURL: "https://managed.example/v1"}}}, configPath, nil)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/openai-compatibility", strings.NewReader(`[{"name":"managed","prefix":"gateway-owned","base-url":"https://managed.example/v1"},{"name":"operations","prefix":"gateway-owned","base-url":"https://ops.example/v1"}]`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PutOpenAICompat(ctx)
	if recorder.Code != http.StatusConflict || len(h.cfg.OpenAICompatibility) != 1 {
		t.Fatalf("shared model prefix was accepted: status=%d providers=%+v", recorder.Code, h.cfg.OpenAICompatibility)
	}
}

func TestGetOpenAICompatIncludesDisableCooling(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	requestRetry := 0
	disableCooling := true
	h := NewHandlerWithoutConfigFilePath(&config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:    "Mimo CN",
				BaseURL: "https://token-plan-cn.xiaomimimo.com/v1",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "test-key"},
				},
				Models: []config.OpenAICompatibilityModel{
					{Name: "mimo-v2.5", Alias: ""},
				},
				SupportPromptCacheKey: true,
				DisableCooling:        &disableCooling,
				RequestRetry:          &requestRetry,
			},
		},
	}, nil)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/openai-compatibility", nil)
	h.GetOpenAICompat(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var body struct {
		OpenAICompatibility []struct {
			SupportPromptCacheKey *bool `json:"support-prompt-cache-key"`
			DisableCooling        *bool `json:"disable-cooling"`
			RequestRetry          *int  `json:"request-retry"`
		} `json:"openai-compatibility"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(body.OpenAICompatibility) != 1 {
		t.Fatalf("expected 1 openai-compatibility entry, got %d", len(body.OpenAICompatibility))
	}
	if body.OpenAICompatibility[0].SupportPromptCacheKey == nil || !*body.OpenAICompatibility[0].SupportPromptCacheKey {
		t.Fatalf("expected support-prompt-cache-key to be present and true, got %#v", body.OpenAICompatibility[0].SupportPromptCacheKey)
	}
	if body.OpenAICompatibility[0].DisableCooling == nil || !*body.OpenAICompatibility[0].DisableCooling {
		t.Fatalf("expected disable-cooling to be present and true, got %#v", body.OpenAICompatibility[0].DisableCooling)
	}
	if body.OpenAICompatibility[0].RequestRetry == nil || *body.OpenAICompatibility[0].RequestRetry != 0 {
		t.Fatalf("expected request-retry to be present and 0, got %#v", body.OpenAICompatibility[0].RequestRetry)
	}
}
