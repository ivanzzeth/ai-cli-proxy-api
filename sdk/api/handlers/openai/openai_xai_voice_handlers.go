package openai

import (
	"context"
	"io"
	"strings"

	"github.com/gin-gonic/gin"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	native "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// XAIVoiceHTTP is reached only after the server's standard /v1 authentication.
func (h *OpenAIAPIHandler) XAIVoiceHTTP(c *gin.Context) {
	if h == nil || h.BaseAPIHandler == nil || h.AuthManager == nil || h.AuthManager.HomeEnabled() {
		c.JSON(503, gin.H{"error": "native Voice execution is unavailable"})
		return
	}
	pin, present, failure := h.PrepareServerCredentialPin(c)
	if failure != nil {
		h.WriteErrorResponse(c, failure)
		return
	}
	if !present {
		c.JSON(400, gin.H{"error": "exactly one server credential pin is required"})
		return
	}
	auth, ok := h.AuthManager.GetByID(pin)
	if !ok || auth == nil {
		c.JSON(400, gin.H{"error": "server credential pin is stale"})
		return
	}
	executor, _ := h.AuthManager.Executor(auth.Provider)
	var operation native.NativeVoiceHTTPOperation
	for _, item := range runtimeexecutor.VoiceHTTPCompatibleOperations(auth, executor, h.AuthManager.SupportsVoiceHTTPTransport(pin), h.AuthManager.HomeEnabled()) {
		if item.Method == c.Request.Method && item.Path == c.FullPath() {
			operation = item.Operation
			break
		}
	}
	if operation == "" {
		c.JSON(400, gin.H{"error": "credential does not support this Voice operation"})
		return
	}
	if operation.Unsafe() {
		for key := range c.Request.Header {
			if strings.EqualFold(key, "Idempotency-Key") || strings.EqualFold(key, "X-Idempotency-Key") {
				c.JSON(400, gin.H{"error": "unsafe Voice idempotency header rejected"})
				return
			}
		}
	}
	var body []byte
	var err error
	if c.Request.Body != nil {
		body, err = io.ReadAll(io.LimitReader(c.Request.Body, (64<<20)+1))
	}
	if err != nil || len(body) > 64<<20 {
		c.JSON(413, gin.H{"error": "Voice request body exceeds limit or could not be read"})
		return
	}
	query := c.Request.URL.Query()
	query.Set("_native_voice_id", c.Param("voice_id"))
	// Buffer under the existing Base lifecycle, detached from downstream disconnect.
	original := c.Request
	parent := context.WithoutCancel(c.Request.Context())
	c.Request = c.Request.WithContext(parent)
	defer func() { c.Request = original }()
	ctx, cancel := h.GetContextWithCancel(h, c, parent)
	defer cancel()
	ctx = handlers.WithPinnedAuthID(ctx, pin)
	response, failure := h.ExecuteProtocolWithAuthManager(ctx, handlers.ProtocolExecutionRequest{EntryProtocol: "xai-voice-http", ExitProtocol: "xai-voice-http", ForcedProvider: "xai", NativeVoiceOperation: operation, SingleAttempt: operation.Unsafe(), Body: body, Headers: c.Request.Header.Clone(), Query: query})
	if failure != nil {
		h.WriteErrorResponse(c, failure)
		return
	}
	for name, values := range response.Headers {
		c.Writer.Header()[name] = append([]string(nil), values...)
	}
	c.Data(response.StatusCode, response.Headers.Get("Content-Type"), response.Body)
}
