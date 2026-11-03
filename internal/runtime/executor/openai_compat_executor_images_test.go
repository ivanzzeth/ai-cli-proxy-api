package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOpenAICompatExecutor_ImageStreamChunkBoundaryObservability(t *testing.T) {
	tests := []struct {
		name      string
		chunks    []string
		wantModel string
	}{
		{
			name: "chunk split across json boundaries (拆包)",
			chunks: []string{
				`data: {"id":"img_split","cre`,
				`ated":123,"mo`,
				`del":"dall-e-3","data":[{"url":"http://example.com/1.png"}]}` + "\n\n",
			},
			wantModel: "dall-e-3",
		},
		{
			name: "multiple events in single network chunk (合包)",
			chunks: []string{
				"event: ping\ndata: {}\n\nevent: completion\ndata: {\"model\":\"dall-e-3\",\"status\":\"done\"}\n\n",
			},
			wantModel: "dall-e-3",
		},
		{
			name: "chunk starting with event: prefix",
			chunks: []string{
				"event: image_event\n",
				"data: {\"model\":\"dall-e-3\",\"data\":[]}\n\n",
			},
			wantModel: "dall-e-3",
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher, ok := w.(http.Flusher)
				for _, c := range tt.chunks {
					_, _ = w.Write([]byte(c))
					if ok {
						flusher.Flush()
					}
				}
			}))
			defer server.Close()

			alias := fmt.Sprintf("openai-compat-image-stream-test-%d", i)
			capture := &multiProviderUsageCapture{alias: alias, records: make(chan coreusage.Record, 4)}
			coreusage.RegisterNamedPlugin(t.Name(), capture)
			t.Cleanup(func() {
				coreusage.RegisterNamedPlugin(t.Name(), multiProviderNoopUsagePlugin{})
			})

			executor := NewOpenAICompatExecutor("openai-compatibility", &config.Config{
				OpenAICompatibility: []config.OpenAICompatibility{{
					Name: "compat",
				}},
			})
			auth := &cliproxyauth.Auth{
				Provider: "openai-compatibility",
				Attributes: map[string]string{
					"base_url":     server.URL,
					"api_key":      "test-key",
					"compat_name":  "compat",
					"provider_key": "compat",
				},
			}

			ctx := coreusage.WithRequestedModelAlias(context.Background(), alias)
			payload := []byte(`{"model":"dall-e-3","prompt":"a sunset over mountains"}`)
			streamRes, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
				Model:   "dall-e-3",
				Payload: payload,
			}, cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FromString("openai-image"),
				Stream:       true,
			})
			if err != nil {
				t.Fatalf("ExecuteStream error: %v", err)
			}

			// Verify verbatim forwarding: collect all streamed bytes
			var received bytes.Buffer
			for chunk := range streamRes.Chunks {
				if chunk.Err != nil {
					t.Fatalf("unexpected chunk error: %v", chunk.Err)
				}
				received.Write(chunk.Payload)
			}

			expectedForwarded := strings.Join(tt.chunks, "")
			if received.String() != expectedForwarded {
				t.Fatalf("forwarded payload mismatch: got %q, want %q", received.String(), expectedForwarded)
			}

			// Verify response model in recorded usage
			record := capture.await(t)
			if record.ResponseModel != tt.wantModel {
				t.Fatalf("recorded ResponseModel = %q, want %q", record.ResponseModel, tt.wantModel)
			}
		})
	}
}

const seedreamTestModel = "doubao-seedream-5pro-synthetic"
const seedreamTestProfile = "ark-agent-plan-seedream-5pro-v1"

func seedreamTestExecutor(origin, model, profile string) (*OpenAICompatExecutor, *cliproxyauth.Auth) {
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "seedream-fixture", Models: []config.OpenAICompatibilityModel{{Name: model, Image: true, ImageProfile: profile}}}}}
	return NewOpenAICompatExecutor("openai-compatibility", cfg), &cliproxyauth.Auth{ID: "synthetic-ark-only", Provider: "openai-compatibility", Attributes: map[string]string{"base_url": origin, "api_key": "synthetic-ark-key", "compat_name": "seedream-fixture", "provider_key": "seedream-fixture"}}
}

