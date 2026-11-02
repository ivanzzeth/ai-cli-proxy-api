package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	native "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type voiceConductorExecutor struct {
	compactTestExecutor
	target    string
	refreshes atomic.Int32
}

func (*voiceConductorExecutor) Identifier() string { return "xai" }
func (e *voiceConductorExecutor) Execute(ctx context.Context, credential *Auth, req native.Request, opts native.Options) (native.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.target, strings.NewReader(string(req.Payload)))
	if err != nil {
		return native.Response{}, err
	}
	request.Header.Set("X-Test-Auth", credential.ID)
	native.MarkUpstreamAttempt(ctx)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return native.Response{}, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return native.Response{}, voiceConductorError{status: response.StatusCode}
}
func (e *voiceConductorExecutor) Refresh(ctx context.Context, credential *Auth) (*Auth, error) {
	e.refreshes.Add(1)
	return credential, nil
}

type voiceConductorError struct{ status int }

func (e voiceConductorError) Error() string              { return fmt.Sprint(e.status) }
func (e voiceConductorError) StatusCode() int            { return e.status }
func (e voiceConductorError) RetryAfter() *time.Duration { d := 5 * time.Second; return &d }
func TestVoiceConductorSingleAttemptMarksResultWithoutAnyReplay(t *testing.T) {
	for _, status := range []int{401, 429, 0} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var wires atomic.Int32
			var b atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wires.Add(1)
				if r.Header.Get("X-Test-Auth") == "B" {
					b.Add(1)
				}
				_, _ = io.Copy(io.Discard, r.Body)
				if status == 0 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = conn.Close()
					}
					return
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			executor := &voiceConductorExecutor{target: server.URL}
			hook := &recordingHook{}
			manager := NewManager(nil, nil, hook)
			manager.RegisterExecutor(executor)
			manager.SetRetryConfig(3, 10*time.Second, 3)
			for _, id := range []string{"A", "B"} {
				credential := &Auth{ID: id, Provider: "xai", Status: StatusActive, Attributes: map[string]string{"api_key": "fixture"}}
				if _, err := manager.Register(context.Background(), credential); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(id, "xai", []*registry.ModelInfo{{ID: "voice-wire"}})
				defer registry.GetGlobalRegistry().UnregisterClient(id)
			}
			_, err := manager.Execute(context.Background(), []string{"xai"}, native.Request{Model: "voice-wire", Payload: []byte(`{}`)}, native.Options{NativeVoiceOperation: native.NativeVoiceTTS, SingleAttempt: true, Metadata: map[string]any{native.PinnedAuthMetadataKey: "A"}})
			if err == nil || wires.Load() != 1 || b.Load() != 0 || executor.refreshes.Load() != 0 {
				t.Fatalf("err=%v wires=%d B=%d refresh=%d", err, wires.Load(), b.Load(), executor.refreshes.Load())
			}
			snapshot, ok := manager.GetByID("A")
			if !ok || snapshot == nil || hook.lastResult.Load() == nil || hook.lastResult.Load().AuthID != "A" || hook.lastResult.Load().Success {
				t.Fatal("native MarkResult hook missing")
			}
			if status == 429 && hook.lastResult.Load().RetryAfter == nil {
				t.Fatal("native RetryAfter result classification lost")
			}
			t.Logf("manager_actual_wire_count status=%d wires=%d refresh=%d SourceB=%d", status, wires.Load(), executor.refreshes.Load(), b.Load())
		})
	}
}
