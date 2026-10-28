package management

import (
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPostGeminiKeyPreservesExistingAndRejectsDuplicate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("port: 8317\napi-keys: [client]\ngemini-api-key:\n  - api-key: existing\n    base-url: https://existing.example\n    models: [{name: old}]\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.POST("/v0/management/gemini-api-key", h.PostGeminiKey)
	body := "{\"description\":\"managed:new\",\"api-key\":\"new-key\",\"base-url\":\"https://new.example\",\"models\":[{\"name\":\"new-model\"}]}"
	for _, expected := range []int{http.StatusOK, http.StatusConflict} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v0/management/gemini-api-key", strings.NewReader(body)))
		if rec.Code != expected {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	if len(h.cfg.GeminiKey) != 2 || h.cfg.GeminiKey[0].APIKey != "existing" || h.cfg.GeminiKey[1].Description != "managed:new" {
		t.Fatalf("unexpected credentials: %#v", h.cfg.GeminiKey)
	}
}

func TestPostGeminiKeyCreateOnlyRejectsSecondOwnerCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("gemini-api-key: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{}, path, nil)
	post := func(key, suffix string) int {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/gemini-api-key"+suffix, strings.NewReader(`{"description":"gateway-provider/v1:owner","api-key":"`+key+`","base-url":"https://example.test","models":[{"name":"model"}]}`))
		h.PostGeminiKey(ctx)
		return recorder.Code
	}
	if code := post("first", "?create-only=true"); code != http.StatusOK {
		t.Fatalf("first create: %d", code)
	}
	if code := post("second", "?create-only=true"); code != http.StatusConflict || len(h.cfg.GeminiKey) != 1 {
		t.Fatalf("duplicate create: status=%d count=%d", code, len(h.cfg.GeminiKey))
	}
	if code := post("second", ""); code != http.StatusOK || len(h.cfg.GeminiKey) != 2 {
		t.Fatalf("credential pool append: status=%d count=%d", code, len(h.cfg.GeminiKey))
	}
}

func TestPostGeminiKeyRejectsForeignModelAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("gemini-api-key: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{GeminiKey: []config.GeminiKey{{Description: "operator", APIKey: "operator-key", BaseURL: "https://operator.example", Models: []config.GeminiModel{{Name: "operator-model", Alias: "shared"}}}}}
	handler := NewHandler(cfg, path, nil)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/v0/management/gemini-api-key", strings.NewReader(`{"description":"gateway-provider/v1:managed","api-key":"new-key","base-url":"https://provider.example","models":[{"name":"model","alias":"shared"}]}`))
	context.Request.Header.Set("Content-Type", "application/json")
	handler.PostGeminiKey(context)
	if recorder.Code != http.StatusConflict || len(cfg.GeminiKey) != 1 {
		t.Fatalf("foreign model alias accepted: status=%d credentials=%d", recorder.Code, len(cfg.GeminiKey))
	}
}

func TestPatchGeminiKeyUpdatesModelsWithoutReplacingOtherCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("port: 8317\napi-keys: [client]\ngemini-api-key:\n  - api-key: managed\n    base-url: https://managed.example\n    models: [{name: original}]\n  - api-key: operator\n    base-url: https://operator.example\n    models: [{name: operator-model}]\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.PATCH("/v0/management/gemini-api-key", h.PatchGeminiKey)
	body := `{"match":"managed","value":{"models":[{"name":"new-model"}]}}`
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/gemini-api-key", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if len(cfg.GeminiKey) != 2 || len(cfg.GeminiKey[0].Models) != 1 || cfg.GeminiKey[0].Models[0].Name != "new-model" || cfg.GeminiKey[1].Models[0].Name != "operator-model" {
		t.Fatalf("unexpected credentials: %#v", cfg.GeminiKey)
	}
}
