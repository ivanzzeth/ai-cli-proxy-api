package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type sharedPinCaptureExecutor struct {
	modelExecutionCaptureExecutor
	ids []string
}

func (e *sharedPinCaptureExecutor) Execute(_ context.Context, a *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.ids = append(e.ids, a.ID)
	return coreexecutor.Response{Payload: []byte("ok")}, nil
}
func (e *sharedPinCaptureExecutor) ExecuteStream(_ context.Context, a *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.ids = append(e.ids, a.ID)
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}
func (e *sharedPinCaptureExecutor) CountTokens(_ context.Context, a *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.ids = append(e.ids, a.ID)
	return coreexecutor.Response{Payload: []byte("1")}, nil
}

type sharedPinActionHost struct {
	handlerDirectExecutorInterceptorHost
	beforeCalls int
}

func (h *sharedPinActionHost) InterceptRequestBeforeAuth(_ context.Context, req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	h.beforeCalls++
	return pluginapi.RequestInterceptResponse{Headers: cloneHeader(req.Headers), Body: cloneBytes(req.Body)}
}

func sharedPinFixture(t *testing.T) (*BaseAPIHandler, *sharedPinCaptureExecutor) {
	t.Helper()
	m := coreauth.NewManager(nil, nil, nil)
	e := &sharedPinCaptureExecutor{}
	m.RegisterExecutor(e)
	for _, id := range []string{"shared-A", "shared-B"} {
		if _, err := m.Register(t.Context(), &coreauth.Auth{ID: id, Provider: "codex", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "fixture"}}); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: "shared-pin-model"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
	}
	return NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, m), e
}
func sharedPinRequest(t *testing.T, h *BaseAPIHandler, id string) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if id != "" {
		c.Request.Header.Set("X-CLIProxy-Auth-ID", id)
		got, present, failure := h.PrepareServerCredentialPin(c)
		if failure != nil || !present || got != id {
			t.Fatalf("prepare=%q/%v/%v", got, present, failure)
		}
	}
	return c
}
func sharedPinRunSink(h *BaseAPIHandler, ctx context.Context, kind, model string) *interfaces.ErrorMessage {
	if kind == "prepared" {
		ctx, _ = h.PrepareStreamModelRoute(ctx, "openai", model, nil)
		kind = "stream"
	}
	switch kind {
	case "execute":
		_, _, failure := h.ExecuteWithAuthManager(ctx, "openai", model, nil, "")
		return failure
	case "count":
		_, _, failure := h.ExecuteCountWithAuthManager(ctx, "openai", model, nil, "")
		return failure
	default:
		data, _, errs := h.ExecuteStreamWithAuthManager(ctx, "openai", model, nil, "")
		if data != nil {
			for range data {
			}
		}
		if errs != nil {
			for failure := range errs {
				if failure != nil {
					return failure
				}
			}
		}
		return nil
	}
}
func TestSharedPinParserAndAuthority(t *testing.T) {
	for _, scenario := range []string{"none", "generic-oauth", "single-mixedcase", "blank", "empty-slice", "comma", "duplicate", "mixedcase-duplicate", "gone", "disabled", "authority-missing", "nil-authority-no-header", "request-typed-conflict"} {
		t.Run(scenario, func(t *testing.T) {
			h, _ := sharedPinFixture(t)
			c := sharedPinRequest(t, h, "")
			want := 400
			switch scenario {
			case "none":
				want = 0
			case "nil-authority-no-header":
				h.AuthManager = nil
				want = 0
			case "generic-oauth":
				c.Request.Header.Set("X-CLIProxy-Auth-ID", "shared-A")
				want = 0
			case "single-mixedcase":
				c.Request.Header["x-cLiPrOxY-aUtH-iD"] = []string{"shared-A"}
				want = 0
			case "blank":
				c.Request.Header.Set("X-CLIProxy-Auth-ID", " ")
			case "empty-slice":
				c.Request.Header["X-CLIProxy-Auth-ID"] = []string{}
			case "comma":
				c.Request.Header.Set("X-CLIProxy-Auth-ID", "shared-A,shared-B")
			case "duplicate":
				c.Request.Header["X-CLIProxy-Auth-ID"] = []string{"shared-A", "shared-A"}
			case "mixedcase-duplicate":
				c.Request.Header["X-CLIProxy-Auth-ID"] = []string{"shared-A"}
				c.Request.Header["x-cliproxy-auth-id"] = []string{"shared-A"}
			case "gone":
				c.Request.Header.Set("X-CLIProxy-Auth-ID", "gone")
			case "disabled":
				a, _ := h.AuthManager.GetByID("shared-A")
				a.Disabled = true
				if _, err := h.AuthManager.Update(t.Context(), a); err != nil {
					t.Fatal(err)
				}
				c.Request.Header.Set("X-CLIProxy-Auth-ID", "shared-A")
			case "authority-missing":
				h.AuthManager = nil
				c.Request.Header.Set("X-CLIProxy-Auth-ID", "shared-A")
				want = 503
			case "request-typed-conflict":
				c.Request = c.Request.WithContext(WithPinnedAuthID(c.Request.Context(), "shared-B"))
				c.Request.Header.Set("X-CLIProxy-Auth-ID", "shared-A")
			}
			id, present, failure := h.PrepareServerCredentialPin(c)
			if want != 0 {
				if failure == nil || failure.StatusCode != want {
					t.Fatalf("want%d got%v", want, failure)
				}
				return
			}
			if failure != nil {
				t.Fatal(failure)
			}
			if scenario == "generic-oauth" || scenario == "single-mixedcase" {
				if !present || id != "shared-A" {
					t.Fatalf("id=%q present=%v", id, present)
				}
				for key := range c.Request.Header {
					if http.CanonicalHeaderKey(key) == "X-Cliproxy-Auth-Id" {
						t.Fatal("reserved header retained")
					}
				}
			} else if present {
				t.Fatal("no header manufactured a scope")
			}
		})
	}
}
func TestSharedPinProtectedScopePreRouter(t *testing.T) {
	for _, kind := range []string{"execute", "stream", "count", "prepared"} {
		for _, scenario := range []string{"parentA-requestB", "parentB-requestA", "same", "parent-only", "request-only", "typed-conflict", "direct-gin-conflict", "disabled-after-prepare", "removed-after-prepare", "authority-after-prepare", "unsupported-model"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				h, e := sharedPinFixture(t)
				parent := sharedPinRequest(t, h, "shared-A").Request.Context()
				c := sharedPinRequest(t, h, "shared-A")
				wantError := true
				model := "shared-pin-model"
				switch scenario {
				case "parentA-requestB":
					c = sharedPinRequest(t, h, "shared-B")
				case "parentB-requestA":
					parent = sharedPinRequest(t, h, "shared-B").Request.Context()
				case "same":
					wantError = false
				case "parent-only":
					c = sharedPinRequest(t, h, "")
					wantError = false
				case "request-only":
					parent = context.Background()
					wantError = false
				case "typed-conflict":
					parent = WithPinnedAuthID(parent, "shared-B")
				case "disabled-after-prepare":
					a, _ := h.AuthManager.GetByID("shared-A")
					a.Disabled = true
					if _, err := h.AuthManager.Update(t.Context(), a); err != nil {
						t.Fatal(err)
					}
				case "removed-after-prepare":
					h.AuthManager.Remove(t.Context(), "shared-A")
				case "authority-after-prepare":
					h.AuthManager = nil
				case "unsupported-model":
					registry.GetGlobalRegistry().UnregisterClient("shared-A")
				}
				ctx, cancel := h.GetContextWithCancel(nil, c, parent)
				defer cancel()
				if scenario == "direct-gin-conflict" {
					ctx = context.WithValue(parent, "gin", sharedPinRequest(t, h, "shared-B"))
				}
				routes := 0
				host := &sharedPinActionHost{}
				host.hasRouters = true
				host.route = func(context.Context, pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
					routes++
					return pluginapi.ModelRouteResponse{}, false
				}
				h.SetModelRouterHost(host)
				h.SetPluginHost(host)
				failure := sharedPinRunSink(h, ctx, kind, model)
				if wantError {
					if failure == nil {
						t.Fatal("conflict accepted")
					}
					if len(e.ids) != 0 || host.lastPluginID != "" {
						t.Fatalf("dispatch after rejection: provider%v plugin%q", e.ids, host.lastPluginID)
					}
					if scenario != "unsupported-model" && (routes != 0 || host.beforeCalls != 0 || host.afterAuthCalled) {
						t.Fatalf("invalid scope reached router %d", routes)
					}
				} else {
					if failure != nil || len(e.ids) != 1 || e.ids[0] != "shared-A" {
						t.Fatalf("valid scope failure=%v dispatch=%v", failure, e.ids)
					}
				}
				t.Logf("scope=%s sink=%s routes=%d before=%d after=%v provider=%v plugin=%q", scenario, kind, routes, host.beforeCalls, host.afterAuthCalled, e.ids, host.lastPluginID)
			})
		}
	}
}
func TestSharedPinPluginModes(t *testing.T) {
	for _, kind := range []string{"execute", "stream", "count", "prepared"} {
		for _, pinned := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/pin=%v", kind, pinned), func(t *testing.T) {
				h, e := sharedPinFixture(t)
				id := ""
				if pinned {
					id = "shared-A"
				}
				c := sharedPinRequest(t, h, id)
				ctx, cancel := h.GetContextWithCancel(nil, c, context.Background())
				defer cancel()
				host := &handlerDirectExecutorRouteHost{}
				host.hasRouters = true
				host.route = func(context.Context, pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, bool) {
					return pluginapi.ModelRouteResponse{Handled: true, TargetKind: pluginapi.ModelRouteTargetExecutor, Target: "fixture-plugin"}, true
				}
				h.SetModelRouterHost(host)
				failure := sharedPinRunSink(h, ctx, kind, "shared-pin-model")
				if pinned {
					if failure == nil || host.lastPluginID != "" {
						t.Fatalf("pinned plugin dispatched: %v/%q", failure, host.lastPluginID)
					}
				} else if failure != nil || host.lastPluginID != "fixture-plugin" {
					t.Fatalf("legacy plugin changed: %v/%q", failure, host.lastPluginID)
				}
				if len(e.ids) != 0 {
					t.Fatalf("provider dispatched %v", e.ids)
				}
			})
		}
	}
}
