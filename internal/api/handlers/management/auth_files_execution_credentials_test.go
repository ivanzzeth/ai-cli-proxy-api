package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestVoiceExecutionCredentialPublicNativeConfigReference(t *testing.T) {
	cfg := &config.Config{XAIKey: []config.XAIKey{{APIKey: "fixture-private-key", BaseURL: "https://example.invalid/v1", Prefix: "owned-prefix"}}}
	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(runtimeexecutor.NewXAIExecutor(cfg))
	synth := synthesizer.NewConfigSynthesizer()
	entries, err := synth.Synthesize(&synthesizer.SynthesisContext{Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator()})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if _, err := manager.Register(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	handler := &Handler{cfg: cfg, authManager: manager}
	projection := handler.buildExecutionCredentials()
	if len(projection) != 1 {
		t.Fatalf("credential count%d", len(projection))
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/xai-api-key", nil)
	handler.GetXAIKeys(c)
	var public struct {
		Keys []struct {
			AuthIndex string `json:"auth-index"`
		} `json:"xai-api-key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &public); err != nil {
		t.Fatal(err)
	}
	if len(public.Keys) != 1 || public.Keys[0].AuthIndex == "" || projection[0].SourceRef != public.Keys[0].AuthIndex || projection[0].SourceKind != "config" || projection[0].ID != entries[0].ID || len(projection[0].CompatibleOperations) != 8 {
		t.Fatal("native public config/auth-index bridge not exact")
	}
	safe, _ := json.Marshal(projection)
	for _, secret := range []string{"fixture-private-key", "base_url", "config_index", "header:"} {
		if strings.Contains(string(safe), secret) {
			t.Fatalf("unsafe field leaked: %s", secret)
		}
	}
	manager.RegisterExecutor(&voiceUnsupportedExecutor{})
	if len(handler.buildExecutionCredentials()[0].CompatibleOperations) != 0 {
		t.Fatal("unknown replacement executor advertised")
	}
}

type voiceUnsupportedExecutor struct{ auth.ProviderExecutor }

func (*voiceUnsupportedExecutor) Identifier() string { return "xai" }
func TestVoiceExecutionCredentialNilManagerKeepsDiskFilesAndEmptyProjection(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample.json"), []byte(`{"type":"xai","email":"fixture@example.invalid"}`), 0600); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: &config.Config{AuthDir: dir}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files?page=1&page_size=10", nil)
	h.ListAuthFiles(c)
	var data struct {
		Files       []map[string]any      `json:"files"`
		Credentials []executionCredential `json:"execution_credentials"`
		Total       int                   `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || len(data.Files) != 1 || data.Total != 1 || data.Credentials == nil || len(data.Credentials) != 0 {
		t.Fatalf("legacy disk fallback fields lost: code%d files%d total%d empty_present%v", rec.Code, len(data.Files), data.Total, data.Credentials != nil)
	}
}