func TestSeedreamProfileJSONContract(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"one", `{"model":"ignored","prompt":"draw","size":"1K","n":1}`, true},
		{"two_k", `{"model":"ignored","prompt":"draw","size":"2K","watermark":false,"response_format":"b64_json","output_format":"png"}`, true},
		{"fractional_n", `{"prompt":"draw","size":"1K","n":1.9}`, false},
		{"lexical_one_decimal_restricted", `{"prompt":"draw","size":"1K","n":1.0}`, false},
		{"null_n", `{"prompt":"draw","size":"1K","n":null}`, false},
		{"null_stream", `{"prompt":"draw","size":"1K","stream":null}`, false},
		{"true_stream", `{"prompt":"draw","size":"1K","stream":true}`, false},
		{"null_watermark", `{"prompt":"draw","size":"1K","watermark":null}`, false},
		{"one_point_five_k", `{"prompt":"draw","size":"1.5K"}`, false},
		{"image_input_P2", `{"prompt":"draw","size":"1K","image":"https://example.invalid/image"}`, false},
		{"sequential", `{"prompt":"draw","size":"1K","sequential_image_generation":"auto"}`, false},
		{"duplicate_size", `{"prompt":"draw","size":"1K","size":"2K"}`, false},
		{"escaped_duplicate", `{"prompt":"draw","size":"1K","si\u007ae":"2K"}`, false},
		{"duplicate_model", `{"model":"a","model":"b","prompt":"draw","size":"1K"}`, false},
		{"duplicate_stream", `{"prompt":"draw","size":"1K","stream":false,"stream":false}`, false},
		{"two_documents", `{"prompt":"draw","size":"1K"} {}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeSeedreamGeneration([]byte(tc.body), "/images/generations", seedreamTestProfile, seedreamTestModel)
			if !tc.valid {
				if err == nil {
					t.Fatal("invalid profile request accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var value map[string]json.RawMessage
			if err := json.Unmarshal(got, &value); err != nil {
				t.Fatal(err)
			}
			var model string
			if err := json.Unmarshal(value["model"], &model); err != nil {
				t.Fatal(err)
			}
			if model != seedreamTestModel {
				t.Fatalf("wire model %q", model)
			}
			for _, key := range []string{"n", "stream", "sequential_image_generation"} {
				if _, ok := value[key]; ok {
					t.Fatalf("unexpected wire field %s", key)
				}
			}
		})
	}
}

func TestSeedreamExecutorActualHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, reply      string
		status           int
		closeAfterCommit bool
		wantError        bool
	}{
		{name: "one_1K", reply: `{"data":[{"b64_json":"aGVsbG8=","size":"1024x1024"}]}`, status: 200},
		{name: "zero_results", reply: `{"data":[]}`, status: 200, wantError: true},
		{name: "two_results", reply: `{"data":[{"url":"a","size":"1K"},{"url":"b","size":"1K"}]}`, status: 200, wantError: true},
		{name: "missing_size", reply: `{"data":[{"b64_json":"aGVsbG8="}]}`, status: 200, wantError: true},
		{name: "mismatched_size", reply: `{"data":[{"b64_json":"aGVsbG8=","size":"2K"}]}`, status: 200, wantError: true},
		{name: "unknown_tier", reply: `{"data":[{"b64_json":"aGVsbG8=","size":"1.5K"}]}`, status: 200, wantError: true},
		{name: "upstream_401", reply: `{"error":{"message":"synthetic"}}`, status: 401, wantError: true},
		{name: "commit_then_EOF", closeAfterCommit: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/images/generations" {
					t.Errorf("path=%s", r.URL.Path)
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				var value map[string]any
				if err := json.Unmarshal(raw, &value); err != nil {
					t.Error(err)
				}
				if value["model"] != seedreamTestModel {
					t.Errorf("model=%v", value["model"])
				}
				if _, ok := value["n"]; ok {
					t.Error("n leaked")
				}
				if r.Header.Get("Authorization") != "Bearer synthetic-ark-key" {
					t.Error("wrong credential")
				}
				if tc.closeAfterCommit {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.reply)
			}))
			defer provider.Close()
			executor, auth := seedreamTestExecutor(provider.URL, seedreamTestModel, seedreamTestProfile)
			got, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: seedreamTestModel, Payload: []byte(`{"model":"customer-other","prompt":"draw","size":"1K","n":1}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-image")})
			if calls.Load() != 1 {
				t.Fatalf("provider calls=%d", calls.Load())
			}
			if tc.wantError {
				var signal interface{ NonReplayableImageAttempt() bool }
				if err == nil || !errors.As(err, &signal) || !signal.NonReplayableImageAttempt() {
					t.Fatalf("no typed stop: %v", err)
				}
				if !strings.Contains(err.Error(), "image_generation_outcome_unknown") {
					t.Fatalf("unsafe error: %v", err)
				}
			} else if err != nil || !bytes.Equal(got.Payload, []byte(tc.reply)) {
				t.Fatalf("response=%s err=%v", got.Payload, err)
			}
		})
	}
}

