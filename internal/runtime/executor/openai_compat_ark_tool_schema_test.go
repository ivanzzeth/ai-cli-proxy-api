package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	native "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

const arkWireSchema = `{"type":"object","$defs":{"query":{"type":"string","minLength":2}},"properties":{"query":{"$ref":"#/$defs/query"}},"required":["query"],"additionalProperties":false,"default":{"$ref":"literal-instance"}}`

func arkWirePayload(format translator.Format, schema string, stream bool) []byte {
	switch format {
	case translator.FormatClaude:
		return []byte(fmt.Sprintf(`{"model":"client-model","max_tokens":64,"stream":%t,"messages":[{"role":"user","content":"lookup"}],"tools":[{"name":"lookup","description":"fixture","input_schema":%s}]}`, stream, schema))
	case translator.FormatOpenAIResponse:
		return []byte(fmt.Sprintf(`{"model":"client-model","stream":%t,"input":"lookup","tools":[{"type":"function","name":"lookup","description":"fixture","parameters":%s}]}`, stream, schema))
	default:
		return []byte(fmt.Sprintf(`{"model":"client-model","stream":%t,"messages":[{"role":"user","content":"lookup"}],"tools":[{"type":"function","function":{"name":"lookup","description":"fixture","parameters":%s}}]}`, stream, schema))
	}
}
func arkWireExecutor(base string, protocol registry.UpstreamProtocol) (*OpenAICompatExecutor, *auth.Auth) {
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "plan-text", BaseURL: base, Models: []config.OpenAICompatibilityModel{{Name: "upstream-model", Alias: "client-model", UpstreamProtocol: protocol}}}}}
	credential := &auth.Auth{ID: "ark-schema-fixture", Provider: "plan-text", Status: auth.StatusActive, Attributes: map[string]string{"api_key": "synthetic-provider-key", "base_url": base, "compat_name": "plan-text", "source": "config:plan-text[0]"}}
	return NewOpenAICompatExecutor("plan-text", cfg), credential
}
func arkWireResponse(w http.ResponseWriter, stream bool) {
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-fixture","object":"chat.completion","created":1,"model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call-fixture","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"valid\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "data: {\"id\":\"chat-fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call-fixture\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"query\\\":\"}}]},\"finish_reason\":null}]}\n\n")
	_, _ = io.WriteString(w, "data: {\"id\":\"chat-fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"valid\\\"}\"}}]},\"finish_reason\":null}]}\n\n")
	_, _ = io.WriteString(w, "data: {\"id\":\"chat-fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\ndata: [DONE]\n\n")
}

