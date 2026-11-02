package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestXAIReservedPinRemovedAfterCustomHeadersAtWire(t *testing.T) {
	for _, mode := range []string{"static", "client-reference", "mixedcase"} {
		for _, stream := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "/nonstream", true: "/stream"}[stream], func(t *testing.T) {
				var calls atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					for name := range r.Header {
						if strings.EqualFold(name, "X-CLIProxy-Auth-ID") {
							t.Error("reserved pin survived native custom headers")
						}
					}
					if r.Header.Get("X-Normal") != "retained" || r.Header.Get("X-Client-Copy") != "normal-client" {
						t.Error("unrelated native custom header behavior changed")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_reserved\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"grok-4.6\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
				}))
				defer srv.Close()
				key, value := "header:X-CLIProxy-Auth-ID", "static-private-pin"
				if mode == "client-reference" {
					value = "$X-CLIProxy-Auth-ID"
				}
				if mode == "mixedcase" {
					key = "header:x-cLiPrOxY-aUtH-iD"
					value = "$X-CLIProxy-Auth-ID"
				}
				auth := &cliproxyauth.Auth{ID: "reserved-test", Provider: "xai", Attributes: map[string]string{"api_key": "fixture", "base_url": srv.URL + "/v1", key: value, "header:X-Normal": "retained", "header:X-Client-Copy": "$X-Normal-Client"}}
				payload := []byte(`{"model":"grok-4.6","input":"hello"}`)
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse, Headers: http.Header{"X-CLIProxy-Auth-ID": []string{"server-private-pin"}, "X-Normal-Client": []string{"normal-client"}}}
				exec := NewXAIExecutor(&config.Config{})
				if stream {
					out, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "grok-4.6", Payload: payload}, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range out.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else {
					if _, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "grok-4.6", Payload: payload}, opts); err != nil {
						t.Fatal(err)
					}
				}
				if calls.Load() != 1 {
					t.Fatalf("wire calls=%d", calls.Load())
				}
			})
		}
	}
}
