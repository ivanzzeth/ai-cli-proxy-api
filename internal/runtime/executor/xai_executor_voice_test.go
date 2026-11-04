package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	native "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func voiceFixtureAuth(base string) *auth.Auth {
	return &auth.Auth{ID: "native-A", Provider: "xai", Status: auth.StatusActive, Attributes: map[string]string{"api_key": "fixture-secret", "base_url": base, "source": "config:xai[opaque]", "header:X-CLIProxy-Auth-ID": "must-strip"}}
}
func voiceFixtureOptions(operation native.NativeVoiceHTTPOperation) native.Options {
	return native.Options{NativeVoiceOperation: operation, SingleAttempt: operation.Unsafe(), Headers: http.Header{"Content-Type": []string{"multipart/form-data; boundary=fixture"}, native.NativeVoiceAuthIDHeader: []string{"inbound-pin"}}, Query: url.Values{"_native_voice_id": []string{"voice-one"}}}
}
func TestVoiceHTTPCommittedDisconnectActualWireOnce(t *testing.T) {
	for _, op := range []native.NativeVoiceHTTPOperation{native.NativeVoiceTTS, native.NativeVoicePatch, native.NativeVoiceDelete} {
		for _, body := range []string{"", "--fixture\r\nopaque audio\r\n"} {
			t.Run(string(op)+body, func(t *testing.T) {
				var wires atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					wires.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					if r.Header.Get(native.NativeVoiceAuthIDHeader) != "" {
						t.Error("internal pin leaked after custom attributes")
					}
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = conn.Close()
					}
				}))
				defer srv.Close()
				_, err := NewXAIExecutor(nil).Execute(context.Background(), voiceFixtureAuth(srv.URL), native.Request{Payload: []byte(body)}, voiceFixtureOptions(op))
				if err == nil || wires.Load() != 1 {
					t.Fatalf("err=%v actual wires=%d", err, wires.Load())
				}
				t.Logf("actual_voice_wire_count operation=%s body_bytes=%d committed_disconnect=%d", op, len(body), wires.Load())
			})
		}
	}
}
func TestVoiceHTTPRedirectAndSuccessStatus(t *testing.T) {
	for _, code := range []int{201, 204, 301, 302, 307, 308, 429} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var origin, target atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { target.Add(1) }))
			defer destination.Close()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				origin.Add(1)
				if r.Header.Get(native.NativeVoiceAuthIDHeader) != "" {
					t.Error("pin leaked")
				}
				if r.Header.Get("Content-Type") != "multipart/form-data; boundary=fixture" {
					t.Error("multipart boundary changed")
				}
				w.Header().Set("Location", destination.URL)
				w.Header().Set("Retry-After", "7")
				w.Header().Set("Content-Type", "audio/wav")
				w.Header().Set("Set-Cookie", "must-not-leak")
				w.WriteHeader(code)
				if code != 204 {
					_, _ = w.Write([]byte("raw audio/error"))
				}
			}))
			defer srv.Close()
			response, err := NewXAIExecutor(nil).Execute(context.Background(), voiceFixtureAuth(srv.URL), native.Request{Payload: []byte("--fixture")}, voiceFixtureOptions(native.NativeVoiceTTS))
			if origin.Load() != 1 || target.Load() != 0 {
				t.Fatalf("wire origin=%d redirect=%d", origin.Load(), target.Load())
			}
			if code < 300 {
				if err != nil || response.StatusCode != code {
					t.Fatalf("status=%d err=%v", response.StatusCode, err)
				}
				if response.Headers.Get("Set-Cookie") != "" {
					t.Fatal("cookie leaked")
				}
			} else {
				failure, ok := err.(*voiceHTTPStatusError)
				if !ok || failure.StatusCode() != code || string(failure.ResponseBody()) != "raw audio/error" || failure.VoiceHTTPResponseHeaders().Get("Retry-After") != "7" {
					t.Fatalf("raw failure not preserved: %v", err)
				}
			}
		})
	}
}
func TestVoiceHTTPIdempotencyAndKindsFailBeforeWire(t *testing.T) {
	var wires atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { wires.Add(1) }))
	defer srv.Close()
	for _, header := range []string{"Idempotency-Key", "x-idempotency-key"} {
		for _, value := range []string{"", "opaque"} {
			opts := voiceFixtureOptions(native.NativeVoiceDelete)
			opts.Headers[header] = []string{value}
			_, err := NewXAIExecutor(nil).Execute(context.Background(), voiceFixtureAuth(srv.URL), native.Request{}, opts)
			if err == nil {
				t.Fatal("idempotency presence accepted")
			}
		}
	}
	base := voiceFixtureAuth(srv.URL)
	for _, variant := range []string{"oauth", "compat", "disabled", "status-disabled", "home", "unknown-executor"} {
		candidate := base.Clone()
		var executor auth.ProviderExecutor = NewXAIExecutor(nil)
		home := false
		switch variant {
		case "oauth":
			candidate.Attributes["auth_kind"] = "oauth"
		case "compat":
			candidate.Provider = "openai-compatibility"
			candidate.Label = "grok"
		case "disabled":
			candidate.Disabled = true
		case "status-disabled":
			candidate.Status = auth.StatusDisabled
		case "home":
			home = true
		case "unknown-executor":
			executor = nil
		}
		if got := VoiceHTTPCompatibleOperations(candidate, executor, true, home); len(got) != 0 {
			t.Fatalf("%s advertised", variant)
		}
	}
	if wires.Load() != 0 {
		t.Fatalf("invalid requests made %d wires", wires.Load())
	}
}
func TestVoiceHTTPHTTP1TLSAndUnknownTransport(t *testing.T) {
	var protocol atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocol.Store(int32(r.ProtoMajor))
		_, _ = w.Write([]byte("audio"))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", srv.Client().Transport)
	response, err := NewXAIExecutor(nil).Execute(ctx, voiceFixtureAuth(srv.URL), native.Request{}, voiceFixtureOptions(native.NativeVoiceTTS))
	if err != nil || response.StatusCode != 200 || protocol.Load() != 1 {
		t.Fatalf("HTTP/1 contract err=%v proto=%d", err, protocol.Load())
	}
	unknown := context.WithValue(context.Background(), "cliproxy.roundtripper", voiceUnknownTransport{})
	_, err = NewXAIExecutor(nil).Execute(unknown, voiceFixtureAuth(srv.URL), native.Request{}, voiceFixtureOptions(native.NativeVoiceList))
	if err == nil {
		t.Fatal("unknown transport accepted")
	}
}

