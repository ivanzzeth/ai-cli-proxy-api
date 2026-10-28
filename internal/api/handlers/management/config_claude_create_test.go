package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestPostClaudeKeyPreservesExistingAndRejectsDuplicate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("port: 8317\napi-keys: [client]\nclaude-api-key:\n  - api-key: existing\n    base-url: https://existing.example\n    models: [{name: old}]\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.POST("/v0/management/claude-api-key", h.PostClaudeKey)
	body := "{\"api-key\":\"new-key\",\"base-url\":\"https://new.example\",\"models\":[{\"name\":\"new-model\"}]}"
	for _, expected := range []int{http.StatusOK, http.StatusConflict} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v0/management/claude-api-key", strings.NewReader(body)))
		if recorder.Code != expected {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
	if len(h.cfg.ClaudeKey) != 2 || h.cfg.ClaudeKey[0].APIKey != "existing" || h.cfg.ClaudeKey[1].APIKey != "new-key" {
		t.Fatalf("unexpected credentials: %#v", h.cfg.ClaudeKey)
	}
}

func TestPostClaudeKeyCreateOnlyRejectsSecondOwnerCredential(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("claude-api-key: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{}, path, nil)
	post := func(key, suffix string) int {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/claude-api-key"+suffix, strings.NewReader(`{"description":"gateway-provider/v1:owner","prefix":"owner","api-key":"`+key+`","base-url":"https://example.test","models":[{"name":"model"}]}`))
		h.PostClaudeKey(ctx)
		return recorder.Code
	}
	if code := post("first", "?create-only=true"); code != http.StatusOK {
		t.Fatalf("first create: %d", code)
	}
	if code := post("second", "?create-only=true"); code != http.StatusConflict || len(h.cfg.ClaudeKey) != 1 {
		t.Fatalf("duplicate create: status=%d count=%d", code, len(h.cfg.ClaudeKey))
	}
	if code := post("second", ""); code != http.StatusOK || len(h.cfg.ClaudeKey) != 2 {
		t.Fatalf("credential pool append: status=%d count=%d", code, len(h.cfg.ClaudeKey))
	}
}

func TestPostClaudeKeyRejectsForeignManagedPrefixWithoutBlockingSameOwnerPool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("claude-api-key: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{ClaudeKey: []config.ClaudeKey{{Description: "operations", Prefix: "gateway-owned", APIKey: "ops", BaseURL: "https://ops.example"}}}, path, nil)
	post := func(body string) int {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/claude-api-key", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		h.PostClaudeKey(ctx)
		return recorder.Code
	}
	if status := post(`{"description":"gateway-provider/v1:managed","prefix":"gateway-owned","api-key":"new","base-url":"https://new.example","models":[{"name":"model"}]}`); status != http.StatusConflict || len(h.cfg.ClaudeKey) != 1 {
		t.Fatalf("foreign managed prefix accepted: %d", status)
	}
	h.cfg.ClaudeKey[0].Description = "gateway-provider/v1:managed"
	if status := post(`{"description":"operations","prefix":"gateway-owned","api-key":"ops-new","base-url":"https://ops.example","models":[{"name":"model"}]}`); status != http.StatusConflict || len(h.cfg.ClaudeKey) != 1 {
		t.Fatalf("operations credential stole managed prefix: %d", status)
	}
	if status := post(`{"description":"gateway-provider/v1:managed","prefix":"gateway-owned","api-key":"second","base-url":"https://new.example","models":[{"name":"model"}]}`); status != http.StatusOK || len(h.cfg.ClaudeKey) != 2 {
		t.Fatalf("same-owner credential pool rejected: %d", status)
	}
}

func TestPatchClaudeKeyRejectsForeignManagedPrefix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("claude-api-key: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{ClaudeKey: []config.ClaudeKey{
		{Description: "operations", Prefix: "gateway-owned", APIKey: "ops", BaseURL: "https://ops.example"},
		{Description: "gateway-provider/v1:managed", Prefix: "managed-original", APIKey: "managed", BaseURL: "https://managed.example"},
	}}, path, nil)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/claude-api-key", strings.NewReader(`{"match":"managed","value":{"prefix":"gateway-owned"}}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PatchClaudeKey(ctx)
	if recorder.Code != http.StatusConflict || h.cfg.ClaudeKey[1].Prefix != "managed-original" {
		t.Fatalf("foreign Claude prefix was accepted: status=%d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/claude-api-key", strings.NewReader(`{"match":"ops","value":{"prefix":"managed-original"}}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PatchClaudeKey(ctx)
	if recorder.Code != http.StatusConflict || h.cfg.ClaudeKey[0].Prefix != "gateway-owned" {
		t.Fatalf("operations patch stole managed prefix: status=%d", recorder.Code)
	}
}

func TestPutClaudeKeysRejectsForeignManagedPrefix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("claude-api-key: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{ClaudeKey: []config.ClaudeKey{{Description: "gateway-provider/v1:managed", Prefix: "gateway-owned", APIKey: "managed", BaseURL: "https://managed.example"}}}, path, nil)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/claude-api-key", strings.NewReader(`[{"description":"gateway-provider/v1:managed","prefix":"gateway-owned","api-key":"managed","base-url":"https://managed.example"},{"description":"operations","prefix":"gateway-owned","api-key":"ops","base-url":"https://ops.example"}]`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PutClaudeKeys(ctx)
	if recorder.Code != http.StatusConflict || len(h.cfg.ClaudeKey) != 1 {
		t.Fatalf("full replacement stole managed prefix: status=%d", recorder.Code)
	}
}
