package openai_test

import (
	"context"
	planconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	native "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
		registry.GetGlobalRegistry().RegisterClient(id, "xai", []*registry.ModelInfo{{ID: "grok-tts", UpstreamProtocol: registry.UpstreamProtocolXAIVoiceHTTP, AudioSpeech: registry.XAISpeechProfile()}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
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
	registry.GetGlobalRegistry().RegisterClient("A", "xai", []*registry.ModelInfo{{ID: "grok-tts", UpstreamProtocol: registry.UpstreamProtocolXAIVoiceHTTP, AudioSpeech: registry.XAISpeechProfile()}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("A") })
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

func TestPlanSpeechHandlerRealCompatExecutorPreservesInternalCount(t *testing.T) {
	var wires atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wires.Add(1)
		_, _ = io.WriteString(w, `{"code":0,"data":"SUQzBAAAAAAAAA==","usage":{"text_words":2}}`+"\n"+`{"code":20000000,"usage":{"text_words":2}}`)
	}))
	defer srv.Close()
	transport := srv.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = srv.Certificate().DNSNames[0]
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	cfg := &planconfig.Config{OpenAICompatibility: []planconfig.OpenAICompatibility{{
		Name: "plan", Prefix: "source-A", BaseURL: "https://ark.cn-beijing.volces.com/api/plan/v3",
		APIKeyEntries: []planconfig.OpenAICompatibilityAPIKey{{APIKey: "fixture-plan-key"}},
		Models:        []planconfig.OpenAICompatibilityModel{{Name: "doubao-seed-tts-2.0", SpeechProfile: "ark-agent-plan-tts-2-v1", UpstreamProtocol: "volcengine-tts-v3-http"}},
	}}}
	manager := auth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	entries, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator()})
	if err != nil || len(entries) != 1 {
		t.Fatalf("normal config synthesis failed: count=%d err=%v", len(entries), err)
	}
	credential := entries[0]
	manager.RegisterExecutor(runtimeexecutor.NewOpenAICompatExecutor(credential.Provider, cfg))
	if _, err := manager.Register(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	// Match Service.applyModelPrefixes under this config's ForceModelPrefix=false.
	registry.GetGlobalRegistry().RegisterClient(credential.ID, credential.Provider, []*registry.ModelInfo{
		{ID: "doubao-seed-tts-2.0", UpstreamProtocol: "volcengine-tts-v3-http", AudioSpeech: registry.ArkPlanSpeechProfile("ark-agent-plan-tts-2-v1")},
		{ID: "source-A/doubao-seed-tts-2.0", MetadataModelID: "doubao-seed-tts-2.0", UpstreamProtocol: "volcengine-tts-v3-http", AudioSpeech: registry.ArkPlanSpeechProfile("ark-agent-plan-tts-2-v1")},
	})
	defer registry.GetGlobalRegistry().UnregisterClient(credential.ID)
	manager.RefreshSchedulerEntry(credential.ID)
	selected, selectErr := manager.SelectAuth(context.Background(), credential.Provider, "source-A/doubao-seed-tts-2.0", native.Options{})
	if selectErr != nil || selected == nil || selected.ID != credential.ID {
		t.Fatalf("normal config route selection failed: selected=%v err=%v", selected != nil, selectErr)
	}
	h := openai.NewOpenAIAPIHandler(base.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.POST("/v1/audio/speech", h.AudioSpeech)
	for _, spoof := range []bool{true, false} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(`{"model":"source-A/doubao-seed-tts-2.0","input":"你好"}`))
		req = req.WithContext(context.WithValue(req.Context(), "cliproxy.roundtripper", transport))
		if spoof {
			req.Header.Set("X-CLIProxy-Speech-Text-Words", "0")
		}
		router.ServeHTTP(rec, req)
		if spoof {
			if rec.Code != 400 || wires.Load() != 0 {
				t.Fatal("spoof reached supplier")
			}
		} else if rec.Code != 200 || rec.Header().Get("X-CLIProxy-Speech-Text-Words") != "2" || rec.Header().Get("Content-Type") != "audio/mpeg" || wires.Load() != 1 {
			t.Fatalf("status=%d count=%s wires=%d body=%s", rec.Code, rec.Header().Get("X-CLIProxy-Speech-Text-Words"), wires.Load(), rec.Body.String())
		}
	}
}

func TestCompatibleSpeechSelectedXAIWireAndNoFakeWords(t *testing.T) {
	var wires atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wires.Add(1)
		data, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/tts" || !strings.Contains(string(data), `"language":"auto"`) || strings.Contains(string(data), `"model"`) {
			t.Errorf("wrong translated native wire: %s %s", r.URL.Path, data)
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = io.WriteString(w, "RIFF-native-fixture")
	}))
	defer srv.Close()
	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(runtimeexecutor.NewXAIExecutor(nil))
	credential := &auth.Auth{ID: "speech-handler-A", Provider: "xai", Status: auth.StatusActive, Attributes: map[string]string{"api_key": "fixture", "base_url": srv.URL, "source": "config:xai[fixture]"}}
	if _, err := manager.Register(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(credential.ID, "xai", []*registry.ModelInfo{{ID: "customer-tts", MetadataModelID: "grok-tts", UpstreamProtocol: registry.UpstreamProtocolXAIVoiceHTTP, AudioSpeech: registry.XAISpeechProfile()}})
	defer reg.UnregisterClient(credential.ID)
	handler := openai.NewOpenAIAPIHandler(base.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	gin.SetMode(gin.TestMode)
	router := gin.New()
	configaccess.Register(&sdkconfig.SDKConfig{APIKeys: []string{"speech-proxy-service"}})
	defer configaccess.Register(nil)
	access := sdkaccess.NewManager()
	access.SetProviders(sdkaccess.RegisteredProviders())
	router.POST("/v1/audio/speech", api.AuthMiddleware(access), handler.AudioSpeech)
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"customer-tts","input":"hello","voice":"eve","response_format":"wav"}`))
	request.Header.Set("Authorization", "Bearer speech-proxy-service")
	request.Header.Set("X-CLIProxy-Auth-ID", credential.ID)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 200 || response.Body.String() != "RIFF-native-fixture" || response.Header().Get("Content-Type") != "audio/wav" || response.Header().Get("X-CLIProxy-Speech-Text-Words") != "" || response.Header().Get("X-Audio-Text-Words") != "" || wires.Load() != 1 {
		t.Fatalf("compatible speech lost native boundary: status=%d headers=%v wires=%d", response.Code, response.Header(), wires.Load())
	}
}
