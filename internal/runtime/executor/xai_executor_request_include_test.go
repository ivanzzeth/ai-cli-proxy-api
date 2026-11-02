package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	native "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestXAIResponsesExplicitIncludePrepareAndWire(t *testing.T) {
	for _, tc := range []struct {
		name, include, want, originalInclude string
		rejected                             bool
	}{
		{name: "web sources", include: `["web_search_call.action.sources"]`, want: `["web_search_call.action.sources"]`},
		{name: "no inline", include: `["no_inline_citations"]`, want: `["no_inline_citations"]`},
		{name: "empty explicit", include: `[]`, want: `[]`},
		{name: "absent old default", want: `["reasoning.encrypted_content"]`},
		{name: "effective absent does not resurrect original include", want: `["reasoning.encrypted_content"]`, originalInclude: `["web_search_call.action.sources"]`},
		{name: "explicit null delegates validation", include: `null`, want: `null`, rejected: true},
		{name: "invalid string delegates validation", include: `"not-an-array"`, want: `"not-an-array"`, rejected: true},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				fields := `"model":"grok-4.6","input":"search fixture","tools":[{"type":"web_search"}],"prompt_cache_key":"task23-explicit-cache","store":false`
				if tc.include != "" {
					fields += `,"include":` + tc.include
				}
				body := []byte(`{` + fields + `}`)
				original := bytes.Clone(body)
				check := func(raw []byte) {
					t.Helper()
					if got := gjson.GetBytes(raw, "include").Raw; got != tc.want {
						t.Fatalf("include got%s want%s", got, tc.want)
					}
					if gjson.GetBytes(raw, "prompt_cache_key").String() != "task23-explicit-cache" {
						t.Fatal("cache key lost")
					}
					if gjson.GetBytes(raw, "model").String() != "grok-4.6" || gjson.GetBytes(raw, "tools.0.type").String() != "web_search" || gjson.GetBytes(raw, "store").Bool() {
						t.Fatal("unrelated model/tools/store changed")
					}
				}
				originalRequest := body
				if tc.originalInclude != "" {
					originalRequest = []byte(`{` + fields + `,"include":` + tc.originalInclude + `}`)
				}
				exec := NewXAIExecutor(&config.Config{})
				opts := native.Options{SourceFormat: translator.FormatOpenAIResponse, ResponseFormat: translator.FormatOpenAIResponse, OriginalRequest: originalRequest}
				prepared, err := exec.prepareResponsesRequest(context.Background(), native.Request{Model: "grok-4.6", Payload: body}, opts, true)
				if err != nil {
					t.Fatal(err)
				}
				check(prepared.body)
				calls := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if r.Header.Get("Authorization") != "Bearer fixture" {
						t.Error("selected credential changed")
					}
					raw, e := io.ReadAll(r.Body)
					if e != nil {
						t.Error(e)
						return
					}
					check(raw)
					if !gjson.GetBytes(raw, "stream").Bool() {
						t.Error("native upstream SSE requirement changed")
					}
					if tc.rejected {
						w.WriteHeader(422)
						_, _ = w.Write([]byte(`{"error":"fixture invalid include"}`))
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_include\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"grok-4.6\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
				}))
				defer srv.Close()
				credential := &auth.Auth{ID: "task23-native", Provider: "xai", Attributes: map[string]string{"api_key": "fixture", "base_url": srv.URL + "/v1"}}
				if stream {
					result, e := exec.ExecuteStream(context.Background(), credential, native.Request{Model: "grok-4.6", Payload: body}, opts)
					err = e
					if e == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					}
				} else {
					_, err = exec.Execute(context.Background(), credential, native.Request{Model: "grok-4.6", Payload: body}, opts)
				}
				if tc.rejected {
					if err == nil {
						t.Fatal("native API include rejection hidden")
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if calls != 1 || !bytes.Equal(body, original) {
					t.Fatal("request replayed or original body mutated")
				}
			})
		}
	}
}

func TestXAIIncludeOtherEntryFormatsKeepCodexDefaults(t *testing.T) {
	for _, tc := range []struct {
		name string
		from translator.Format
		body string
	}{
		{"chat", translator.FormatOpenAI, `{"model":"grok-4.6","messages":[{"role":"user","content":"fixture"}],"include":["not-chat-semantics"]}`},
		{"claude", translator.FormatClaude, `{"model":"grok-4.6","max_tokens":32,"messages":[{"role":"user","content":"fixture"}],"include":["not-claude-semantics"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepared, err := NewXAIExecutor(&config.Config{}).prepareResponsesRequest(context.Background(), native.Request{Model: "grok-4.6", Payload: []byte(tc.body)}, native.Options{SourceFormat: tc.from}, true)
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(prepared.body, "include").Raw; got != `["reasoning.encrypted_content"]` {
				t.Fatalf("nonResponses include gained new meaning: %s", got)
			}
		})
	}
}

func TestXAIResponsesIncludeUsesCurrentEffectivePayload(t *testing.T) {
	current := []byte(`{"model":"grok-4.6","input":"fixture","include":[]}`)
	original := []byte(`{"model":"grok-4.6","input":"fixture","include":["web_search_call.action.sources"]}`)
	prepared, err := NewXAIExecutor(&config.Config{}).prepareResponsesRequest(context.Background(), native.Request{Model: "grok-4.6", Payload: current}, native.Options{SourceFormat: translator.FormatOpenAIResponse, OriginalRequest: original}, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(gjson.GetBytes(prepared.body, "include").Raw) != "[]" {
		t.Fatal("original request resurrected an include removed/changed in current payload")
	}
}
