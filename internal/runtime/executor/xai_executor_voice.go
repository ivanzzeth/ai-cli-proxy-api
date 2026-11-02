package executor

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	native "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const voiceHTTPBodyLimit = 64 << 20

// VoiceHTTPOperation is the safe native operation projection, not entitlement proof.
type VoiceHTTPOperation struct {
	Method    string                          `json:"method"`
	Path      string                          `json:"path"`
	Operation native.NativeVoiceHTTPOperation `json:"-"`
}

var voiceHTTPOperations = []VoiceHTTPOperation{
	{http.MethodPost, "/v1/tts", native.NativeVoiceTTS},
	{http.MethodPost, "/v1/stt", native.NativeVoiceSTT},
	{http.MethodPost, "/v1/custom-voices", native.NativeVoiceCreate},
	{http.MethodGet, "/v1/custom-voices", native.NativeVoiceList},
	{http.MethodGet, "/v1/custom-voices/:voice_id", native.NativeVoiceGet},
	{http.MethodGet, "/v1/custom-voices/:voice_id/audio", native.NativeVoiceAudio},
	{http.MethodPatch, "/v1/custom-voices/:voice_id", native.NativeVoicePatch},
	{http.MethodDelete, "/v1/custom-voices/:voice_id", native.NativeVoiceDelete},
}

// VoiceHTTPCompatibleOperations uses the actual native executor and credential kind.
func VoiceHTTPCompatibleOperations(auth *cliproxyauth.Auth, executor cliproxyauth.ProviderExecutor, safeTransport, home bool) []VoiceHTTPOperation {
	supported := false
	switch actual := executor.(type) {
	case *XAIExecutor:
		supported = actual != nil
	case *XAIAutoExecutor:
		supported = actual != nil && actual.httpExec != nil
	}
	if !supported || !safeTransport || home || auth == nil || auth.Disabled || auth.Status == cliproxyauth.StatusDisabled || auth.ID == "" || !strings.EqualFold(strings.TrimSpace(auth.Provider), "xai") || auth.AuthKind() != cliproxyauth.AuthKindAPIKey || strings.TrimSpace(auth.Attributes["api_key"]) == "" || strings.TrimSpace(auth.Attributes["compat_name"]) != "" {
		return []VoiceHTTPOperation{}
	}
	return append([]VoiceHTTPOperation(nil), voiceHTTPOperations...)
}

func voiceSingleAttemptClient(client *http.Client) (*http.Client, func(), error) {
	if client == nil {
		return nil, nil, fmt.Errorf("voice HTTP client is unavailable")
	}
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	base, ok := transport.(*http.Transport)
	if !ok || base == nil {
		return nil, nil, fmt.Errorf("voice requires a native HTTP transport")
	}
	owned := base.Clone()
	owned.Protocols = new(http.Protocols)
	owned.Protocols.SetHTTP1(true)
	owned.Protocols.SetHTTP2(false)
	owned.Protocols.SetUnencryptedHTTP2(false)
	owned.ForceAttemptHTTP2 = false
	owned.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if owned.TLSClientConfig == nil {
		owned.TLSClientConfig = &tls.Config{}
	} else {
		owned.TLSClientConfig = owned.TLSClientConfig.Clone()
	}
	owned.TLSClientConfig.NextProtos = []string{"http/1.1"}
	clone := *client
	clone.Transport = owned
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &clone, owned.CloseIdleConnections, nil
}

type voiceHTTPStatusError struct {
	status         int
	body           []byte
	headers        http.Header
	classification statusErr
}

func (e *voiceHTTPStatusError) Error() string                         { return e.classification.Error() }
func (e *voiceHTTPStatusError) Unwrap() error                         { return e.classification }
func (e *voiceHTTPStatusError) StatusCode() int                       { return e.status }
func (e *voiceHTTPStatusError) DirectResponse() bool                  { return true }
func (e *voiceHTTPStatusError) ResponseBody() []byte                  { return append([]byte(nil), e.body...) }
func (e *voiceHTTPStatusError) VoiceHTTPResponseHeaders() http.Header { return e.headers.Clone() }
func (e *voiceHTTPStatusError) RetryAfter() *time.Duration {
	raw := strings.TrimSpace(e.headers.Get("Retry-After"))
	if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
		value := time.Duration(seconds) * time.Second
		return &value
	}
	if at, err := http.ParseTime(raw); err == nil {
		value := time.Until(at)
		if value < 0 {
			value = 0
		}
		return &value
	}
	return e.classification.RetryAfter()
}
func voiceResponseHeaders(header http.Header) http.Header {
	out := make(http.Header)
	for _, name := range []string{"Content-Type", "Retry-After", "Location", "X-Request-Id"} {
		if value := header.Get(name); value != "" {
			out.Set(name, value)
		}
	}
	return out
}

