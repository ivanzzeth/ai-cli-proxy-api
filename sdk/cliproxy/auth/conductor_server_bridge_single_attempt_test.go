package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	native "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestPinnedServerBridgeSingleAttemptScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts native.Options
		want bool
	}{
		{"authorized", native.Options{SingleAttempt: true, Metadata: map[string]any{"server_bridge_single_attempt": true, native.PinnedAuthMetadataKey: "A"}}, true},
		{"unrelated-pinned", native.Options{Metadata: map[string]any{native.PinnedAuthMetadataKey: "A"}}, false},
		{"untrusted-unpinned-policy", native.Options{SingleAttempt: true, Metadata: map[string]any{"server_bridge_single_attempt": true}}, false},
		{"pin-alone-not-policy", native.Options{SingleAttempt: true, Metadata: map[string]any{native.PinnedAuthMetadataKey: "A"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pinnedBridgeSingleAttempt(tc.opts); got != tc.want {
				t.Fatalf("got%v want%v", got, tc.want)
			}
		})
	}
}

type bridgeConductorExecutor struct {
	voiceConductorExecutor
	afterChunk bool
}

func (*bridgeConductorExecutor) Identifier() string { return "gemini" }

func (e *bridgeConductorExecutor) ExecuteStream(ctx context.Context, credential *Auth, request native.Request, options native.Options) (*native.StreamResult, error) {
	_, err := e.voiceConductorExecutor.Execute(ctx, credential, request, options)
	if !e.afterChunk {
		return nil, err
	}
	status, ok := err.(voiceConductorError)
	if !ok || status.status != http.StatusOK {
		return nil, err
	}
	chunks := make(chan native.StreamChunk, 2)
	chunks <- native.StreamChunk{Payload: []byte("first")}
	chunks <- native.StreamChunk{Err: voiceConductorError{status: http.StatusServiceUnavailable}}
	close(chunks)
	return &native.StreamResult{Chunks: chunks}, nil
}

func TestPinnedServerBridgeManagerDoesNotReplay(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		stream     bool
		afterChunk bool
		protected  bool
	}{
		{name: "sync-unauthorized", status: 401, protected: true},
		{name: "sync-rate-limited", status: 429, protected: true},
		{name: "sync-unknown-after-write", protected: true},
		{name: "stream-bootstrap-rate-limited", status: 429, stream: true, protected: true},
		{name: "stream-error-after-first-chunk", status: 200, stream: true, afterChunk: true, protected: true},
		{name: "unprotected-legacy-still-tries-other-credential", status: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wires, other atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				wires.Add(1)
				if request.Header.Get("X-Test-Auth") == "B" {
					other.Add(1)
				}
				if tc.status == 0 {
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					if err := connection.Close(); err != nil {
						t.Error(err)
					}
					return
				}
				w.WriteHeader(tc.status)
			}))
			defer upstream.Close()
			executor := &bridgeConductorExecutor{voiceConductorExecutor: voiceConductorExecutor{target: upstream.URL}, afterChunk: tc.afterChunk}
			hook := &recordingHook{}
			manager := NewManager(nil, nil, hook)
			manager.RegisterExecutor(executor)
			manager.SetRetryConfig(3, 0, 3)
			for _, id := range []string{"A", "B"} {
				if _, err := manager.Register(context.Background(), &Auth{ID: id, Provider: "gemini", Status: StatusActive, Attributes: map[string]string{"api_key": "fixture"}}); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(id, "gemini", []*registry.ModelInfo{{ID: "bridge-wire"}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			}
			options := native.Options{}
			if tc.protected {
				options.SingleAttempt = true
				options.Metadata = map[string]any{"server_bridge_single_attempt": true, native.PinnedAuthMetadataKey: "A"}
			}
			request := native.Request{Model: "bridge-wire", Payload: []byte("{}")}
			var err error
			if tc.stream {
				var result *native.StreamResult
				result, err = manager.ExecuteStream(context.Background(), []string{"gemini"}, request, options)
				if tc.afterChunk {
					if err != nil || result == nil {
						t.Fatalf("stream did not start: %v", err)
					}
					var payload, failure bool
					for chunk := range result.Chunks {
						payload = payload || string(chunk.Payload) == "first"
						failure = failure || chunk.Err != nil
					}
					if !payload || !failure {
						t.Fatal("stream did not expose its first chunk and terminal failure")
					}
				}
			} else {
				_, err = manager.Execute(context.Background(), []string{"gemini"}, request, options)
			}
			if !tc.afterChunk && err == nil {
				t.Fatal("constructed upstream failure was not experienced")
			}
			if tc.protected {
				if wires.Load() != 1 || other.Load() != 0 || executor.refreshes.Load() != 0 {
					t.Fatalf("protected request replayed: wires=%d B=%d refresh=%d err=%v", wires.Load(), other.Load(), executor.refreshes.Load(), err)
				}
			} else if wires.Load() < 2 || other.Load() == 0 {
				t.Fatal("unprotected legacy control was silently forced to one attempt", fmt.Sprint(wires.Load()), other.Load(), err)
			}
		})
	}
}
