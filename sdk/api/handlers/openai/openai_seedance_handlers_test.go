package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	apihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	native "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type seedanceCaptureExecutor struct {
	videoAuthCaptureExecutor
	calls []seedanceCapturedCall
}

type seedanceCapturedCall struct {
	pin, model, taskID, body string
}

func (*seedanceCaptureExecutor) Identifier() string { return "openai-compatible-seedance-test" }

func (e *seedanceCaptureExecutor) Execute(_ context.Context, auth *coreauth.Auth, req native.Request, opts native.Options) (native.Response, error) {
	e.calls = append(e.calls, seedanceCapturedCall{auth.ID, req.Model, opts.Alt, string(req.Payload)})
	return native.Response{
		StatusCode: http.StatusForbidden,
		Payload:    []byte(" { \"error\": {\"code\":\"ArkDenied\"} }\n"),
		Headers:    http.Header{"Content-Type": {"application/json"}, "X-Cliproxy-Auth-Id": {"hidden"}, "Location": {"https://do-not-forward.invalid"}},
	}, nil
}

func TestSeedanceTaskProtectedModelBoundary(t *testing.T) {
	const model = "doubao-seedance-2.0"
	const pin = "seedance-config-a"
	const body = `{"model":"doubao-seedance-2.0","content":[{"type":"text","text":"hello"}]}`
	executor := &seedanceCaptureExecutor{}
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)
	for _, fixture := range []struct {
		id, source string
		protocol   registry.UpstreamProtocol
	}{
		{pin, "config:seedance", registry.UpstreamProtocolVolcengineArkSeedanceV3},
		{"seedance-config-b", "config:seedance", registry.UpstreamProtocolVolcengineArkSeedanceV3},
		{"seedance-legacy", "config:seedance", registry.UpstreamProtocolLegacy},
		{"seedance-file", "file", registry.UpstreamProtocolVolcengineArkSeedanceV3},
	} {
		auth := &coreauth.Auth{ID: fixture.id, Provider: executor.Identifier(), Status: coreauth.StatusActive, Attributes: map[string]string{"source": fixture.source, "api_key": "fixture"}}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(fixture.id, auth.Provider, []*registry.ModelInfo{{ID: model, UpstreamProtocol: fixture.protocol}})
		manager.RefreshSchedulerEntry(fixture.id)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(fixture.id) })
	}
	h := NewOpenAIAPIHandler(apihandlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if c.GetHeader("Authorization") == "Bearer fixture-proxy-key" {
			c.Set("userApiKey", "fixture-authenticated")
		}
		c.Next()
	}, h.ServerCredentialPinMiddleware())
	router.POST(seedanceTaskPath, h.SeedanceTask)
	router.GET(seedanceTaskPath+"/:task_id", h.SeedanceTask)

	for _, tc := range []struct {
		name, method, path, body, pin string
		modelHeaders                  []string
		authenticated                 bool
		want                          int
	}{
		{"create", "POST", seedanceTaskPath, body, pin, nil, true, 403},
		{"get", "GET", seedanceTaskPath + "/cgt-fixture", "", pin, []string{model}, true, 403},
		{"missing access auth", "POST", seedanceTaskPath, body, pin, nil, false, 401},
		{"missing pin", "POST", seedanceTaskPath, body, "", nil, true, 400},
		{"conflicting pin", "POST", seedanceTaskPath, body, pin + ",seedance-config-b", nil, true, 400},
		{"wrong protocol", "POST", seedanceTaskPath, body, "seedance-legacy", nil, true, 400},
		{"file key", "POST", seedanceTaskPath, body, "seedance-file", nil, true, 400},
		{"query override", "GET", seedanceTaskPath + "/cgt-fixture?model=other", "", pin, []string{model}, true, 400},
		{"encoded id", "GET", seedanceTaskPath + "/cgt-%66ixture", "", pin, []string{model}, true, 400},
		{"dot id", "GET", seedanceTaskPath + "/..", "", pin, []string{model}, true, 400},
		{"get body", "GET", seedanceTaskPath + "/cgt-fixture", body, pin, []string{model}, true, 400},
		{"get missing model", "GET", seedanceTaskPath + "/cgt-fixture", "", pin, nil, true, 400},
		{"get duplicate model", "GET", seedanceTaskPath + "/cgt-fixture", "", pin, []string{model, model}, true, 400},
		{"get combined models", "GET", seedanceTaskPath + "/cgt-fixture", "", pin, []string{model + "," + model}, true, 400},
		{"get wrong model", "GET", seedanceTaskPath + "/cgt-fixture", "", pin, []string{"other"}, true, 400},
		{"create conflicting model header", "POST", seedanceTaskPath, body, pin, []string{model}, true, 400},
		{"list", "GET", seedanceTaskPath, "", pin, []string{model}, true, 404},
		{"delete", "DELETE", seedanceTaskPath + "/cgt-fixture", "", pin, []string{model}, true, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prior := len(executor.calls)
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.authenticated {
				req.Header.Set("Authorization", "Bearer fixture-proxy-key")
			}
			if tc.pin != "" {
				req.Header.Set("X-CLIProxy-Auth-ID", tc.pin)
			}
			for _, value := range tc.modelHeaders {
				req.Header.Add(seedanceModelHeader, value)
			}
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)
			if res.Code != tc.want {
				t.Fatalf("status=%d, body=%s, want %d", res.Code, res.Body.String(), tc.want)
			}
			if tc.want != 403 {
				if len(executor.calls) != prior {
					t.Fatal("refused request reached native execution")
				}
				return
			}
			if len(executor.calls) != prior+1 {
				t.Fatalf("got %d executions, want one", len(executor.calls)-prior)
			}
			taskID := strings.TrimPrefix(tc.path, seedanceTaskPath+"/")
			if tc.method == http.MethodPost {
				taskID = ""
			}
			if got := executor.calls[prior]; got != (seedanceCapturedCall{pin, model, taskID, tc.body}) {
				t.Fatalf("changed execution scope: %#v", got)
			}
			if res.Body.String() != " { \"error\": {\"code\":\"ArkDenied\"} }\n" || res.Header().Get("X-CLIProxy-Auth-ID") != "" || res.Header().Get("Location") != "" {
				t.Fatalf("Ark response changed or private header leaked: %s", res.Body.String())
			}
		})
	}
}