func (e *XAIExecutor) executeVoiceHTTP(ctx context.Context, auth *cliproxyauth.Auth, req native.Request, opts native.Options) (response native.Response, err error) {
	if len(VoiceHTTPCompatibleOperations(auth, e, true, false)) == 0 {
		return response, statusErr{code: 400, msg: "credential does not support native Voice HTTP"}
	}
	var operation *VoiceHTTPOperation
	for i := range voiceHTTPOperations {
		if voiceHTTPOperations[i].Operation == opts.NativeVoiceOperation {
			operation = &voiceHTTPOperations[i]
			break
		}
	}
	if operation == nil {
		return response, statusErr{code: 400, msg: "unknown native Voice operation"}
	}
	if operation.Operation.Unsafe() && !opts.SingleAttempt {
		return response, statusErr{code: 400, msg: "unsafe Voice requires single-attempt execution"}
	}
	if len(req.Payload) > voiceHTTPBodyLimit {
		return response, statusErr{code: 413, msg: "Voice request body exceeds limit"}
	}
	path := strings.TrimPrefix(operation.Path, "/v1")
	query := make(url.Values)
	for key, values := range opts.Query {
		query[key] = append([]string(nil), values...)
	}
	id := query.Get("_native_voice_id")
	query.Del("_native_voice_id")
	if strings.Contains(path, ":voice_id") {
		if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\?#") {
			return response, statusErr{code: 400, msg: "invalid voice_id"}
		}
		path = strings.ReplaceAll(path, ":voice_id", url.PathEscape(id))
	}
	target := strings.TrimRight(xaiCompactBaseURL(auth), "/") + path
	if encoded := query.Encode(); encoded != "" {
		target += "?" + encoded
	}
	request, err := http.NewRequestWithContext(ctx, operation.Method, target, bytes.NewReader(req.Payload))
	if err != nil {
		return response, err
	}
	if operation.Operation.Unsafe() {
		for key := range opts.Headers {
			if strings.EqualFold(key, "Idempotency-Key") || strings.EqualFold(key, "X-Idempotency-Key") {
				return response, statusErr{code: 400, msg: "unsafe Voice idempotency header rejected"}
			}
		}
	}
	applyXAIHeaders(request, auth, strings.TrimSpace(auth.Attributes["api_key"]), false, "", opts.Headers)
	if contentType := opts.Headers.Get("Content-Type"); contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	// Remove the internal server pin only after native custom-header attributes.
	for key := range request.Header {
		if strings.EqualFold(key, native.NativeVoiceAuthIDHeader) {
			delete(request.Header, key)
		} else if operation.Operation.Unsafe() && (strings.EqualFold(key, "Idempotency-Key") || strings.EqualFold(key, "X-Idempotency-Key")) {
			return response, statusErr{code: 400, msg: "unsafe Voice idempotency header rejected"}
		}
	}
	client := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	effectiveTransport := client.Transport
	if effectiveTransport == nil {
		effectiveTransport = http.DefaultTransport
	}
	if transport, ok := effectiveTransport.(*http.Transport); !ok || transport == nil {
		return response, fmt.Errorf("Voice requires a native HTTP transport")
	}
	if operation.Operation.Unsafe() {
		var closeIdle func()
		client, closeIdle, err = voiceSingleAttemptClient(client)
		if err != nil {
			return response, err
		}
		defer closeIdle()
		request.GetBody = nil
	} else {
		voiceClient := *client
		voiceClient.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
		client = &voiceClient
	}
	reporter := helps.NewExecutorUsageReporter(ctx, e, string(operation.Operation), auth)
	defer reporter.TrackFailure(ctx, &err)
	client = reporter.TrackHTTPClient(client)
	e.recordXAIRequest(ctx, auth, target, request.Header.Clone(), req.Payload)
	upstream, err := client.Do(request)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return response, err
	}
	defer func() { _ = upstream.Body.Close() }()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, upstream.StatusCode, upstream.Header.Clone())
	body, err := io.ReadAll(io.LimitReader(upstream.Body, voiceHTTPBodyLimit+1))
	if err != nil {
		return response, err
	}
	if len(body) > voiceHTTPBodyLimit {
		return response, fmt.Errorf("Voice response body exceeds limit")
	}
	header := voiceResponseHeaders(upstream.Header)
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		return response, &voiceHTTPStatusError{status: upstream.StatusCode, body: body, headers: header, classification: xaiStatusErr(upstream.StatusCode, body)}
	}
	reporter.EnsurePublished(ctx)
	return native.Response{StatusCode: upstream.StatusCode, Payload: body, Headers: header}, nil
}
