package openai_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	configaccess "github.com/router-for-me/CLIProxyAPI/v8/internal/access/config_access"
	api "github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	base "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	openai "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestResponsesCredentialPinUsesActualNativeManager(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, scenario := range []string{"A", "single-mixedcase", "gone", "disabled-A", "unsupported-A", "multi", "mixedcase-duplicate", "empty-slice", "blank", "combined", "legacy", "body-query-only", "unauthenticated", "plugin-self-valid", "plugin-self-gone", "plugin-self-disabled", "plugin-self-unsupported", "plugin-self-legacy", "plugin-executor-valid", "plugin-executor-gone", "plugin-executor-disabled", "plugin-executor-unsupported", "plugin-executor-legacy"} {
			t.Run(fmt.Sprintf("stream=%v/%s", stream, scenario), func(t *testing.T) {
				var a, b atomic.Int32
				provider := func(counter *atomic.Int32) *httptest.Server {
					return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						counter.Add(1)
						if r.Header.Get("Authorization") != "Bearer fixture" {
							t.Error("selected native token lost")
						}
						for name := range r.Header {
							if strings.EqualFold(name, "X-CLIProxy-Auth-ID") {
								t.Error("internal pin reached vendor wire")
							}
						}
						if r.Header.Get("X-Normal") != "retained" {
							t.Error("normal custom header lost")
						}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_pin\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"grok-4.6\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
					}))
				}
				first, second := provider(&a), provider(&b)
				defer first.Close()
				defer second.Close()
				manager := auth.NewManager(nil, nil, nil)
				manager.RegisterExecutor(runtimeexecutor.NewXAIExecutor(&config.Config{}))
				model := "shared/grok-4.6"
				for id, target := range map[string]string{"A": first.URL + "/v1", "B": second.URL + "/v1"} {
					item := &auth.Auth{ID: id, Provider: "xai", Prefix: "shared", Status: auth.StatusActive, Disabled: id == "A" && (scenario == "disabled-A" || scenario == "body-query-only" || strings.HasSuffix(scenario, "-disabled")), Attributes: map[string]string{"api_key": "fixture", "base_url": target, "source": "config:xai[opaque]", "header:x-cLiPrOxY-aUtH-iD": "static-reinjection", "header:X-Normal": "retained"}}
					if _, err := manager.Register(context.Background(), item); err != nil {
						t.Fatal(err)
					}
					if id != "A" || scenario != "unsupported-A" && !strings.HasSuffix(scenario, "-unsupported") {
						registry.GetGlobalRegistry().RegisterClient(id, "xai", []*registry.ModelInfo{{ID: model}})
					}
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				}
				gin.SetMode(gin.TestMode)
				apiBase := base.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
				pluginHost := &responsesPinPluginHost{kind: pluginapi.ModelRouteTargetExecutor}
				isPlugin := strings.HasPrefix(scenario, "plugin-")
				if strings.HasPrefix(scenario, "plugin-self-") {
					pluginHost.kind = pluginapi.ModelRouteTargetSelf
				}
				if isPlugin {
					apiBase.SetModelRouterHost(pluginHost)
				}
				handler := openai.NewOpenAIResponsesAPIHandler(apiBase)
				router := gin.New()
				configaccess.Register(&sdkconfig.SDKConfig{APIKeys: []string{"proxy-service-test"}})
				t.Cleanup(func() { configaccess.Register(nil) })
				access := sdkaccess.NewManager()
				access.SetProviders(sdkaccess.RegisteredProviders())
				router.POST("/v1/responses", api.AuthMiddleware(access), handler.Responses)
				bodyID := "B"
				if scenario == "body-query-only" {
					bodyID = "A"
				}
				req := httptest.NewRequest(http.MethodPost, "/v1/responses?auth_id="+bodyID, strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"hello","stream":%v,"auth_id":%q}`, model, stream, bodyID)))
				req.Header.Set("Content-Type", "application/json")
				if scenario != "unauthenticated" {
					req.Header.Set("Authorization", "Bearer proxy-service-test")
				}
				switch scenario {
				case "legacy", "body-query-only":
				case "single-mixedcase":
					req.Header["x-cLiPrOxY-aUtH-iD"] = []string{"A"}
				case "gone":
					req.Header.Set("X-CLIProxy-Auth-ID", "gone")
				case "multi":
					req.Header["X-CLIProxy-Auth-ID"] = []string{"A", "B"}
				case "mixedcase-duplicate":
					req.Header["X-CLIProxy-Auth-ID"] = []string{"A"}
					req.Header["x-cliproxy-auth-id"] = []string{"B"}
				case "empty-slice":
					req.Header["X-CLIProxy-Auth-ID"] = []string{}
				case "blank":
					req.Header.Set("X-CLIProxy-Auth-ID", " ")
				case "combined":
					req.Header.Set("X-CLIProxy-Auth-ID", "A,B")
				default:
					req.Header.Set("X-CLIProxy-Auth-ID", "A")
				}
				if isPlugin {
					req.Header.Del("X-CLIProxy-Auth-ID")
					if !strings.HasSuffix(scenario, "-legacy") {
						pin := "A"
						if strings.HasSuffix(scenario, "-gone") {
							pin = "gone"
						}
						req.Header.Set("X-CLIProxy-Auth-ID", pin)
					}
				}
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				if isPlugin {
					invalidScope := strings.HasSuffix(scenario, "-gone") || strings.HasSuffix(scenario, "-disabled")
					if invalidScope {
						if pluginHost.routes.Load() != 0 {
							t.Fatal("unavailable credential scope reached model router")
						}
					} else if pluginHost.routes.Load() == 0 {
						t.Fatal("actual Base model-router branch was not reached")
					}
					if strings.HasSuffix(scenario, "-legacy") {
						if rec.Code != 200 || pluginHost.executes.Load() != 1 {
							t.Fatalf("no-pin plugin compatibility failed status%d calls%d", rec.Code, pluginHost.executes.Load())
						}
					} else if rec.Code != 400 || pluginHost.executes.Load() != 0 {
						t.Fatalf("pinned request reached plugin status%d calls%d", rec.Code, pluginHost.executes.Load())
					}
					if a.Load()+b.Load() != 0 {
						t.Fatal("plugin refusal fell back to native Source")
					}
					return
				}
				switch scenario {
				case "A", "single-mixedcase":
					if rec.Code != 200 || a.Load() != 1 || b.Load() != 0 {
						t.Fatalf("pin failed status%d wires A/B=%d/%d body=%s", rec.Code, a.Load(), b.Load(), rec.Body.String())
					}
				case "legacy":
					if rec.Code != 200 || a.Load()+b.Load() != 1 {
						t.Fatalf("legacy changed status%d A/B=%d/%d", rec.Code, a.Load(), b.Load())
					}
				case "body-query-only":
					if rec.Code != 200 || a.Load() != 0 || b.Load() != 1 {
						t.Fatalf("body/query acquired authority status%d A/B=%d/%d", rec.Code, a.Load(), b.Load())
					}
				default:
					if rec.Code < 400 || a.Load()+b.Load() != 0 {
						t.Fatalf("invalid pin/auth fell back status%d A/B=%d/%d", rec.Code, a.Load(), b.Load())
					}
				}
			})
		}
	}
}

// Same exported router/executor test-host contract used by existing Base and
// Responses WebSocket fixtures; execution counters observe the real dispatch.
type responsesPinPluginHost struct {
	kind     pluginapi.ModelRouteTargetKind
	routes   atomic.Int32
	executes atomic.Int32
}

func (h *responsesPinPluginHost) HasModelRouters() bool { return true }
func (h *responsesPinPluginHost) RouteModel(_ context.Context, _ pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
	h.routes.Add(1)
	return pluginapi.ModelRouteResponse{Handled: true, TargetKind: h.kind, Target: "responses-pin-plugin"}, true
}
func (h *responsesPinPluginHost) ExecutePluginExecutor(_ context.Context, _ string, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	h.executes.Add(1)
	return coreexecutor.Response{Payload: []byte(`{"id":"plugin-positive-control","output":[]}`)}, nil
}
func (h *responsesPinPluginHost) ExecutePluginExecutorStream(_ context.Context, _ string, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	h.executes.Add(1)
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"plugin-positive-control\",\"output\":[]}}\n\n")}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}
func (h *responsesPinPluginHost) CountPluginExecutor(_ context.Context, _ string, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, fmt.Errorf("unexpected count call")
}
