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

func TestPostOpenAICompatPreservesExistingAndRejectsDuplicate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := []byte("port: 8317\napi-keys: [client]\nopenai-compatibility:\n  - name: existing\n    base-url: https://existing.example/v1\n    models: [{name: old}]\n    api-key-entries: [{api-key: old-key}]\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.POST("/v0/management/openai-compatibility", h.PostOpenAICompat)
	body := `{"name":"new","base-url":"https://new.example/v1","models":[{"name":"new-model"}],"api-key-entries":[{"api-key":"new-key"}]}`
	for _, expected := range []int{http.StatusOK, http.StatusConflict} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v0/management/openai-compatibility", strings.NewReader(body)))
		if recorder.Code != expected {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
	if len(h.cfg.OpenAICompatibility) != 2 || h.cfg.OpenAICompatibility[0].Name != "existing" || h.cfg.OpenAICompatibility[1].Name != "new" {
		t.Fatalf("unexpected providers: %#v", h.cfg.OpenAICompatibility)
	}
}

func TestPostOpenAICompatRejectsSharedModelPrefix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("openai-compatibility: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "operations", Prefix: "gateway-owned", BaseURL: "https://existing.example/v1"}}}, path, nil)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/openai-compatibility", strings.NewReader(`{"name":"managed","prefix":"gateway-owned","base-url":"https://new.example/v1","models":[{"name":"model"}],"api-key-entries":[{"api-key":"secret"}]}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.PostOpenAICompat(ctx)
	if recorder.Code != http.StatusConflict || len(h.cfg.OpenAICompatibility) != 1 {
		t.Fatalf("shared model prefix was accepted: status=%d providers=%+v", recorder.Code, h.cfg.OpenAICompatibility)
	}
}
