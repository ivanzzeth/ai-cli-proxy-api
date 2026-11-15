package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	native "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	"github.com/tidwall/gjson"
)

const (
	seedancePlanOrigin  = "https://ark.cn-beijing.volces.com/api/plan/v3"
	seedancePlanTasks   = seedancePlanOrigin + "/contents/generations/tasks"
	seedanceMaxResponse = 1 << 20
)

var seedanceTaskID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

type seedanceAttemptError struct {
	create bool
}

func (e *seedanceAttemptError) Error() string {
	if e.create {
		return `{"error":{"code":"seedance_create_outcome_unknown","message":"Seedance create outcome is unknown","retryable":false}}`
	}
	return `{"error":{"code":"seedance_status_unavailable","message":"Seedance status is unavailable"}}`
}

func (*seedanceAttemptError) StatusCode() int { return http.StatusBadGateway }

func (*seedanceAttemptError) NonReplayableSeedanceAttempt() bool { return true }

func (e *OpenAICompatExecutor) seedanceCredential(auth *coreauth.Auth, model string) (string, error) {
	invalid := statusErr{code: http.StatusBadRequest, msg: "Seedance requires the original configured Ark Plan key and model"}
	if e.cfg == nil || auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled || auth.AuthKind() != coreauth.AuthKindAPIKey || auth.AuthSourceKind() != coreauth.AuthSourceConfig {
		return "", invalid
	}
	index, err := strconv.Atoi(auth.Attributes[coreauth.AttributeConfigIndex])
	if err != nil || index < 0 || index >= len(e.cfg.OpenAICompatibility) {
		return "", invalid
	}
	compat := &e.cfg.OpenAICompatibility[index]
	base, key := e.resolveCredentials(auth)
	if compat.Disabled || key == "" || base != strings.TrimSpace(compat.BaseURL) || strings.TrimSuffix(base, "/") != seedancePlanOrigin || auth.Provider != util.OpenAICompatibleProviderKey(compat.Name) {
		return "", invalid
	}
	for name := range compat.Headers {
		if strings.EqualFold(strings.TrimSpace(name), "Authorization") {
			return "", invalid
		}
	}
	for name := range auth.Attributes {
		if strings.EqualFold(name, "header:Authorization") {
			return "", invalid
		}
	}

	matchingRows := 0
	for _, row := range compat.Models {
		if row.Name == model || row.Alias == model {
			if row.Name != model || row.UpstreamProtocol != registry.UpstreamProtocolVolcengineArkSeedanceV3 {
				return "", invalid
			}
			matchingRows++
		}
	}
	if matchingRows != 1 {
		return "", invalid
	}

	// Verify the watched credential against the live config using its existing
	// identity generator, including the duplicate-tuple suffix when present.
	name := seedanceConfigProviderName(compat.Name)
	generator := synthesizer.NewStableIDGenerator()
	for i := 0; i <= index; i++ {
		entry := &e.cfg.OpenAICompatibility[i]
		if entry.Disabled || seedanceConfigProviderName(entry.Name) != name {
			continue
		}
		for _, candidate := range entry.APIKeyEntries {
			candidateKey := strings.TrimSpace(candidate.APIKey)
			candidateProxy := strings.TrimSpace(candidate.ProxyURL)
			id, _ := generator.Next("openai-compatibility:"+name, candidateKey, entry.BaseURL, candidateProxy)
			if i == index && id == auth.ID && candidateKey == key && candidateProxy == strings.TrimSpace(auth.ProxyURL) {
				return key, nil
			}
		}
	}
	return "", invalid
}

func seedanceConfigProviderName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "openai-compatibility"
	}
	return name
}