func TestOpenAICompatArkToolSchemaThreeFormats(t *testing.T) {
	for _, format := range []translator.Format{translator.FormatOpenAI, translator.FormatOpenAIResponse, translator.FormatClaude} {
		for _, stream := range []bool{false, true} {
			for _, protocol := range []registry.UpstreamProtocol{registry.UpstreamProtocolVolcengineArkTextV3, registry.UpstreamProtocolLegacy} {
				t.Run(fmt.Sprintf("%s/stream=%t/protocol=%s", format, stream, protocol), func(t *testing.T) {
					var observed []byte
					var upstreamCalls atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						upstreamCalls.Add(1)
						if r.URL.Path != "/api/plan/v3/chat/completions" {
							t.Errorf("wrong native path %s", r.URL.Path)
						}
						if r.Header.Get("Authorization") != "Bearer synthetic-provider-key" {
							t.Error("selected provider credential lost")
						}
						observed, _ = io.ReadAll(r.Body)
						arkWireResponse(w, stream)
					}))
					defer server.Close()
					executor, credential := arkWireExecutor(server.URL+"/api/plan/v3", protocol)
					payload := arkWirePayload(format, arkWireSchema, stream)
					canonical := bytes.Clone(payload)
					req := native.Request{Model: "upstream-model", Payload: payload}
					opts := native.Options{SourceFormat: format, OriginalRequest: payload, Stream: stream}
					var output []byte
					if stream {
						result, err := executor.ExecuteStream(context.Background(), credential, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							output = append(output, chunk.Payload...)
						}
					} else {
						result, err := executor.Execute(context.Background(), credential, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						output = result.Payload
					}
					if upstreamCalls.Load() != 1 {
						t.Fatalf("upstream calls %d", upstreamCalls.Load())
					}
					if !bytes.Equal(payload, canonical) {
						t.Fatal("canonical request mutated")
					}
					params := gjson.GetBytes(observed, "tools.0.function.parameters")
					hasRef := params.Get("properties.query.$ref").Exists()
					if hasRef != (protocol == registry.UpstreamProtocolLegacy) {
						t.Fatalf("wrong schema adaptation: %s", params.Raw)
					}
					if params.Get("default.$ref").String() != "literal-instance" {
						t.Fatal("instance annotation changed")
					}
					if protocol == registry.UpstreamProtocolVolcengineArkTextV3 {
						if params.Get("properties.query.minLength").Int() != 2 || params.Get("additionalProperties").Bool() || !params.Get("required.0").Exists() {
							t.Fatalf("constraints lost: %s", params.Raw)
						}
					}
					if !bytes.Contains(output, []byte("lookup")) || !bytes.Contains(output, []byte("valid")) {
						t.Fatalf("native tool result lost: %s", output)
					}
					if !stream {
						input, completion := "usage.prompt_tokens", "usage.completion_tokens"
						if format != translator.FormatOpenAI {
							input, completion = "usage.input_tokens", "usage.output_tokens"
						}
						if gjson.GetBytes(output, input).Int() != 10 || gjson.GetBytes(output, completion).Int() != 5 {
							t.Fatalf("usage changed: %s", output)
						}
					} else {
						marker := `"finish_reason":"tool_calls"`
						if format == translator.FormatClaude {
							marker = "message_stop"
						}
						if format == translator.FormatOpenAIResponse {
							marker = "response.completed"
						}
						if !bytes.Contains(output, []byte(marker)) {
							t.Fatalf("native stream termination lost: %s", output)
						}
					}
				})
			}
		}
	}
}
func TestOpenAICompatArkToolSchemaFailureBeforeHTTP(t *testing.T) {
	for _, format := range []translator.Format{translator.FormatOpenAI, translator.FormatOpenAIResponse, translator.FormatClaude} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", format, stream), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); arkWireResponse(w, stream) }))
				defer server.Close()
				executor, credential := arkWireExecutor(server.URL, registry.UpstreamProtocolVolcengineArkTextV3)
				payload := arkWirePayload(format, `{"type":"object","properties":{"x":{"$ref":"#"}}}`, stream)
				req := native.Request{Model: "upstream-model", Payload: payload}
				opts := native.Options{SourceFormat: format, OriginalRequest: payload, Stream: stream}
				var err error
				if stream {
					_, err = executor.ExecuteStream(context.Background(), credential, req, opts)
				} else {
					_, err = executor.Execute(context.Background(), credential, req, opts)
				}
				status, ok := err.(interface{ StatusCode() int })
				if !ok || status.StatusCode() != 400 {
					t.Fatalf("want typed 400, got %v", err)
				}
				if calls.Load() != 0 {
					t.Fatal("invalid schema reached upstream")
				}
			})
		}
	}
}
func TestOpenAICompatArkToolSchemaAfterPayloadConfig(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var observed []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				observed, _ = io.ReadAll(r.Body)
				arkWireResponse(w, stream)
			}))
			defer server.Close()
			executor, credential := arkWireExecutor(server.URL, registry.UpstreamProtocolVolcengineArkTextV3)
			executor.cfg.Payload = config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "upstream-model", Protocol: "openai"}}, Params: map[string]any{"tools.0.function.parameters": arkDecode(t, arkWireSchema)}}}}
			payload := arkWirePayload(translator.FormatOpenAI, `{"type":"object"}`, stream)
			req := native.Request{Model: "upstream-model", Payload: payload}
			opts := native.Options{SourceFormat: translator.FormatOpenAI, OriginalRequest: payload, Stream: stream}
			if stream {
				result, err := executor.ExecuteStream(context.Background(), credential, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else {
				if _, err := executor.Execute(context.Background(), credential, req, opts); err != nil {
					t.Fatal(err)
				}
			}
			if !gjson.GetBytes(observed, "tools.0.function.parameters.properties.query.minLength").Exists() {
				t.Fatalf("final config schema missed: %s", observed)
			}
		})
	}
}
func TestOpenAICompatArkToolSchemaNoOp(t *testing.T) {
	executor, credential := arkWireExecutor("https://not-called.invalid", registry.UpstreamProtocolVolcengineArkTextV3)
	for name, payload := range map[string][]byte{"no refs": arkWirePayload(translator.FormatOpenAI, `{"type":"object","$defs":{"unused":{"type":"string"}}}`, false), "no tools": []byte(`{"model":"upstream-model","messages":[]}`), "instance only": arkWirePayload(translator.FormatOpenAI, `{"type":"object","default":{"$ref":"literal"}}`, false)} {
		t.Run(name, func(t *testing.T) {
			got, err := executor.normalizeArkToolSchemas(credential, "upstream-model", payload, true)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatal("no-op request changed")
			}
		})
	}
	payload := arkWirePayload(translator.FormatOpenAI, arkWireSchema, false)
	got, err := executor.normalizeArkToolSchemas(credential, "upstream-model", payload, false)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatal("compact contract changed")
	}
}

