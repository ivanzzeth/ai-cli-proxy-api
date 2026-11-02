package openai_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	configaccess "github.com/router-for-me/CLIProxyAPI/v8/internal/access/config_access"
	api "github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	base "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	openai "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestXAIVoiceMandatoryPinAndNativeCredentialWire(t *testing.T) {
	var a, b atomic.Int32
	server := func(counter *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			if r.Header.Get("Authorization") != "Bearer fixture" {
				t.Error("native credential replaced by caller service key")
			}
			if r.Header.Get("X-CLIProxy-Auth-ID") != "" {
				t.Error("internal pin leaked")
			}
			w.Header().Set("Content-Type", "audio/wav")
			w.WriteHeader(201)
			_, _ = w.Write([]byte("actual audio"))
		}))
	}
	first, second := server(&a), server(&b)
	defer first.Close()
	defer second.Close()
	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(runtimeexecutor.NewXAIExecutor(nil))
	for id, target := range map[string]string{"A": first.URL, "B": second.URL} {
		_, err := manager.Register(context.Background(), &auth.Auth{ID: id, Provider: "xai", Status: auth.StatusActive, Attributes: map[string]string{"api_key": "fixture", "base_url": target, "source": "config:xai[opaque]"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	handler := openai.NewOpenAIAPIHandler(base.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	gin.SetMode(gin.TestMode)
	router := gin.New()
	configaccess.Register(&sdkconfig.SDKConfig{APIKeys: []string{"proxy-service-test"}})
	t.Cleanup(func() { configaccess.Register(nil) })
	access := sdkaccess.NewManager()
	access.SetProviders(sdkaccess.RegisteredProviders())
	router.POST("/v1/tts", api.AuthMiddleware(access), handler.XAIVoiceHTTP)
	for _, credential := range []string{"", "Bearer wrong"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/tts?auth_id=A", strings.NewReader(`{"auth_id":"A"}`))
		req.Header.Set("X-CLIProxy-Auth-ID", "A")
		if credential != "" {
			req.Header.Set("Authorization", credential)
		}
		router.ServeHTTP(rec, req)
		if rec.Code != 401 || a.Load() != 0 || b.Load() != 0 {
			t.Fatal("standard authentication did not stop provider execution")
		}
	}
	for _, scenario := range []string{"missing", "duplicate", "mixedcase", "stale", "valid"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/tts?auth_id=B", strings.NewReader(`{"auth_id":"B","input":"hello"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer proxy-service-test")
		switch scenario {
		case "duplicate":
			req.Header["X-CLIProxy-Auth-ID"] = []string{"A", "B"}
		case "mixedcase":
			req.Header["X-CLIProxy-Auth-ID"] = []string{"A"}
			req.Header["x-cliproxy-auth-id"] = []string{"B"}
		case "stale":
			req.Header.Set("X-CLIProxy-Auth-ID", "gone")
		case "valid":
			req.Header.Set("X-CLIProxy-Auth-ID", "A")
		}
		router.ServeHTTP(rec, req)
		if scenario == "valid" {
			if rec.Code != 201 || rec.Body.String() != "actual audio" || rec.Header().Get("Content-Type") != "audio/wav" {
				t.Fatalf("native status/body %d %s", rec.Code, rec.Body.String())
			}
		} else if rec.Code != 400 {
			t.Fatalf("%s status%d", scenario, rec.Code)
		}
	}
	if a.Load() != 1 || b.Load() != 0 {
		t.Fatalf("Source A/B wires=%d/%d", a.Load(), b.Load())
	}
}

func TestXAIVoiceNativeErrorBodyHeadersWithoutGenericPassthrough(t *testing.T) {
	var wires atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wires.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.Header().Set("X-Request-Id", "voice-native-error")
		w.Header().Set("Set-Cookie", "never")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"native limited"}}`))
	}))
	defer srv.Close()
	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(runtimeexecutor.NewXAIExecutor(nil))
	_, err := manager.Register(context.Background(), &auth.Auth{ID: "A", Provider: "xai", Status: auth.StatusActive, Attributes: map[string]string{"api_key": "fixture", "base_url": srv.URL, "source": "config:xai[opaque]"}})
	if err != nil {
		t.Fatal(err)
	}
	handler := openai.NewOpenAIAPIHandler(base.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.POST("/v1/tts", handler.XAIVoiceHTTP)
	rec := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/tts", strings.NewReader(`{}`))
	request.Header.Set("X-CLIProxy-Auth-ID", "A")
	router.ServeHTTP(rec, request)
	if rec.Code != 429 || rec.Body.String() != `{"error":{"message":"native limited"}}` || rec.Header().Get("Retry-After") != "7" || rec.Header().Get("X-Request-Id") != "voice-native-error" || rec.Header().Get("Set-Cookie") != "" || wires.Load() != 1 {
		t.Fatalf("native raw error mismatch status=%d wires=%d retry=%q", rec.Code, wires.Load(), rec.Header().Get("Retry-After"))
	}
}
