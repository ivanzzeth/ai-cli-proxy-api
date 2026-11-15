package executor

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	native "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const seedanceFixtureModel = "doubao-seedance-2.0"
const seedanceFixtureBody = `{ "model":"doubao-seedance-2.0","content":[{"type":"text","text":"hello"}],"duration":5,"resolution":"480p","ratio":"16:9","execution_expires_after":3600,"generate_audio":false }`

func seedanceExecutorFixture(t *testing.T) (*config.Config, *coreauth.Auth, native.Options) {
	t.Helper()
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name: "plan", BaseURL: seedancePlanOrigin,
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "fixture-first-key"}, {APIKey: "fixture-original-key"}},
		Models:        []config.OpenAICompatibilityModel{{Name: seedanceFixtureModel, Alias: seedanceFixtureModel, UpstreamProtocol: registry.UpstreamProtocolVolcengineArkSeedanceV3}},
	}}}
	auths, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{Config: cfg, IDGenerator: synthesizer.NewStableIDGenerator()})
	if err != nil || len(auths) != 2 {
		t.Fatalf("auth fixture count=%d, err=%v", len(auths), err)
	}
	credential := auths[1]
	format := sdktranslator.FromString(string(registry.UpstreamProtocolVolcengineArkSeedanceV3))
	opts := native.Options{SingleAttempt: true, SourceFormat: format, ResponseFormat: format, Metadata: map[string]any{
		native.PinnedAuthMetadataKey:     credential.ID,
		native.RequestedModelMetadataKey: seedanceFixtureModel,
	}, Headers: http.Header{
		"Authorization":             {"Bearer fixture-proxy-key"},
		"X-CLIProxy-Auth-ID":        {"internal-credential-id"},
		"X-CLIProxy-Seedance-Model": {seedanceFixtureModel},
		"Idempotency-Key":           {"never-forward"},
	}}
	return cfg, credential, opts
}

func TestVolcengineSeedanceHTTPWireAndNoReplay(t *testing.T) {
	for _, tc := range []struct {
		name, taskID, requestBody, response string
		status                              int
		disconnect                          bool
	}{
		{"create", "", seedanceFixtureBody, `{"id":"cgt-fixture"}`, http.StatusCreated, false},
		{"status", "cgt-fixture", "", `{ "id":"cgt-fixture","status":"succeeded","usage":{"completion_tokens":501} }`, http.StatusOK, false},
		{"reject", "", seedanceFixtureBody, `{"error":{"code":"InvalidParameter"}}`, http.StatusBadRequest, false},
		{"redirect", "", seedanceFixtureBody, `{"error":{"code":"Moved"}}`, http.StatusTemporaryRedirect, false},
		{"disconnect", "", seedanceFixtureBody, "", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				wantMethod, wantPath := http.MethodPost, "/api/plan/v3/contents/generations/tasks"
				if tc.taskID != "" {
					wantMethod, wantPath = http.MethodGet, wantPath+"/"+tc.taskID
				}
				body, _ := io.ReadAll(r.Body)
				if r.Host != "ark.cn-beijing.volces.com" || r.URL.Path != wantPath || r.URL.RawQuery != "" || r.Method != wantMethod || r.ProtoMajor != 1 || r.Header.Get("Authorization") != "Bearer fixture-original-key" || string(body) != tc.requestBody {
					t.Errorf("supplier operation changed: %s %s %s body=%q", r.Method, r.Host, r.URL, body)
				}
				for _, header := range []string{"X-CLIProxy-Auth-ID", "X-CLIProxy-Seedance-Model", "Idempotency-Key", "X-Idempotency-Key"} {
					if r.Header.Get(header) != "" {
						t.Errorf("private header forwarded: %s", header)
					}
				}
				if tc.disconnect {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Errorf("hijack: %v", err)
						return
					}
					_ = conn.Close()
					return
				}
				w.Header().Set("Location", "https://ark.cn-beijing.volces.com/never-follow")
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-CLIProxy-Auth-ID", "never-return")
				w.Header().Set("X-Request-Id", "ark-request-id")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.response)
			}))
			defer srv.Close()
			transport := srv.Client().Transport.(*http.Transport).Clone()
			transport.TLSClientConfig.ServerName = srv.Certificate().DNSNames[0]
			transport.Proxy = nil
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				if address != "ark.cn-beijing.volces.com:443" {
					t.Errorf("wrong Ark target: %s", address)
				}
				return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
			}
			cfg, credential, opts := seedanceExecutorFixture(t)
			opts.Alt = tc.taskID
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
			response, err := NewOpenAICompatExecutor(credential.Provider, cfg).Execute(ctx, credential, native.Request{Model: seedanceFixtureModel, Payload: []byte(tc.requestBody)}, opts)
			if calls.Load() != 1 {
				t.Fatalf("Ark calls=%d, want 1", calls.Load())
			}
			if tc.disconnect {
				var uncertain *seedanceAttemptError
				if !errors.As(err, &uncertain) || !uncertain.create || !uncertain.NonReplayableSeedanceAttempt() {
					t.Fatalf("expected unknown create stop, got %v", err)
				}
				return
			}
			if err != nil || response.StatusCode != tc.status || string(response.Payload) != tc.response ||
				response.Headers.Get("Content-Type") != "application/json" || response.Headers.Get("X-Request-Id") != "ark-request-id" ||
				response.Headers.Get("X-CLIProxy-Auth-ID") != "" || response.Headers.Get("Location") != "" {
				t.Fatalf("response=%#v err=%v", response, err)
			}
		})
	}
}

