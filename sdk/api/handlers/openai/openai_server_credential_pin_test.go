package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	apihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestSharedPinVideoAffinityAndContentBoundary(t *testing.T) {
	for _, scenario := range []string{"conflict", "matching", "legacy", "content-denied"} {
		t.Run(scenario, func(t *testing.T) {
			resetVideoAuthBindingsForTest(t)
			e := &videoAuthCaptureExecutor{requestID: "shared-pin-video"}
			h := newVideoAuthBindingTestHandler(t, e)
			pin := "shared-pin-video-auth-a"
			bound := pin
			if scenario == "conflict" {
				bound = "shared-pin-video-auth-b"
			}
			videoAuthBindings.set("video-pin", bound, time.Hour)
			router := gin.New()
			router.Use(h.ServerCredentialPinMiddleware())
			route, path := "/openai/v1/videos/:video_id", "/openai/v1/videos/video-pin"
			if scenario == "content-denied" {
				route += "/content"
				path += "/content"
				router.GET(route, h.RejectServerCredentialPin(), h.VideosContent)
			} else {
				router.GET(route, h.VideosRetrieve)
			}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			if scenario != "legacy" {
				req.Header.Set("X-CLIProxy-Auth-ID", pin)
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if scenario == "conflict" || scenario == "content-denied" {
				if rec.Code != 400 || len(e.AuthIDs()) != 0 {
					t.Fatalf("unsafe video action status%d auth%v body%s", rec.Code, e.AuthIDs(), rec.Body.String())
				}
			} else {
				if rec.Code != 200 || len(e.AuthIDs()) != 1 || e.AuthIDs()[0] != pin {
					t.Fatalf("video compatibility status%d auth%v body%s", rec.Code, e.AuthIDs(), rec.Body.String())
				}
			}
			t.Logf("video scenario=%s status=%d status_dispatches=%d", scenario, rec.Code, len(e.AuthIDs()))
		})
	}
}

func TestSharedPinDirectDomainHandlersStillRejectMalformed(t *testing.T) {
	for _, endpoint := range []string{"responses", "voice"} {
		t.Run(endpoint, func(t *testing.T) {
			// Deliberately no route-level pin middleware: existing direct mounts retain protection.
			base := apihandlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
			router := gin.New()
			if endpoint == "responses" {
				router.POST("/", NewOpenAIResponsesAPIHandler(base).Responses)
			} else {
				router.POST("/", NewOpenAIAPIHandler(base).XAIVoiceHTTP)
			}
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"model":"unused"}`))
			req.Header["X-CLIProxy-Auth-ID"] = []string{"A", "B"}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != 400 && rec.Code != 503 {
				t.Fatalf("direct handler bypass: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestSharedPinBackgroundBridgeVideoContext(t *testing.T) {
	resetVideoAuthBindingsForTest(t)
	e := &videoAuthCaptureExecutor{requestID: "shared-bridge"}
	h := newVideoAuthBindingTestHandler(t, e)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Request.Header.Set("X-CLIProxy-Auth-ID", "shared-bridge-auth-a")
	if _, _, failure := h.PrepareServerCredentialPin(c); failure != nil {
		t.Fatal(failure)
	}
	ctx, cancel := h.GetContextWithCancel(h, c, context.Background())
	defer cancel()
	videoAuthBindings.set("bridge-video", "shared-bridge-auth-b", time.Hour)
	if _, failure := h.contextWithVideoAuthBinding(ctx, "bridge-video"); failure == nil {
		t.Fatal("video binding replaced protected Background bridge")
	}
}
