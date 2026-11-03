package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"io"
	"net/http"
	"strings"
)

// This error is created only after an actual HTTP attempt by the selected
// profiled native executor. It carries no credential, URL or vendor body.
type seedreamAttemptError struct{}

func (*seedreamAttemptError) Error() string {
	return `{"error":{"type":"upstream_image_error","code":"image_generation_outcome_unknown","message":"Image result is not verified; do not resubmit","retryable":false}}`
}
func (*seedreamAttemptError) StatusCode() int                 { return http.StatusBadGateway }
func (*seedreamAttemptError) NonReplayableImageAttempt() bool { return true }

func seedreamModelSyntax(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(model, "doubao-seedream-") || strings.HasPrefix(model, "seedream-")
}

func (e *OpenAICompatExecutor) seedreamProfile(auth *cliproxyauth.Auth, model string) (string, error) {
	var profile string
	if cfg := e.resolveCompatConfig(auth); cfg != nil {
		for _, row := range cfg.Models {
			if row.Name != model && row.Alias != model {
				continue
			}
			if row.ImageProfile == "" {
				continue
			}
			if profile != "" && profile != row.ImageProfile {
				return "", statusErr{code: 400, msg: "ambiguous image profile"}
			}
			if !row.Image || !seedreamModelSyntax(row.Name) || registry.SeedreamImageProfile(row.ImageProfile) == nil {
				return "", statusErr{code: 400, msg: "unsupported image profile binding"}
			}
			profile = row.ImageProfile
		}
	}
	if seedreamModelSyntax(model) && profile == "" {
		return "", statusErr{code: 400, msg: "explicit Seedream image profile required"}
	}
	return profile, nil
}

func normalizeSeedreamGeneration(body []byte, endpoint, profile, selectedModel string) ([]byte, error) {
	if registry.SeedreamImageProfile(profile) == nil || endpoint != "/images/generations" {
		return nil, statusErr{code: 400, msg: "image profile supports synchronous generations only"}
	}
	fields := make(map[string]json.RawMessage)
	decoder := json.NewDecoder(bytes.NewReader(body))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, statusErr{code: 400, msg: "image request must be one JSON object"}
	}
	for decoder.More() {
		token, readErr := decoder.Token()
		key, ok := token.(string)
		if readErr != nil || !ok {
			return nil, statusErr{code: 400, msg: "invalid image JSON field"}
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, statusErr{code: 400, msg: "duplicate image JSON field"}
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, statusErr{code: 400, msg: "invalid image JSON value"}
		}
		fields[key] = value
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, statusErr{code: 400, msg: "invalid image JSON object"}
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, statusErr{code: 400, msg: "one JSON document required"}
	}
	allowed := map[string]bool{"model": true, "prompt": true, "size": true, "response_format": true, "n": true, "stream": true, "watermark": true, "output_format": true}
	for key := range fields {
		if !allowed[key] {
			return nil, statusErr{code: 400, msg: "unsupported Seedream P1 parameter: " + key}
		}
	}
	var prompt, size string
	if json.Unmarshal(fields["prompt"], &prompt) != nil || strings.TrimSpace(prompt) == "" {
		return nil, statusErr{code: 400, msg: "prompt required"}
	}
	if json.Unmarshal(fields["size"], &size) != nil || (size != "1K" && size != "2K") {
		return nil, statusErr{code: 400, msg: "priced size 1K or 2K required; 1.5K unavailable"}
	}
	if raw, ok := fields["n"]; ok {
		var n json.Number
		if json.Unmarshal(raw, &n) != nil {
			return nil, statusErr{code: 400, msg: "n must be integer 1"}
		}
		v, err := n.Int64()
		if err != nil || v != 1 {
			return nil, statusErr{code: 400, msg: "n must be integer 1"}
		}
	}
	if raw, ok := fields["stream"]; ok {
		var stream *bool
		if json.Unmarshal(raw, &stream) != nil || stream == nil || *stream {
			return nil, statusErr{code: 400, msg: "stream unsupported by Seedream P1"}
		}
	}
	if raw, ok := fields["response_format"]; ok {
		var value string
		if json.Unmarshal(raw, &value) != nil || (value != "url" && value != "b64_json") {
			return nil, statusErr{code: 400, msg: "unsupported response_format"}
		}
	}
	if raw, ok := fields["watermark"]; ok {
		var value *bool
		if json.Unmarshal(raw, &value) != nil || value == nil {
			return nil, statusErr{code: 400, msg: "watermark must be boolean"}
		}
	}
	if raw, ok := fields["output_format"]; ok {
		var value string
		if json.Unmarshal(raw, &value) != nil || (value != "png" && value != "jpeg") {
			return nil, statusErr{code: 400, msg: "unsupported output_format"}
		}
	}
	// OpenAI n=1 is normalized to this profile's single-image operation. No
	// sequential_image_generation field is invented for pro/flash.
	// Map decoding can accept duplicate keys; always overwrite model from the
	// selected native Auth/model route, never from a later customer JSON key.
	fields["model"], _ = json.Marshal(selectedModel)
	delete(fields, "n")
	delete(fields, "stream")
	return json.Marshal(fields)
}

func verifySeedreamSingleResult(body []byte, requested []byte) error {
	var value struct {
		Data []struct {
			URL  string `json:"url"`
			B64  string `json:"b64_json"`
			Size string `json:"size"`
		} `json:"data"`
	}
	var req struct {
		Size string `json:"size"`
	}
	if json.Unmarshal(body, &value) != nil || json.Unmarshal(requested, &req) != nil || len(value.Data) != 1 {
		return fmt.Errorf("unverified Seedream image result")
	}
	row := value.Data[0]
	if (row.URL == "") == (row.B64 == "") {
		return fmt.Errorf("one image result required")
	}
	tier := map[string]string{"1K": "1K", "1024x1024": "1K", "2K": "2K", "2048x2048": "2K"}[row.Size]
	if tier == "" || tier != req.Size {
		return fmt.Errorf("unverified image size; no guessed tier")
	}
	return nil
}