func TestVolcengineSeedanceRejectsInvalidBindingAndControls(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.Config, *coreauth.Auth, *native.Request, *native.Options)
	}{
		{"no pin", func(_ *config.Config, _ *coreauth.Auth, _ *native.Request, opts *native.Options) {
			delete(opts.Metadata, native.PinnedAuthMetadataKey)
		}},
		{"different pin", func(_ *config.Config, _ *coreauth.Auth, _ *native.Request, opts *native.Options) {
			opts.Metadata[native.PinnedAuthMetadataKey] = "elsewhere"
		}},
		{"rotated key", func(cfg *config.Config, _ *coreauth.Auth, _ *native.Request, _ *native.Options) {
			cfg.OpenAICompatibility[0].APIKeyEntries[1].APIKey = "rotated"
		}},
		{"rotated proxy", func(cfg *config.Config, _ *coreauth.Auth, _ *native.Request, _ *native.Options) {
			cfg.OpenAICompatibility[0].APIKeyEntries[1].ProxyURL = "direct"
		}},
		{"overridden authorization", func(cfg *config.Config, _ *coreauth.Auth, _ *native.Request, _ *native.Options) {
			cfg.OpenAICompatibility[0].Headers = map[string]string{"aUtHoRiZaTiOn": "Bearer override"}
		}},
		{"legacy protocol", func(cfg *config.Config, _ *coreauth.Auth, _ *native.Request, _ *native.Options) {
			cfg.OpenAICompatibility[0].Models[0].UpstreamProtocol = registry.UpstreamProtocolLegacy
		}},
		{"chat entry", func(_ *config.Config, _ *coreauth.Auth, _ *native.Request, opts *native.Options) {
			opts.SourceFormat = sdktranslator.FromString("openai")
		}},
		{"changed upstream name", func(cfg *config.Config, _ *coreauth.Auth, _ *native.Request, _ *native.Options) {
			cfg.OpenAICompatibility[0].Models[0].Name = "other"
		}},
		{"wrong origin", func(cfg *config.Config, credential *coreauth.Auth, _ *native.Request, _ *native.Options) {
			cfg.OpenAICompatibility[0].BaseURL = "https://other.invalid/api/plan/v3"
			credential.Attributes["base_url"] = cfg.OpenAICompatibility[0].BaseURL
		}},
		{"audio omitted", func(_ *config.Config, _ *coreauth.Auth, req *native.Request, _ *native.Options) {
			req.Payload = []byte(strings.Replace(string(req.Payload), `,"generate_audio":false`, "", 1))
		}},
		{"audio enabled", func(_ *config.Config, _ *coreauth.Auth, req *native.Request, _ *native.Options) {
			req.Payload = []byte(strings.Replace(string(req.Payload), `"generate_audio":false`, `"generate_audio":true`, 1))
		}},
		{"oversized prompt", func(_ *config.Config, _ *coreauth.Auth, req *native.Request, _ *native.Options) {
			req.Payload = []byte(strings.Replace(string(req.Payload), "hello", strings.Repeat("x", 4097), 1))
		}},
		{"callback", func(_ *config.Config, _ *coreauth.Auth, req *native.Request, _ *native.Options) {
			req.Payload = []byte(strings.Replace(string(req.Payload), `"duration":5`, `"duration":5,"callback_url":"https://example.invalid"`, 1))
		}},
		{"duplicate control", func(_ *config.Config, _ *coreauth.Auth, req *native.Request, _ *native.Options) {
			req.Payload = []byte(strings.Replace(string(req.Payload), `"duration":5`, `"duration":5,"duratio\u006e":9`, 1))
		}},
		{"get path traversal", func(_ *config.Config, _ *coreauth.Auth, req *native.Request, opts *native.Options) {
			req.Payload = nil
			opts.Alt = "../other"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, credential, opts := seedanceExecutorFixture(t)
			request := native.Request{Model: seedanceFixtureModel, Payload: []byte(seedanceFixtureBody)}
			tc.mutate(cfg, credential, &request, &opts)
			transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
				t.Error("rejected operation reached HTTP transport")
				return nil, errors.New("unexpected dial")
			}}
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
			_, err := NewOpenAICompatExecutor(credential.Provider, cfg).Execute(ctx, credential, request, opts)
			var status interface{ StatusCode() int }
			if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
				t.Fatalf("expected preflight rejection, got %v", err)
			}
		})
	}
}
