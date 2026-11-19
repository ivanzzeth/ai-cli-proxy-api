package executor

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/tidwall/gjson"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
	if opts.NativeVoiceOperation == native.NativeVoiceTTS && strings.TrimSpace(req.Model) == "" {
		declared := xaiSpeechDeclarationForModel(auth, "")
		if declared == nil || declared.UpstreamProtocol != registry.UpstreamProtocolXAIVoiceHTTP || declared.AudioSpeech == nil || declared.AudioSpeech.Profile != "xai-grok-tts-v1" {
			return response, statusErr{code: 400, msg: "native TTS requires a current selected-credential speech declaration"}
		}
	}
	compatibleSpeech := opts.SourceFormat.String() == "openai-speech"
	expectedMIME := ""
	if compatibleSpeech {
		if auth == nil {
			return response, statusErr{code: 400, msg: "speech credential required"}
		}
		if opts.NativeVoiceOperation != native.NativeVoiceTTS || opts.Stream || !opts.SingleAttempt {
			return response, statusErr{code: 400, msg: "unsupported speech operation"}
		}
		info := xaiDeclaredSpeechModel(auth, req.Model)
		if info == nil || info.UpstreamProtocol != registry.UpstreamProtocolXAIVoiceHTTP || info.AudioSpeech == nil || info.AudioSpeech.Profile != "xai-grok-tts-v1" {
			return response, statusErr{code: 400, msg: "explicit xAI speech profile required"}
		}
		payload, err := openAISpeechToXAI(req.Payload)
		if err != nil {
			return response, statusErr{code: 400, msg: err.Error()}
		}
		req.Payload = payload
		expectedMIME = map[string]string{"mp3": "audio/mpeg", "wav": "audio/wav", "pcm": "audio/pcm"}[gjson.GetBytes(payload, "output_format.codec").String()]
	}

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
	if compatibleSpeech {
		actual, _, parseErr := mime.ParseMediaType(header.Get("Content-Type"))
		if parseErr != nil || actual != expectedMIME || len(body) == 0 {
			return response, fmt.Errorf("speech_outcome_unknown: requested audio codec mismatch")
		}
		header.Set("Content-Type", actual)
	}
	reporter.EnsurePublished(ctx)
	usageSource := "native_operation_units"
	if operation.Operation == native.NativeVoiceTTS {
		usageSource = "request_estimated_characters"
	}
	return native.Response{StatusCode: upstream.StatusCode, Payload: body, Headers: header, Metadata: map[string]any{"upstream_protocol": string(registry.UpstreamProtocolXAIVoiceHTTP), "speech_usage_source": usageSource}}, nil
}

// xaiDeclaredSpeechModel consults only this selected credential's explicit registry
// declarations. Equal model names on a different credential are unrelated.
func xaiDeclaredSpeechModel(auth *cliproxyauth.Auth, model string) *registry.ModelInfo {
	if strings.TrimSpace(model) == "" {
		return nil
	}
	return xaiSpeechDeclarationForModel(auth, model)
}
func xaiSpeechDeclarationForModel(auth *cliproxyauth.Auth, model string) *registry.ModelInfo {
	if auth == nil {
		return nil
	}
	name := thinking.ParseSuffix(model).ModelName
	var found *registry.ModelInfo
	for _, info := range registry.GetGlobalRegistry().GetModelsForClient(auth.ID) {
		if info == nil || (name != "" && info.ID != name && info.MetadataModelID != name) || info.AudioSpeech == nil {
			continue
		}
		if found != nil && (found.UpstreamProtocol != info.UpstreamProtocol || found.AudioSpeech.Profile != info.AudioSpeech.Profile) {
			return &registry.ModelInfo{UpstreamProtocol: "invalid", AudioSpeech: &registry.AudioSpeechSupport{}}
		}
		found = info
	}
	return found
}

// openAISpeechToXAI translates only representable existing fields. Provider selection
// has already used the selected credential's explicit model declaration above.
func openAISpeechToXAI(body []byte) ([]byte, error) {
	if len(body) > 32768 || !utf8.Valid(body) || !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("invalid bounded speech JSON")
	}
	object := gjson.ParseBytes(body)
	if !object.IsObject() {
		return nil, fmt.Errorf("speech JSON object required")
	}
	fields := map[string]gjson.Result{}
	var invalid bool
	object.ForEach(func(k, v gjson.Result) bool {
		if _, exists := fields[k.String()]; exists {
			invalid = true
			return false
		}
		fields[k.String()] = v
		return true
	})
	if invalid {
		return nil, fmt.Errorf("duplicate speech field")
	}
	for name := range fields {
		switch name {
		case "model", "input", "voice", "response_format", "speed", "instructions", "stream_format":
		default:
			return nil, fmt.Errorf("unsupported speech parameter")
		}
	}
	for _, name := range []string{"model", "input", "voice"} {
		if fields[name].Type != gjson.String || strings.TrimSpace(fields[name].String()) == "" {
			return nil, fmt.Errorf("speech model,input,voice strings required")
		}
	}
	if utf8.RuneCountInString(fields["input"].String()) > 4096 || len(fields["voice"].String()) > 128 || strings.ContainsAny(fields["voice"].String(), "/\\?# \t\r\n") {
		return nil, fmt.Errorf("unsupported speech input or native voice ID")
	}
	codec := "mp3"
	if f, ok := fields["response_format"]; ok {
		if f.Type != gjson.String {
			return nil, fmt.Errorf("speech format string required")
		}
		codec = f.String()
	}
	switch codec {
	case "mp3", "wav", "pcm":
	default:
		return nil, fmt.Errorf("unsupported speech format")
	}
	speed := 1.0
	if v, ok := fields["speed"]; ok {
		if v.Type != gjson.Number || v.Float() < 0.7 || v.Float() > 1.5 {
			return nil, fmt.Errorf("speech speed outside supported intersection")
		}
		speed = v.Float()
	}
	if v, ok := fields["instructions"]; ok && (v.Type != gjson.String || v.String() != "") {
		return nil, fmt.Errorf("speech instructions unsupported")
	}
	if v, ok := fields["stream_format"]; ok && (v.Type != gjson.String || v.String() != "audio") {
		return nil, fmt.Errorf("speech event format unsupported")
	}
	output := map[string]any{"codec": codec, "sample_rate": 24000}
	if codec == "mp3" {
		output["bit_rate"] = 128000
	}
	return json.Marshal(map[string]any{"text": fields["input"].String(), "voice_id": fields["voice"].String(), "language": "auto", "speed": speed, "output_format": output})
}
