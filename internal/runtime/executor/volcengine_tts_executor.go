package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	native "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const arkPlanSpeechModel = "doubao-seed-tts-2.0"

func (e *OpenAICompatExecutor) speechProfile(auth *cliproxyauth.Auth, model string) (string, error) {
	cfg := e.resolveCompatConfig(auth)
	if cfg == nil {
		return "", nil
	}
	profile := ""
	for _, row := range cfg.Models {
		if row.Name != model && row.Alias != model {
			continue
		}
		if row.SpeechProfile == "" {
			continue
		}
		if registry.ArkPlanSpeechProfile(row.SpeechProfile) == nil || row.Name != arkPlanSpeechModel || row.Image || (row.Alias != "" && row.Alias != row.Name) {
			return "", statusErr{code: 400, msg: "invalid explicit speech profile"}
		}
		if profile != "" && profile != row.SpeechProfile {
			return "", statusErr{code: 400, msg: "ambiguous speech profile"}
		}
		profile = row.SpeechProfile
	}
	return profile, nil
}

type speechOutcomeUnknown struct{}

func (*speechOutcomeUnknown) Error() string {
	return `{"error":{"type":"upstream_speech_error","code":"speech_outcome_unknown","message":"Speech result is not verified; do not resubmit","retryable":false}}`
}
func (*speechOutcomeUnknown) StatusCode() int { return http.StatusBadGateway }

// VolcengineTTSAdapter is selected only by the explicit upstream wire protocol.
// Credential selection and accounting remain at their existing owners.
type VolcengineTTSAdapter struct{ owner *OpenAICompatExecutor }

func (adapter VolcengineTTSAdapter) Execute(ctx context.Context, auth *cliproxyauth.Auth, req native.Request, opts native.Options) (response native.Response, err error) {
	e := adapter.owner
	if e == nil {
		return response, statusErr{code: 400, msg: "native TTS adapter unavailable"}
	}
	protocol, protocolErr := e.configuredUpstreamProtocol(auth, req.Model)
	if protocolErr != nil {
		return response, protocolErr
	}
	if protocol != registry.UpstreamProtocolVolcengineTTSV3HTTP {
		return response, statusErr{code: 400, msg: "explicit Volcengine TTS protocol required"}
	}
	if auth == nil || auth.AuthKind() != cliproxyauth.AuthKindAPIKey || auth.AuthSourceKind() != cliproxyauth.AuthSourceConfig || opts.NativeVoiceOperation != native.NativeVoiceTTS || !opts.SingleAttempt || opts.Stream {
		return response, statusErr{code: 400, msg: "native single-attempt speech required"}
	}
	profile, err := e.speechProfile(auth, req.Model)
	if err != nil {
		return response, err
	}
	if profile == "" || req.Model != arkPlanSpeechModel {
		return response, statusErr{code: 400, msg: "explicit Plan speech model required"}
	}
	base, key := e.resolveCredentials(auth)
	origin, parseErr := url.Parse(base)
	if parseErr != nil || origin.Scheme != "https" || !strings.EqualFold(origin.Host, "ark.cn-beijing.volces.com") || strings.TrimRight(origin.Path, "/") != "/api/plan/v3" || origin.RawQuery != "" || origin.Fragment != "" || origin.User != nil || key == "" {
		return response, statusErr{code: 400, msg: "speech profile requires the original official Ark Plan origin"}
	}
	input, err := helps.ParseVolcengineTTSRequest(req.Payload)
	if err != nil {
		return response, statusErr{code: 400, msg: err.Error()}
	}
	payload, _ := json.Marshal(map[string]any{"req_params": map[string]any{"text": input.Input, "speaker": input.Voice, "audio_params": map[string]any{"format": "mp3", "sample_rate": 24000}}})
	for name := range opts.Headers {
		if strings.EqualFold(name, "Idempotency-Key") || strings.EqualFold(name, "X-Idempotency-Key") || strings.EqualFold(name, "X-CLIProxy-Speech-Text-Words") || strings.EqualFold(name, "X-Audio-Text-Words") {
			return response, statusErr{code: 400, msg: "unsupported speech control header"}
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://openspeech.bytedance.com/api/v3/plan/tts/unidirectional", bytes.NewReader(payload))
	if err != nil {
		return response, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", key)
	request.Header.Set("X-Api-Resource-Id", "seed-tts-2.0")
	request.Header.Set("X-Api-Request-Id", uuid.NewString())
	request.Header.Set("X-Control-Require-Usage-Tokens-Return", "*")
	request.GetBody = nil
	client, closeIdle, err := voiceSingleAttemptClient(helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0))
	if err != nil {
		return response, err
	}
	defer closeIdle()
	reporter := helps.NewExecutorUsageReporter(ctx, e, req.Model, auth)
	defer reporter.TrackFailure(ctx, &err)
	client = reporter.TrackHTTPClient(client)
	attempted := true
	defer func() {
		if attempted && err != nil {
			err = &speechOutcomeUnknown{}
		}
	}()
	helps.LogWithRequestID(ctx).WithFields(log.Fields{"upstream_protocol": string(registry.UpstreamProtocolVolcengineTTSV3HTTP), "upstream_operation": "tts/unidirectional"}).Debug("native protocol selected")
	upstream, err := client.Do(request)
	if err != nil {
		return response, err
	}
	defer func() { _ = upstream.Body.Close() }()
	if upstream.StatusCode != http.StatusOK {
		return response, fmt.Errorf("speech upstream did not return success")
	}
	audio, words, err := helps.ReadVolcengineTTS(upstream.Body)
	if err != nil {
		return response, err
	}
	header := make(http.Header)
	header.Set("Content-Type", "audio/mpeg")
	header.Set("X-CLIProxy-Speech-Text-Words", strconv.FormatInt(words, 10))
	header.Set("X-Request-Id", request.Header.Get("X-Api-Request-Id"))
	reporter.EnsurePublished(ctx)
	return native.Response{StatusCode: 200, Payload: audio, Headers: header, Metadata: map[string]any{"upstream_protocol": string(registry.UpstreamProtocolVolcengineTTSV3HTTP), "speech_usage_source": "upstream_actual_words"}}, nil
}