type voiceUnknownTransport struct{}

func (voiceUnknownTransport) RoundTrip(*http.Request) (*http.Response, error) {
	panic("unknown transport must never execute")
}

func TestVoiceHTTPBoundsAndUnknownOperationBeforeWire(t *testing.T) {
	var wires atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wires.Add(1)
		chunk := make([]byte, 32<<10)
		remaining := voiceHTTPBodyLimit + 2
		for remaining > 0 {
			size := len(chunk)
			if remaining < size {
				size = remaining
			}
			if _, err := w.Write(chunk[:size]); err != nil {
				return
			}
			remaining -= size
		}
	}))
	defer srv.Close()
	executor := NewXAIExecutor(nil)
	credential := voiceFixtureAuth(srv.URL)
	opts := voiceFixtureOptions(native.NativeVoiceHTTPOperation("unknown"))
	if _, err := executor.Execute(context.Background(), credential, native.Request{}, opts); err == nil {
		t.Fatal("unknown operation accepted")
	}
	opts = voiceFixtureOptions(native.NativeVoiceTTS)
	opts.SingleAttempt = false
	if _, err := executor.Execute(context.Background(), credential, native.Request{}, opts); err == nil {
		t.Fatal("unsafe unmarked operation accepted")
	}
	opts = voiceFixtureOptions(native.NativeVoiceTTS)
	if _, err := executor.Execute(context.Background(), credential, native.Request{Payload: make([]byte, voiceHTTPBodyLimit+1)}, opts); err == nil {
		t.Fatal("oversized request accepted")
	}
	if wires.Load() != 0 {
		t.Fatal("invalid input reached wire")
	}
	_, err := executor.Execute(context.Background(), credential, native.Request{}, opts)
	if err == nil || wires.Load() != 1 {
		t.Fatalf("response bound not enforced err=%v wires=%d", err, wires.Load())
	}
}

