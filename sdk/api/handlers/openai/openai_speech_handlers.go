package openai

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	native "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// AudioSpeech uses the existing native execution boundary; it never selects xAI.
func (h *OpenAIAPIHandler) AudioSpeech(c *gin.Context) {
	if h == nil || h.AuthManager == nil || h.AuthManager.HomeEnabled() {
		c.JSON(503, gin.H{"error": "native speech execution unavailable"})
		return
	}
	for name := range c.Request.Header {
		if strings.EqualFold(name, "X-CLIProxy-Speech-Text-Words") || strings.EqualFold(name, "X-Audio-Text-Words") || strings.EqualFold(name, "Idempotency-Key") || strings.EqualFold(name, "X-Idempotency-Key") {
			c.JSON(400, gin.H{"error": "unsupported speech control header"})
			return
		}
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 32769))
	if err != nil || len(body) > 32768 || !gjson.ValidBytes(body) {
		c.JSON(400, gin.H{"error": "invalid bounded speech JSON"})
		return
	}
	model := gjson.GetBytes(body, "model")
	if model.Type != gjson.String || strings.TrimSpace(model.String()) == "" {
		c.JSON(400, gin.H{"error": "speech model required"})
		return
	}
	pin, present, failure := h.PrepareServerCredentialPin(c)
	if failure != nil {
		h.WriteErrorResponse(c, failure)
		return
	}
	original := c.Request
	parent := context.WithoutCancel(original.Context())
	c.Request = original.WithContext(parent)
	defer func() { c.Request = original }()
	ctx, cancel := h.GetContextWithCancel(h, c, parent)
	defer cancel()
	if present {
		ctx = handlers.WithPinnedAuthID(ctx, pin)
	}
	response, failure := h.ExecuteProtocolWithAuthManager(ctx, handlers.ProtocolExecutionRequest{
		EntryProtocol: "openai-speech", ExitProtocol: "openai-speech", Model: model.String(), Body: body,
		NativeVoiceOperation: native.NativeVoiceTTS, SingleAttempt: true,
		Headers: c.Request.Header.Clone(),
	})
	if failure != nil {
		h.WriteErrorResponse(c, failure)
		return
	}
	for name, values := range response.Headers {
		c.Writer.Header()[name] = append([]string(nil), values...)
	}
	c.Header("Content-Length", strconv.Itoa(len(response.Body)))
	c.Data(http.StatusOK, response.Headers.Get("Content-Type"), response.Body)
}