func TestOpenAICompatArkToolSchemaLegacyByteIdentity(t *testing.T) {
	executor, credential := arkWireExecutor("https://ark-brand.example.invalid/api/plan/v3", registry.UpstreamProtocolLegacy)
	payload := arkWirePayload(translator.FormatOpenAI, arkWireSchema, false)
	got, err := executor.normalizeArkToolSchemas(credential, "upstream-model", payload, true)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatal("undeclared Ark inferred from URL or schema")
	}
}

func TestArkToolSchemaReviewEscapedRef(t *testing.T) {
	executor, credential := arkWireExecutor("https://not-called.invalid", registry.UpstreamProtocolVolcengineArkTextV3)
	payload := arkWirePayload("openai", `{"type":"object","$defs":{"x":{"type":"string"}},"properties":{"x":{"$\u0072ef":"#/$defs/x"}}}`, false)
	got, err := executor.normalizeArkToolSchemas(credential, "upstream-model", payload, true)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(got, "tools.0.function.parameters.properties.x.$ref").Exists() {
		t.Fatalf("escaped schema ref left on wire: %s", got)
	}
}

func TestOpenAICompatArkEscapedReferences(t *testing.T) {
	for _, format := range []translator.Format{translator.FormatOpenAI, translator.FormatOpenAIResponse, translator.FormatClaude} {
		for _, stream := range []bool{false, true} {
			for _, key := range []string{`$\u0072ef`, `\u0024ref`, `\u0024\u0072\u0065\u0066`} {
				t.Run(fmt.Sprintf("%s/stream=%t/key=%s", format, stream, key), func(t *testing.T) {
					var observed []byte
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						observed, _ = io.ReadAll(r.Body)
						arkWireResponse(w, stream)
					}))
					defer server.Close()
					executor, credential := arkWireExecutor(server.URL, registry.UpstreamProtocolVolcengineArkTextV3)
					schema := fmt.Sprintf(`{"type":"object","$defs":{"x":{"type":"string"}},"properties":{"x":{"%s":"#/$defs/x"}}}`, key)
					payload := arkWirePayload(format, schema, stream)
					canonical := bytes.Clone(payload)
					req := native.Request{Model: "upstream-model", Payload: payload}
					opts := native.Options{SourceFormat: format, OriginalRequest: payload, Stream: stream}
					if stream {
						result, err := executor.ExecuteStream(context.Background(), credential, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else {
						if _, err := executor.Execute(context.Background(), credential, req, opts); err != nil {
							t.Fatal(err)
						}
					}
					params := gjson.GetBytes(observed, "tools.0.function.parameters")
					if params.Get("properties.x.$ref").Exists() || params.Get("properties.x.type").String() != "string" {
						t.Fatalf("escaped reference not adapted: %s", params.Raw)
					}
					if !bytes.Equal(payload, canonical) {
						t.Fatal("canonical escaped input changed")
					}
				})
			}
		}
	}
}
func TestOpenAICompatArkEscapedInvalidReferencesBeforeHTTP(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, schema := range []string{`{"$\u0072ef":"#"}`, `{"\u0024ref":"https://example.invalid/never-fetch"}`, `{"$dynamicAnchor":"x","$d\u0079namicRef":"#x"}`, `{"$schema":"https://json-schema.org/draft/2019-09/schema","$recursiveAnchor":true,"$rec\u0075rsiveRef":"#"}`} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, schema), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); arkWireResponse(w, stream) }))
				defer server.Close()
				executor, credential := arkWireExecutor(server.URL, registry.UpstreamProtocolVolcengineArkTextV3)
				payload := arkWirePayload(translator.FormatOpenAI, schema, stream)
				req := native.Request{Model: "upstream-model", Payload: payload}
				opts := native.Options{SourceFormat: translator.FormatOpenAI, OriginalRequest: payload, Stream: stream}
				var err error
				if stream {
					_, err = executor.ExecuteStream(context.Background(), credential, req, opts)
				} else {
					_, err = executor.Execute(context.Background(), credential, req, opts)
				}
				status, ok := err.(interface{ StatusCode() int })
				if !ok || status.StatusCode() != 400 || calls.Load() != 0 {
					t.Fatalf("escaped invalid schema escaped typed-400/zero-HTTP guard: %v calls=%d", err, calls.Load())
				}
			})
		}
	}
}