// Redirect hosts are mapped to one local listener; no external DNS is used.
func TestVoiceGETRedirectNeverReachesSourceB(t *testing.T) {
	for _, operation := range []native.NativeVoiceHTTPOperation{native.NativeVoiceList, native.NativeVoiceGet, native.NativeVoiceAudio} {
		for _, code := range []int{301, 302, 303, 307, 308} {
			for _, destination := range []string{"http://source-a.test/redirect", "http://source-a.test:9876/redirect", "http://sibling.test/redirect", "http://child.source-a.test/redirect", "http://unrelated.test/redirect"} {
				t.Run(fmt.Sprintf("%s/%d/%s", operation, code, destination), func(t *testing.T) {
					var origin, target atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/redirect" {
							target.Add(1)
							t.Error("redirect target received credential-bearing request")
							return
						}
						origin.Add(1)
						if r.Header.Get(native.NativeVoiceAuthIDHeader) != "" || r.Header.Get("Authorization") != "Bearer fixture-secret" || r.Header.Get("X-Custom-Credential") != "fixture-A" {
							t.Error("selected source identity or stripped pin incorrect")
						}
						w.Header().Set("Location", destination)
						w.Header().Set("Retry-After", "7")
						w.WriteHeader(code)
						_, _ = w.Write([]byte("terminal redirect"))
					}))
					defer server.Close()
					transport := http.DefaultTransport.(*http.Transport).Clone()
					transport.Proxy = nil
					transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
						return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
					}
					defer transport.CloseIdleConnections()
					ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
					credential := voiceFixtureAuth("http://source-a.test")
					credential.Attributes["header:X-Custom-Credential"] = "fixture-A"
					credential.Attributes["header:x-CLIPROXY-auth-ID"] = "server-A"
					response, err := NewXAIExecutor(nil).Execute(ctx, credential, native.Request{}, voiceFixtureOptions(operation))
					failure, ok := err.(*voiceHTTPStatusError)
					if !ok || failure.StatusCode() != code || string(failure.ResponseBody()) != "terminal redirect" || failure.VoiceHTTPResponseHeaders().Get("Location") != destination || failure.VoiceHTTPResponseHeaders().Get("Retry-After") != "7" {
						t.Fatalf("terminal redirect not preserved: %v", err)
					}
					if origin.Load() != 1 || target.Load() != 0 || response.StatusCode != 0 {
						t.Fatalf("origin=%d target=%d success=%d", origin.Load(), target.Load(), response.StatusCode)
					}
				})
			}
		}
	}
}

func TestVoiceGETNativeManagerRetriesSelectedSource(t *testing.T) {
	var wires, sourceB atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-secret" {
			sourceB.Add(1)
		}
		if r.Header.Get(native.NativeVoiceAuthIDHeader) != "" {
			t.Error("pin leaked")
		}
		if wires.Add(1) == 1 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":{"message":"temporary"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"voices":[]}`))
	}))
	defer server.Close()
	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(NewXAIExecutor(nil))
	manager.SetRetryConfig(1, 0, 2)
	for _, id := range []string{"voice-get-A", "voice-get-B"} {
		credential := voiceFixtureAuth(server.URL)
		credential.ID = id
		credential.Metadata = map[string]any{"request_retry": 1, "disable_cooling": true}
		if id == "voice-get-B" {
			credential.Attributes["api_key"] = "fixture-B"
		}
		if _, err := manager.Register(context.Background(), credential); err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(id, "xai", []*registry.ModelInfo{{ID: "voice-get-retry"}})
		defer registry.GetGlobalRegistry().UnregisterClient(id)
	}
	opts := voiceFixtureOptions(native.NativeVoiceList)
	opts.Metadata = map[string]any{native.PinnedAuthMetadataKey: "voice-get-A"}
	response, err := manager.Execute(context.Background(), []string{"xai"}, native.Request{Model: "voice-get-retry"}, opts)
	if err != nil || response.StatusCode != 200 || wires.Load() != 2 || sourceB.Load() != 0 {
		t.Fatalf("GET retry: err=%v status=%d wires=%d B=%d", err, response.StatusCode, wires.Load(), sourceB.Load())
	}
}

