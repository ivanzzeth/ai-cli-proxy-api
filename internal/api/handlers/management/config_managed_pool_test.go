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

func TestManagedPoolEditPreservesOtherCredentialsAndRejectsStaleRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, kind := range []string{"claude", "gemini"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("port: 8317\n"), 0600); err != nil {
				t.Fatal(err)
			}
			old := config.GeminiModel{Name: "upstream", Alias: "customer"}
			cfg := &config.Config{}
			if kind == "claude" {
				cfg.ClaudeKey = []config.ClaudeKey{
					{Description: "gateway-provider/v1:fixture", Prefix: "fixture", APIKey: "first", BaseURL: "https://old.example", Models: []config.ClaudeModel{{Name: old.Name, Alias: old.Alias, MaxContextLength: 128000}}},
					{Description: "gateway-provider/v1:fixture", Prefix: "fixture", APIKey: "second", BaseURL: "https://old.example", Models: []config.ClaudeModel{{Name: old.Name, Alias: old.Alias, MaxContextLength: 128000}}},
					{Description: "operator", APIKey: "operator", BaseURL: "https://ops.example"},
				}
			} else {
				cfg.GeminiKey = []config.GeminiKey{
					{Description: "gateway-provider/v1:fixture", APIKey: "first", BaseURL: "https://old.example", Models: []config.GeminiModel{{Name: old.Name, Alias: old.Alias, MaxContextLength: 128000}}},
					{Description: "gateway-provider/v1:fixture", APIKey: "second", BaseURL: "https://old.example", Models: []config.GeminiModel{{Name: old.Name, Alias: old.Alias, MaxContextLength: 128000}}},
					{Description: "operator", APIKey: "operator", BaseURL: "https://ops.example"},
				}
			}
			handler := &Handler{cfg: cfg, configFilePath: path}
			edit := handler.PatchManagedGeminiPool
			if kind == "claude" {
				edit = handler.PatchManagedClaudePool
			}
			post := func(body string) int {
				recorder := httptest.NewRecorder()
				context, _ := gin.CreateTestContext(recorder)
				context.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/"+kind+"-api-key/managed-pool", strings.NewReader(body))
				context.Request.Header.Set("Content-Type", "application/json")
				edit(context)
				return recorder.Code
			}
			payload := `{"description":"gateway-provider/v1:fixture","expected-prefix":"fixture","expected-api-keys":["first","second"],"expected-base-url":"https://old.example","expected-models":[{"name":"upstream","alias":"customer"}],"base-url":"https://new.example","models":[{"name":"new-upstream","alias":"new-customer"}]}`
			if status := post(payload); status != http.StatusOK {
				t.Fatalf("pool edit returned %d", status)
			}
			if kind == "claude" {
				for _, entry := range cfg.ClaudeKey[:2] {
					if entry.BaseURL != "https://new.example" || entry.Models[0].Name != "new-upstream" || entry.Models[0].Alias != "new-customer" {
						t.Fatalf("incomplete Claude pool edit: %+v", entry)
					}
				}
				if cfg.ClaudeKey[2].BaseURL != "https://ops.example" {
					t.Fatal("operator credential was modified")
				}
			} else {
				for _, entry := range cfg.GeminiKey[:2] {
					if entry.BaseURL != "https://new.example" || entry.Models[0].Name != "new-upstream" || entry.Models[0].Alias != "new-customer" {
						t.Fatalf("incomplete Gemini pool edit: %+v", entry)
					}
				}
				if cfg.GeminiKey[2].BaseURL != "https://ops.example" {
					t.Fatal("operator credential was modified")
				}
			}
			if status := post(payload); status != http.StatusConflict {
				t.Fatalf("stale pool edit returned %d", status)
			}
			if kind == "claude" {
				cfg.ClaudeKey[1].APIKey = "rotated"
			} else {
				cfg.GeminiKey[1].APIKey = "rotated"
			}
			freshPayload := `{"description":"gateway-provider/v1:fixture","expected-prefix":"fixture","expected-api-keys":["first","second"],"expected-base-url":"https://new.example","expected-models":[{"name":"new-upstream","alias":"new-customer"}],"base-url":"https://another.example","models":[{"name":"another-upstream","alias":"another-customer"}]}`
			if status := post(freshPayload); status != http.StatusConflict {
				t.Fatalf("rotated key was overwritten: status=%d", status)
			}
			if kind == "claude" {
				if models := updatedClaudePoolModels([]config.ClaudeModel{{Name: "upstream", Alias: "customer", MaxContextLength: 128000}}, []config.GeminiModel{{Name: "upstream", Alias: "renamed"}}); models[0].MaxContextLength != 128000 || models[0].Alias != "renamed" {
					t.Fatalf("Claude model metadata lost on alias edit: %+v", models)
				}
			} else if models := updatedGeminiPoolModels([]config.GeminiModel{{Name: "upstream", Alias: "customer", MaxContextLength: 128000}}, []config.GeminiModel{{Name: "upstream", Alias: "renamed"}}); models[0].MaxContextLength != 128000 || models[0].Alias != "renamed" {
				t.Fatalf("Gemini model metadata lost on alias edit: %+v", models)
			}
			if kind == "claude" && cfg.ClaudeKey[0].BaseURL != "https://new.example" || kind == "gemini" && cfg.GeminiKey[0].BaseURL != "https://new.example" {
				t.Fatal("partial update after key rotation conflict")
			}
		})
	}
}

func TestManagedPoolEditRestoresMemoryWhenPersistenceFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, kind := range []string{"claude", "gemini"} {
		t.Run(kind, func(t *testing.T) {
			cfg := &config.Config{}
			for _, key := range []string{"first", "second"} {
				if kind == "claude" {
					cfg.ClaudeKey = append(cfg.ClaudeKey, config.ClaudeKey{Description: "gateway-provider/v1:fixture", Prefix: "fixture", APIKey: key, BaseURL: "https://old.example", Models: []config.ClaudeModel{{Name: "upstream", Alias: "customer"}}})
				} else {
					cfg.GeminiKey = append(cfg.GeminiKey, config.GeminiKey{Description: "gateway-provider/v1:fixture", APIKey: key, BaseURL: "https://old.example", Models: []config.GeminiModel{{Name: "upstream", Alias: "customer"}}})
				}
			}
			handler := &Handler{cfg: cfg, configFilePath: filepath.Join(t.TempDir(), "missing", "config.yaml")}
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/"+kind+"-api-key/managed-pool", strings.NewReader(`{"description":"gateway-provider/v1:fixture","expected-prefix":"fixture","expected-api-keys":["first","second"],"expected-base-url":"https://old.example","expected-models":[{"name":"upstream","alias":"customer"}],"base-url":"https://new.example","models":[{"name":"new-upstream","alias":"new-customer"}]}`))
			context.Request.Header.Set("Content-Type", "application/json")
			if kind == "claude" {
				handler.PatchManagedClaudePool(context)
			} else {
				handler.PatchManagedGeminiPool(context)
			}
			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("failed persistence returned %d", recorder.Code)
			}
			if kind == "claude" {
				for _, entry := range cfg.ClaudeKey {
					if entry.BaseURL != "https://old.example" || entry.Models[0].Alias != "customer" {
						t.Fatal("failed save changed Claude credentials in memory")
					}
				}
			} else {
				for _, entry := range cfg.GeminiKey {
					if entry.BaseURL != "https://old.example" || entry.Models[0].Alias != "customer" {
						t.Fatal("failed save changed Gemini credentials in memory")
					}
				}
			}
		})
	}
}
