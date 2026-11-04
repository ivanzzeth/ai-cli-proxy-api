package helps

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

const ArkPlanSpeechVoice = "zh_female_vv_uranus_bigtts"
const arkSpeechAudioLimit = 16 << 20

type VolcengineTTSInput struct{ Input, Voice string }

// This is provider-specific bounded JSON, not a generic schema engine.
func speechObject(body []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(body))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return nil, fmt.Errorf("speech requires one JSON object")
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		keyToken, err := d.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok {
			return nil, fmt.Errorf("invalid speech JSON key")
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, fmt.Errorf("duplicate speech JSON key")
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, fmt.Errorf("invalid speech JSON value")
		}
		fields[key] = value
	}
	last, err := d.Token()
	if err != nil || last != json.Delim('}') {
		return nil, fmt.Errorf("invalid speech JSON object")
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return nil, fmt.Errorf("one speech JSON document required")
	}
	return fields, nil
}

func ParseVolcengineTTSRequest(body []byte) (VolcengineTTSInput, error) {
	var result VolcengineTTSInput
	if len(body) > 32768 || !utf8.Valid(body) {
		return result, fmt.Errorf("bounded UTF8 speech request required")
	}
	fields, err := speechObject(body)
	if err != nil {
		return result, err
	}
	for key := range fields {
		switch key {
		case "model", "input", "voice", "response_format", "speed":
		default:
			return result, fmt.Errorf("unsupported speech parameter")
		}
	}
	var model string
	if json.Unmarshal(fields["model"], &model) != nil || strings.TrimSpace(model) == "" || json.Unmarshal(fields["input"], &result.Input) != nil || strings.TrimSpace(result.Input) == "" || utf8.RuneCountInString(result.Input) > 1000 {
		return result, fmt.Errorf("speech model and input of 1..1000 Unicode codepoints required")
	}
	result.Voice = ArkPlanSpeechVoice
	if raw, present := fields["voice"]; present {
		var voice *string
		if json.Unmarshal(raw, &voice) != nil || voice == nil || *voice != ArkPlanSpeechVoice {
			return result, fmt.Errorf("unsupported speech voice")
		}
		result.Voice = *voice
	}
	if raw, present := fields["response_format"]; present {
		var format *string
		if json.Unmarshal(raw, &format) != nil || format == nil || *format != "mp3" {
			return result, fmt.Errorf("only mp3 speech supported")
		}
	}
	if raw, present := fields["speed"]; present {
		var value *float64
		if json.Unmarshal(raw, &value) != nil || value == nil || *value != 1 {
			return result, fmt.Errorf("speech speed must be 1")
		}
	}
	return result, nil
}

// ReadVolcengineTTS verifies protocol completion and format recognition, not codec decoding.
func ReadVolcengineTTS(reader io.Reader) ([]byte, int64, error) {
	limited := &io.LimitedReader{R: reader, N: (32 << 20) + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	var audio []byte
	var words int64
	haveWords := false
	terminal := false
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		fields, err := speechObject(line)
		if err != nil {
			return nil, 0, err
		}
		var codeValue *int64
		if raw, ok := fields["code"]; !ok || json.Unmarshal(raw, &codeValue) != nil || codeValue == nil {
			return nil, 0, fmt.Errorf("missing speech frame code")
		}
		if raw, ok := fields["usage"]; ok {
			usage, err := speechObject(raw)
			if err != nil {
				return nil, 0, fmt.Errorf("invalid speech usage")
			}
			if count, ok := usage["text_words"]; ok {
				var parsed *int64
				if json.Unmarshal(count, &parsed) != nil || parsed == nil {
					return nil, 0, fmt.Errorf("invalid speech character count")
				}
				next := *parsed
				if next < 0 || next > (1<<53) || (haveWords && next < words) {
					return nil, 0, fmt.Errorf("conflicting speech character count")
				}
				words = next
				haveWords = true
			}
		}
		code := *codeValue
		if code != 0 && code != 20000000 {
			return nil, 0, fmt.Errorf("speech provider returned a nonterminal error")
		}
		if raw, ok := fields["data"]; ok && string(raw) != "null" {
			var encoded string
			if json.Unmarshal(raw, &encoded) != nil {
				return nil, 0, fmt.Errorf("invalid speech audio")
			}
			chunk, err := base64.StdEncoding.Strict().DecodeString(encoded)
			if err != nil || len(audio)+len(chunk) > arkSpeechAudioLimit {
				return nil, 0, fmt.Errorf("invalid or oversized speech audio")
			}
			audio = append(audio, chunk...)
		}
		if limited.N <= 1 {
			return nil, 0, fmt.Errorf("speech response exceeds total limit")
		}
		if code == 20000000 {
			terminal = true
			break
		}
	}
	if scanner.Err() != nil || !terminal || !haveWords || len(audio) == 0 {
		return nil, 0, fmt.Errorf("speech outcome unknown: missing terminal/audio/usage")
	}
	mpegSignature := len(audio) >= 2 && audio[0] == 0xff && (audio[1]&0xe0) == 0xe0
	if http.DetectContentType(audio) != "audio/mpeg" && !mpegSignature {
		return nil, 0, fmt.Errorf("speech audio format is not recognized MP3")
	}
	return audio, words, nil
}