type voiceGETCloseCountingConn struct {
	net.Conn
	closed *atomic.Int32
}

func (c *voiceGETCloseCountingConn) Close() error {
	c.closed.Add(1)
	return c.Conn.Close()
}

func TestVoiceGETResponseBodyClosedOnBoundFailure(t *testing.T) {
	var closed, wires atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wires.Add(1)
		chunk := make([]byte, 32<<10)
		// Leave unread bytes after the client limit; only closing the body releases
		// this connection. A fully consumed response would not prove Close ran.
		for remaining := voiceHTTPBodyLimit + 1<<20; remaining > 0; remaining -= len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxIdleConns = 17
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &voiceGETCloseCountingConn{Conn: conn, closed: &closed}, nil
	}
	defer transport.CloseIdleConnections()
	defaultClient, defaultTransport := http.DefaultClient, http.DefaultTransport
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
	response, err := NewXAIExecutor(nil).Execute(ctx, voiceFixtureAuth(server.URL), native.Request{}, voiceFixtureOptions(native.NativeVoiceList))
	if err == nil || response.StatusCode != 0 || wires.Load() != 1 || closed.Load() != 1 {
		t.Fatalf("GET body cleanup: err=%v success=%d wires=%d closed=%d", err, response.StatusCode, wires.Load(), closed.Load())
	}
	if transport.DisableKeepAlives || transport.MaxIdleConns != 17 || http.DefaultClient != defaultClient || http.DefaultTransport != defaultTransport {
		t.Fatal("GET mutated shared or default transport/client")
	}
}

func TestPlanSpeechCumulativeUsageProtocolAndStrictTypes(t *testing.T) {
	audio := []byte("ID3\x04\x00\x00\x00\x00\x00\x00test-audio")
	enc := base64.StdEncoding.EncodeToString(audio)
	for _, tc := range []struct {
		name, body string
		words      int64
		valid      bool
	}{
		{"ordinary", `{"code":0,"data":"` + enc + `","usage":{"text_words":7}}` + "\n" + `{"code":20000000,"usage":{"text_words":7}}`, 7, true},
		{"terminal-audio", `{"code":0,"usage":{"text_words":3}}` + "\n" + `{"code":20000000,"data":"` + enc + `","usage":{"text_words":7}}`, 7, true},
		{"zero", `{"code":20000000,"data":"` + enc + `","usage":{"text_words":0}}`, 0, true},
		{"missing-terminal", `{"code":0,"data":"` + enc + `","usage":{"text_words":7}}`, 0, false},
		{"missing-usage", `{"code":20000000,"data":"` + enc + `"}`, 0, false},
		{"quoted-usage", `{"code":20000000,"data":"` + enc + `","usage":{"text_words":"7"}}`, 0, false},
		{"null-usage", `{"code":20000000,"data":"` + enc + `","usage":{"text_words":null}}`, 0, false},
		{"negative", `{"code":20000000,"data":"` + enc + `","usage":{"text_words":-1}}`, 0, false},
		{"fraction", `{"code":20000000,"data":"` + enc + `","usage":{"text_words":1.2}}`, 0, false},
		{"regress", `{"code":0,"data":"` + enc + `","usage":{"text_words":7}}` + "\n" + `{"code":20000000,"usage":{"text_words":6}}`, 0, false},
		{"bad-base64", `{"code":20000000,"data":"!bad","usage":{"text_words":7}}`, 0, false},
		{"truncated", `{"code":0,"data":"` + enc + `","usage":{"text_words":7}}` + "\n" + `{"code":20000000`, 0, false},
		{"null-code", `{"code":null,"data":"` + enc + `","usage":{"text_words":7}}`, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, words, err := helps.ReadVolcengineTTS(strings.NewReader(tc.body))
			if tc.valid {
				if err != nil || !bytes.Equal(audio, got) || words != tc.words {
					t.Fatalf("words=%d err=%v", words, err)
				}
			} else if err == nil {
				t.Fatal("invalid protocol accepted")
			}
		})
	}
	for _, extra := range []string{`,"speed":"1"`, `,"speed":null`, `,"speed":2`, `,"voice":"alloy"`, `,"response_format":"wav"`, `,"instructions":"x"`, `,"input":"duplicate"`, `,"model":"duplicate"`} {
		_, err := helps.ParseVolcengineTTSRequest([]byte(`{"model":"doubao-seed-tts-2.0","input":"hello"` + extra + `}`))
		if err == nil {
			t.Fatalf("accepted unsupported input %s", extra)
		}
	}
}