func validSeedanceCreate(body []byte, model string) bool {
	if len(body) > 32<<10 || !gjson.ValidBytes(body) {
		return false
	}
	fields := seedanceJSONFields(gjson.ParseBytes(body))
	for field := range fields {
		switch field {
		case "model", "content", "duration", "resolution", "ratio", "execution_expires_after", "generate_audio":
		default:
			return false
		}
	}
	content := fields["content"]
	if !content.IsArray() || len(content.Array()) != 1 {
		return false
	}
	input := seedanceJSONFields(content.Array()[0])
	text := input["text"]
	return fields["model"].Type == gjson.String && fields["model"].String() == model &&
		fields["duration"].Raw == "5" && fields["resolution"].Type == gjson.String && fields["resolution"].String() == "480p" &&
		fields["ratio"].Type == gjson.String && fields["ratio"].String() == "16:9" && fields["execution_expires_after"].Raw == "3600" &&
		fields["generate_audio"].Type == gjson.False && len(input) == 2 &&
		input["type"].Type == gjson.String && input["type"].String() == "text" &&
		text.Type == gjson.String && strings.TrimSpace(text.String()) != "" && len(text.String()) <= 4096
}

func seedanceJSONFields(object gjson.Result) map[string]gjson.Result {
	if !object.IsObject() {
		return nil
	}
	fields := make(map[string]gjson.Result)
	object.ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		if _, duplicate := fields[name]; duplicate {
			fields = nil
			return false
		}
		fields[name] = value
		return true
	})
	return fields
}

func (e *OpenAICompatExecutor) executeVolcengineSeedance(ctx context.Context, auth *coreauth.Auth, req native.Request, opts native.Options) (native.Response, error) {
	pin, _ := opts.Metadata[native.PinnedAuthMetadataKey].(string)
	requested, _ := opts.Metadata[native.RequestedModelMetadataKey].(string)
	if auth == nil || pin == "" || auth.ID != pin || requested != req.Model || !opts.SingleAttempt || opts.Stream || opts.NativeVoiceOperation != "" || opts.ProxyURL != "" || native.RequestProxyURL(ctx) != "" || len(opts.Query) != 0 {
		return native.Response{}, statusErr{code: http.StatusBadRequest, msg: "Seedance requires a pinned native single attempt"}
	}
	key, err := e.seedanceCredential(auth, req.Model)
	if err != nil {
		return native.Response{}, err
	}

	method, target := http.MethodPost, seedancePlanTasks
	var body io.Reader
	if opts.Alt == "" {
		if !validSeedanceCreate(req.Payload, req.Model) {
			return native.Response{}, statusErr{code: http.StatusBadRequest, msg: "invalid controlled Seedance create"}
		}
		body = bytes.NewReader(req.Payload)
	} else {
		if !seedanceTaskID.MatchString(opts.Alt) || len(req.Payload) != 0 {
			return native.Response{}, statusErr{code: http.StatusBadRequest, msg: "invalid Seedance status request"}
		}
		method, target = http.MethodGet, target+"/"+opts.Alt
	}

	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return native.Response{}, err
	}
	request.Header.Set("Authorization", "Bearer "+key)
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	request.GetBody = nil

	var client *http.Client
	proxyURL := strings.TrimSpace(auth.ProxyURL)
	if proxyURL == "" {
		proxyURL = strings.TrimSpace(e.cfg.ProxyURL)
	}
	if proxyURL != "" {
		transport, _, proxyErr := proxyutil.BuildHTTPTransport(proxyURL)
		if proxyErr != nil || transport == nil {
			return native.Response{}, statusErr{code: http.StatusBadRequest, msg: "configured Seedance proxy is invalid"}
		}
		client = &http.Client{Transport: transport}
	} else {
		client = helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	}
	client, closeIdle, err := voiceSingleAttemptClient(client)
	if err != nil {
		return native.Response{}, err
	}
	defer closeIdle()

	// The owned HTTP/1 transport has no reused connection. RoundTrip returns
	// the original status even when a redirect has a malformed Location.
	native.MarkUpstreamAttempt(ctx)
	upstream, err := client.Transport.RoundTrip(request)
	if err != nil {
		return native.Response{}, &seedanceAttemptError{create: method == http.MethodPost}
	}
	defer func() { _ = upstream.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(upstream.Body, seedanceMaxResponse+1))
	if err != nil || len(payload) > seedanceMaxResponse {
		return native.Response{}, &seedanceAttemptError{create: method == http.MethodPost}
	}
	headers := make(http.Header)
	for _, name := range []string{"Content-Type", "X-Request-Id"} {
		if value := upstream.Header.Get(name); value != "" {
			headers.Set(name, value)
		}
	}
	return native.Response{Payload: payload, Headers: headers, StatusCode: upstream.StatusCode}, nil
}
