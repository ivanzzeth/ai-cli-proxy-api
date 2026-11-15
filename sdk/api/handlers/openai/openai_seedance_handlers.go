package openai

import (
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/tidwall/gjson"
)

const (
	seedanceTaskPath    = "/api/v3/contents/generations/tasks"
	seedanceModelHeader = "X-CLIProxy-Seedance-Model"
	seedanceMaxRequest  = 32 << 10
)

var (
	seedanceModelID  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
	seedanceStatusID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
)

// SeedanceTask accepts the private Sub request for a pinned native Ark operation.
func (h *OpenAIAPIHandler) SeedanceTask(c *gin.Context) {
	if h == nil || h.AuthManager == nil || h.AuthManager.HomeEnabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "native Seedance execution unavailable"})
		return
	}
	if c.GetString("userApiKey") == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Seedance requires authenticated access"})
		return
	}
	_, present, failure := h.PrepareServerCredentialPin(c)
	if failure != nil {
		h.WriteErrorResponse(c, failure)
		return
	}
	if !present {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Seedance requires a server credential pin"})
		return
	}

	var modelValues []string
	hasModelHeader := false
	for name, values := range c.Request.Header {
		switch strings.ToLower(name) {
		case strings.ToLower(seedanceModelHeader):
			hasModelHeader = true
			if len(values) == 0 {
				values = []string{""}
			}
			modelValues = append(modelValues, values...)
		case "idempotency-key", "x-idempotency-key":
			c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported Seedance control header"})
			return
		}
	}
	if c.Request.URL.RawQuery != "" || c.Request.URL.ForceQuery {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Seedance query parameters are unsupported"})
		return
	}

	var body []byte
	var model, taskID string
	switch c.Request.Method {
	case http.MethodPost:
		if c.Request.URL.EscapedPath() != seedanceTaskPath || hasModelHeader {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid Seedance create route"})
			return
		}
		var err error
		body, err = io.ReadAll(io.LimitReader(c.Request.Body, seedanceMaxRequest+1))
		if err != nil || len(body) > seedanceMaxRequest || !gjson.ValidBytes(body) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid bounded Seedance JSON"})
			return
		}
		value := gjson.GetBytes(body, "model")
		if value.Type == gjson.String {
			model = value.String()
		}
	case http.MethodGet:
		taskID = c.Param("task_id")
		if !seedanceStatusID.MatchString(taskID) || c.Request.URL.EscapedPath() != seedanceTaskPath+"/"+taskID || c.Request.ContentLength != 0 || len(c.Request.TransferEncoding) != 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid Seedance status route"})
			return
		}
		if len(modelValues) == 1 {
			model = modelValues[0]
		}
	default:
		c.Status(http.StatusMethodNotAllowed)
		return
	}
	if !seedanceModelID.MatchString(model) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "exactly one Seedance model is required"})
		return
	}

	ctx, cancel := h.GetContextWithCancel(h, c, c.Request.Context())
	defer cancel()
	protocol := string(registry.UpstreamProtocolVolcengineArkSeedanceV3)
	response, failure := h.ExecuteProtocolWithAuthManager(ctx, handlers.ProtocolExecutionRequest{
		EntryProtocol: protocol, ExitProtocol: protocol, Model: model, Body: body, Alt: taskID, SingleAttempt: true,
	})
	if failure != nil {
		h.WriteErrorResponse(c, failure)
		return
	}
	for name, values := range response.Headers {
		c.Writer.Header()[name] = append([]string(nil), values...)
	}
	c.Data(response.StatusCode, response.Headers.Get("Content-Type"), response.Body)
}