func TestPlanSpeechActualCompatHTTPWireAndNoRedirect(t *testing.T) {
	for _, status := range []int{200, 401, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var wires atomic.Int32
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wires.Add(1)
				if r.Host != "openspeech.bytedance.com" || r.URL.Path != "/api/v3/plan/tts/unidirectional" || r.Method != "POST" || r.Header.Get("X-Api-Key") != "fixture-plan-key" || r.Header.Get("X-Api-Resource-Id") != "seed-tts-2.0" || r.Header.Get("Authorization") != "" || r.Header.Get("X-CLIProxy-Auth-ID") != "" {
					t.Error("wrong supplier boundary")
				}
				w.Header().Set("Location", "https://openspeech.bytedance.com/never-follow")
				w.WriteHeader(status)
				if status == 200 {
					_, _ = io.WriteString(w, `{"code":20000000,"data":"SUQzBAAAAAAAAA==","usage":{"text_words":2}}`)
				}
			}))
			defer srv.Close()
			transport := srv.Client().Transport.(*http.Transport).Clone()
			transport.TLSClientConfig.ServerName = srv.Certificate().DNSNames[0]
			transport.Proxy = nil
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
			}
			cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "plan", BaseURL: "https://ark.cn-beijing.volces.com/api/plan/v3", Models: []config.OpenAICompatibilityModel{{Name: "doubao-seed-tts-2.0", SpeechProfile: "ark-agent-plan-tts-2-v1", UpstreamProtocol: "volcengine-tts-v3-http"}}}}}
			credential := &auth.Auth{ID: "plan-A", Provider: "plan", Status: auth.StatusActive, Attributes: map[string]string{"api_key": "fixture-plan-key", "base_url": cfg.OpenAICompatibility[0].BaseURL, "compat_name": "plan", "source": "config:plan[0]"}}
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
			response, err := NewOpenAICompatExecutor("plan", cfg).Execute(ctx, credential, native.Request{Model: "doubao-seed-tts-2.0", Payload: []byte(`{"model":"doubao-seed-tts-2.0","input":"你好"}`)}, native.Options{SourceFormat: sdktranslator.FromString("openai-speech"), NativeVoiceOperation: native.NativeVoiceTTS, SingleAttempt: true})
			if wires.Load() != 1 {
				t.Fatalf("supplier attempts=%d", wires.Load())
			}
			if status == 200 {
				if err != nil || response.Headers.Get("X-CLIProxy-Speech-Text-Words") != "2" || len(response.Payload) == 0 {
					t.Fatalf("response=%#v err=%v", response, err)
				}
			} else if err == nil {
				t.Fatal("non-success accepted")
			}
		})
	}
}

