package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type sharedPinRouteExecutor struct {
	codexSearchCaptureExecutor
	executions int
}

func (e *sharedPinRouteExecutor) Execute(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	e.executions++
	return coreexecutor.Response{Payload: []byte(`{"id":"fixture","choices":[]}`)}, nil
}
func (e *sharedPinRouteExecutor) CountTokens(context.Context, *auth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	e.executions++
	return coreexecutor.Response{Payload: []byte(`{"input_tokens":1}`)}, nil
}

func TestSharedPinRegisteredDirectRoutesFailClosed(t *testing.T) {
	s := newTestServer(t)
	e := &sharedPinRouteExecutor{}
	s.handlers.AuthManager.RegisterExecutor(e)
	if _, err := s.handlers.AuthManager.Register(t.Context(), &auth.Auth{ID: "route-pin", Provider: "codex", Status: auth.StatusActive}); err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ method, path string }{
		{"GET", "/v1/responses"}, {"GET", "/backend-api/codex/responses"}, {"GET", "/openai/v1/videos/id/content"},
		{"POST", "/v1/live"}, {"GET", "/v1/live/id"}, {"POST", "/v1/alpha/search"}, {"POST", "/backend-api/codex/alpha/search"},
		{"GET", "/v1/realtime"}, {"POST", "/v1/realtime"}, {"POST", "/v1/realtime/calls"}, {"GET", "/v1/realtime/calls/id"},
		{"POST", "/v1/realtime/client_secrets"}, {"POST", "/v1/realtime/sessions"}, {"POST", "/v1/realtime/transcription_sessions"},
		{"GET", "/v1/realtime/translations"}, {"POST", "/v1/realtime/translations"}, {"POST", "/v1/realtime/translations/client_secrets"},
		{"POST", "/v1/realtime/calls/id/hangup"}, {"POST", "/v1/realtime/calls/id/accept"}, {"POST", "/v1/realtime/calls/id/reject"}, {"POST", "/v1/realtime/calls/id/refer"},
	} {
		t.Run(route.method+route.path, func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer test-key")
			req.Header.Set("X-CLIProxy-Auth-ID", "route-pin")
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			rec := httptest.NewRecorder()
			s.engine.ServeHTTP(rec, req)
			if rec.Code != 400 || !strings.Contains(rec.Body.String(), "server credential pin is unsupported for this endpoint") || e.executions != 0 || e.httpCalls != 0 {
				t.Fatalf("route escaped pin boundary: %d exec%d wire%d %s", rec.Code, e.executions, e.httpCalls, rec.Body.String())
			}
			t.Logf("route=%s status=%d provider=%d HTTP=%d upgrade=false", route.path, rec.Code, e.executions, e.httpCalls)
		})
	}
}
func TestSharedPinRegisteredAuthOrderAndNoPinCompatibility(t *testing.T) {
	s := newTestServer(t)
	e := &sharedPinRouteExecutor{}
	s.handlers.AuthManager.RegisterExecutor(e)
	if _, err := s.handlers.AuthManager.Register(t.Context(), &auth.Auth{ID: "route-generic", Provider: "codex", Status: auth.StatusActive}); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient("route-generic", "codex", []*registry.ModelInfo{{ID: "route-model"}})
	defer registry.GetGlobalRegistry().UnregisterClient("route-generic")
	originalManager := s.handlers.AuthManager
	for _, scenario := range []string{"unauthenticated-malformed", "wrong-auth-gone", "authority-missing", "nil-authority-no-header", "no-pin-chat", "valid-generic-chat", "malformed-chat", "no-pin-ws", "no-pin-content", "no-pin-alpha"} {
		t.Run(scenario, func(t *testing.T) {
			path, method := "/v1/chat/completions", "POST"
			if scenario == "no-pin-ws" {
				path = "/v1/responses"
				method = "GET"
			}
			if scenario == "no-pin-content" {
				path = "/openai/v1/videos/id/content"
				method = "GET"
			}
			if scenario == "no-pin-alpha" {
				path = "/v1/alpha/search"
			}
			req := httptest.NewRequest(method, path, strings.NewReader(`{"model":"route-model","messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer test-key")
			switch scenario {
			case "unauthenticated-malformed":
				req.Header.Del("Authorization")
				req.Header["X-CLIProxy-Auth-ID"] = []string{"A", "B"}
			case "wrong-auth-gone":
				req.Header.Set("Authorization", "Bearer wrong")
				req.Header.Set("X-CLIProxy-Auth-ID", "gone")
			case "authority-missing":
				s.handlers.AuthManager = nil
				req.Header.Set("X-CLIProxy-Auth-ID", "route-generic")
			case "nil-authority-no-header":
				s.handlers.AuthManager = nil
				path = "/v1/models"
				req = httptest.NewRequest("GET", path, nil)
				req.Header.Set("Authorization", "Bearer test-key")
			case "valid-generic-chat":
				req.Header.Set("X-CLIProxy-Auth-ID", "route-generic")
			case "malformed-chat":
				req.Header.Set("X-CLIProxy-Auth-ID", "A,B")
			}
			manager := s.handlers.AuthManager
			rec := httptest.NewRecorder()
			s.engine.ServeHTTP(rec, req)
			// Restore the fixture authority after the two nil-authority cases.
			if manager == nil {
				s.handlers.AuthManager = originalManager
			}
			switch scenario {
			case "unauthenticated-malformed", "wrong-auth-gone":
				if rec.Code != 401 {
					t.Fatalf("auth order status%d %s", rec.Code, rec.Body.String())
				}
			case "authority-missing":
				if rec.Code != 503 {
					t.Fatalf("missing authority %d", rec.Code)
				}
			case "no-pin-chat", "valid-generic-chat":
				if rec.Code != 200 {
					t.Fatalf("chat %d %s", rec.Code, rec.Body.String())
				}
			case "malformed-chat":
				if rec.Code != 400 {
					t.Fatal(rec.Code)
				}
			default:
				if strings.Contains(rec.Body.String(), "server credential pin") || rec.Code == 101 {
					t.Fatalf("no-pin behavior replaced %d %s", rec.Code, rec.Body.String())
				}
			}
		})
	}
}

func TestSharedPinRegisteredStandardPathsRejectMalformedBeforeDispatch(t *testing.T) {
	s := newTestServer(t)
	e := &sharedPinRouteExecutor{}
	s.handlers.AuthManager.RegisterExecutor(e)
	for _, path := range []string{"/v1/chat/completions", "/v1/completions", "/v1/responses", "/v1/responses/compact", "/backend-api/codex/responses/compact", "/v1/messages", "/v1/messages/count_tokens", "/v1/images/generations", "/v1/images/edits", "/v1/videos", "/v1/videos/generations", "/v1/videos/edits", "/v1/videos/extensions", "/openai/v1/videos", "/v1beta/models/test:generateContent"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer test-key")
			req.Header["x-CLIPROXY-auth-id"] = []string{}
			rec := httptest.NewRecorder()
			s.engine.ServeHTTP(rec, req)
			if rec.Code != 400 || !strings.Contains(rec.Body.String(), "exactly one nonempty server credential pin") || e.executions != 0 || e.httpCalls != 0 {
				t.Fatalf("registered path bypass %d exec%d wire%d %s", rec.Code, e.executions, e.httpCalls, rec.Body.String())
			}
		})
	}
}
