package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestPutConfigYAMLClientKeySaveKeepsUpstreamProviders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const original = `config-version: 8
access:
  api-keys:
    - existing
api-keys:
  openai-compatibility:
    - name: sub2api
      base-url: https://example.test/v1
      models:
        - name: gpt-5.6-sol
      keys:
        - api-key: upstream-secret
server:
  port: 8317
`
	// Old visual editor replaced the upstream map with a client-key list.
	const body = `config-version: 8
access:
  api-keys:
    - existing
api-keys:
  - added
server:
  port: 8317
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.PUT("/v0/management/config.yaml", h.PutConfigYAML)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/v0/management/config.yaml", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.OpenAICompatibility) != 1 || loaded.OpenAICompatibility[0].Name != "sub2api" {
		t.Fatalf("upstream providers lost: %+v", loaded.OpenAICompatibility)
	}
	if !reflect.DeepEqual(loaded.APIKeys, []string{"existing", "added"}) {
		t.Fatalf("client keys = %#v", loaded.APIKeys)
	}
}