func TestSeedreamExecutorNoRedirectOrPrewriteVendorCall(t *testing.T) {
	for _, code := range []int{307, 308} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var first, next atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { next.Add(1); w.WriteHeader(200) }))
			defer target.Close()
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				first.Add(1)
				w.Header().Set("Location", target.URL)
				w.WriteHeader(code)
			}))
			defer origin.Close()
			executor, auth := seedreamTestExecutor(origin.URL, seedreamTestModel, seedreamTestProfile)
			_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: seedreamTestModel, Payload: []byte(`{"prompt":"draw","size":"1K"}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-image")})
			if err == nil || first.Load() != 1 || next.Load() != 0 {
				t.Fatalf("err=%v first=%d target=%d", err, first.Load(), next.Load())
			}
		})
	}
	for _, body := range []string{`{"prompt":"draw","size":"1.5K"}`, `{"prompt":"draw","size":"1K","n":2}`, `{"prompt":"draw","size":"1K","image":"x"}`} {
		t.Run(body, func(t *testing.T) {
			var calls atomic.Int32
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
			defer origin.Close()
			executor, auth := seedreamTestExecutor(origin.URL, seedreamTestModel, seedreamTestProfile)
			_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: seedreamTestModel, Payload: []byte(body)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-image")})
			if err == nil || calls.Load() != 0 {
				t.Fatalf("prewrite err=%v calls=%d", err, calls.Load())
			}
		})
	}
}

func TestSeedreamExplicitProfileRequiredBeforeHTTP(t *testing.T) {
	for _, profile := range []string{"", "unsupported-version"} {
		t.Run(profile, func(t *testing.T) {
			var calls atomic.Int32
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
			defer origin.Close()
			executor, auth := seedreamTestExecutor(origin.URL, seedreamTestModel, profile)
			_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: seedreamTestModel, Payload: []byte(`{"prompt":"draw","size":"1K"}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-image")})
			if err == nil || calls.Load() != 0 {
				t.Fatalf("profile=%q err=%v provider=%d", profile, err, calls.Load())
			}
			var attempted interface{ NonReplayableImageAttempt() bool }
			if errors.As(err, &attempted) {
				t.Fatal("prewrite failure mislabeled postattempt")
			}
		})
	}
}
func TestSeedreamRegistryMetadataIsGenOnlyAndCloned(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	id := "seedream-metadata-" + t.Name()
	client := "client-" + t.Name()
	reg.RegisterClient(client, "openai-compatibility", []*registry.ModelInfo{{ID: id, ImageGeneration: registry.SeedreamImageProfile(seedreamTestProfile)}})
	t.Cleanup(func() { reg.UnregisterClient(client) })
	info := reg.GetModelInfo(id, "openai-compatibility")
	if info == nil || info.ImageGeneration == nil {
		t.Fatal("profile metadata missing")
	}
	if len(info.ImageGeneration.SupportedEndpoints) != 1 || info.ImageGeneration.SupportedEndpoints[0] != "/v1/images/generations" || info.ImageGeneration.MaxImages != 1 || info.ImageGeneration.Stream {
		t.Fatalf("unsupported image claims: %+v", info.ImageGeneration)
	}
	info.ImageGeneration.Sizes[0] = "mutated"
	again := reg.GetModelInfo(id, "openai-compatibility")
	if again.ImageGeneration.Sizes[0] != "1K" {
		t.Fatal("metadata shared mutable slice")
	}
	for _, row := range reg.GetAvailableModels("openai") {
		if row["id"] == id {
			if row["image_generation"] == nil {
				t.Fatal("public metadata missing")
			}
			return
		}
	}
	t.Fatal("registered model missing from public registry")
}

// The Plan alias is a public documented name; the server here is synthetic and
// this test proves exact routing/normalization, not real subscription entitlement.
func TestSeedreamFlashPlanAliasHTTP(t *testing.T) {
	const model = "doubao-seedream-5.0-flash"
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/plan/v3/images/generations" {
			t.Errorf("path=%q", r.URL.Path)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Error(err)
		}
		if value["model"] != model || value["size"] != "2K" {
			t.Errorf("unexpected model/size: %v", value)
		}
		if _, ok := value["n"]; ok {
			t.Error("OpenAI n must be normalized away")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"b64_json":"aGVsbG8=","size":"2K"}]}`)
	}))
	defer provider.Close()
	executor, auth := seedreamTestExecutor(provider.URL+"/api/plan/v3", model, "ark-agent-plan-seedream-5flash-v1")
	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: model, Payload: []byte(`{"model":"customer-stale","prompt":"synthetic diagram","size":"2K","n":1}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-image")})
	if err != nil || calls.Load() != 1 {
		t.Fatalf("err=%v provider=%d", err, calls.Load())
	}
}