func TestVolcengineArkTextCompactAndChatWireParity(t *testing.T) {
	for _, alt := range []string{"", "responses/compact"} {
		t.Run(alt, func(t *testing.T) {
			var paths []string
			var bodies [][]byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				paths = append(paths, r.URL.Path)
				bodies = append(bodies, b)
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/responses/compact") {
					_, _ = io.WriteString(w, `{"id":"resp-fixture","object":"response.compaction","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
					return
				}
				_, _ = io.WriteString(w, `{"id":"chat-fixture","object":"chat.completion","created":1,"model":"ark-text","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			}))
			defer srv.Close()
			for _, protocol := range []registry.UpstreamProtocol{"", registry.UpstreamProtocolVolcengineArkTextV3} {
				cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "plan-text", BaseURL: srv.URL, Models: []config.OpenAICompatibilityModel{{Name: "ark-text", UpstreamProtocol: protocol}}}}}
				credential := &auth.Auth{ID: "text", Provider: "plan-text", Status: auth.StatusActive, Attributes: map[string]string{"api_key": "synthetic", "base_url": srv.URL, "compat_name": "plan-text", "source": "config:plan-text[0]"}}
				_, err := NewOpenAICompatExecutor("plan-text", cfg).Execute(context.Background(), credential, native.Request{Model: "ark-text", Payload: []byte(`{"model":"ark-text","input":"hello","include":["reasoning.encrypted_content"]}`)}, native.Options{Alt: alt, SourceFormat: sdktranslator.FromString("openai-response")})
				if err != nil {
					t.Fatal(err)
				}
			}
			if len(paths) != 2 || paths[0] != paths[1] || !bytes.Equal(bodies[0], bodies[1]) {
				t.Fatalf("wire drift paths=%v", paths)
			}
			var body map[string]any
			if json.Unmarshal(bodies[1], &body) != nil {
				t.Fatal("invalid actual wire JSON")
			}
			want := "/chat/completions"
			if alt != "" {
				want = "/responses/compact"
			}
			if paths[0] != want {
				t.Fatalf("operation=%s", paths[0])
			}
		})
	}
}

func TestVolcengineProtocolIdentityIsExplicitAndSuffixAware(t *testing.T) {
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "protocols", Models: []config.OpenAICompatibilityModel{
		{Name: "text", UpstreamProtocol: registry.UpstreamProtocolVolcengineArkTextV3},
		{Name: "doubao-seed-tts-2.0", UpstreamProtocol: registry.UpstreamProtocolVolcengineTTSV3HTTP, SpeechProfile: "ark-agent-plan-tts-2-v1"},
		{Name: "unknown", UpstreamProtocol: "unknown-protocol"}, {Name: "legacy"},
	}}}}
	credential := &auth.Auth{ID: "protocols", Provider: "protocols", Status: auth.StatusActive, Attributes: map[string]string{"api_key": "synthetic", "compat_name": "protocols", "source": "config:protocols[0]"}}
	e := NewOpenAICompatExecutor("protocols", cfg)
	for _, tc := range []struct {
		model string
		want  registry.UpstreamProtocol
	}{{"text(high)", registry.UpstreamProtocolVolcengineArkTextV3}, {"doubao-seed-tts-2.0(high)", registry.UpstreamProtocolVolcengineTTSV3HTTP}, {"legacy", ""}} {
		got, err := e.configuredUpstreamProtocol(credential, tc.model)
		if err != nil || got != tc.want {
			t.Fatalf("model=%s protocol=%s err=%v", tc.model, got, err)
		}
	}
	if _, err := e.configuredUpstreamProtocol(credential, "unknown"); err == nil {
		t.Fatal("unknown protocol fell back")
	}
	if _, err := e.Execute(context.Background(), credential, native.Request{Model: "doubao-seed-tts-2.0(high)"}, native.Options{}); err == nil {
		t.Fatal("suffixed audio reached text executor")
	}
	cfg.OpenAICompatibility[0].Models[1].UpstreamProtocol = ""
	if _, err := e.configuredUpstreamProtocol(credential, "doubao-seed-tts-2.0"); err == nil {
		t.Fatal("speech_profile was allowed to select wire protocol")
	}
}
