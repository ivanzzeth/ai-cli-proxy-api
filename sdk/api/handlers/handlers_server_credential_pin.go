package handlers

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/net/context"
)

type serverCredentialPinScopeKey struct{}
type serverCredentialPinErrorKey struct{}
type serverBridgeSingleAttemptKey struct{}

func serverBridgeSingleAttempt(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if enabled, _ := ctx.Value(serverBridgeSingleAttemptKey{}).(bool); enabled {
		return true
	}
	if c, ok := ctx.Value("gin").(*gin.Context); ok && c != nil && c.Request != nil {
		enabled, _ := c.Request.Context().Value(serverBridgeSingleAttemptKey{}).(bool)
		return enabled
	}
	return false
}

func serverCredentialPinError(status int, message string) *interfaces.ErrorMessage {
	return &interfaces.ErrorMessage{StatusCode: status, Error: fmt.Errorf("%s", message)}
}

func protectedServerCredentialPin(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(serverCredentialPinScopeKey{}).(string)
	if id != "" {
		return id
	}
	if c, ok := ctx.Value("gin").(*gin.Context); ok && c != nil && c.Request != nil {
		id, _ = c.Request.Context().Value(serverCredentialPinScopeKey{}).(string)
	}
	return id
}

func (h *BaseAPIHandler) validateCurrentServerCredentialPin(id string) *interfaces.ErrorMessage {
	if h == nil || h.AuthManager == nil {
		return serverCredentialPinError(http.StatusServiceUnavailable, "credential authority unavailable")
	}
	a, ok := h.AuthManager.GetByID(id)
	if !ok || a == nil || a.Disabled || a.Status == coreauth.StatusDisabled {
		return serverCredentialPinError(http.StatusBadRequest, "server credential pin is unavailable")
	}
	return nil
}

// PrepareServerCredentialPin is the single parser used by authenticated middleware
// and by directly mounted domain handlers. It does not choose models/providers.
func (h *BaseAPIHandler) PrepareServerCredentialPin(c *gin.Context) (string, bool, *interfaces.ErrorMessage) {
	if c == nil || c.Request == nil {
		return "", false, nil
	}
	prior := protectedServerCredentialPin(c.Request.Context())
	present := false
	var values []string
	for name, items := range c.Request.Header {
		if !strings.EqualFold(name, "X-CLIProxy-Auth-ID") {
			continue
		}
		present = true
		if len(items) == 0 {
			values = append(values, "")
		} else {
			values = append(values, items...)
		}
	}
	id := prior
	if present {
		if len(values) != 1 || strings.TrimSpace(values[0]) == "" || strings.Contains(values[0], ",") {
			return "", true, serverCredentialPinError(http.StatusBadRequest, "exactly one nonempty server credential pin is required")
		}
		id = strings.TrimSpace(values[0])
		if prior != "" && prior != id {
			return "", true, serverCredentialPinError(http.StatusBadRequest, "server credential pin scope conflicts")
		}
	}
	policyPresent := false
	var policyValues []string
	for name, values := range c.Request.Header {
		if strings.EqualFold(name, "X-CLIProxy-Single-Attempt") {
			policyPresent = true
			policyValues = append(policyValues, values...)
		}
	}
	if policyPresent && (id == "" || len(policyValues) != 1 || policyValues[0] != "1") {
		return "", true, serverCredentialPinError(http.StatusBadRequest, "single-attempt policy requires one protected server credential pin")
	}
	if id == "" {
		return "", false, nil
	}
	if existing := pinnedAuthIDFromContext(c.Request.Context()); existing != "" && existing != id {
		return "", true, serverCredentialPinError(http.StatusBadRequest, "server credential pin conflicts with request affinity")
	}
	if failure := h.validateCurrentServerCredentialPin(id); failure != nil {
		return "", true, failure
	}
	request := c.Request.Clone(context.WithValue(c.Request.Context(), serverCredentialPinScopeKey{}, id))
	if policyPresent {
		request = request.Clone(context.WithValue(request.Context(), serverBridgeSingleAttemptKey{}, true))
	}
	for name := range request.Header {
		if strings.EqualFold(name, "X-CLIProxy-Auth-ID") || strings.EqualFold(name, "X-CLIProxy-Single-Attempt") {
			delete(request.Header, name)
		}
	}
	c.Request = request
	return id, true, nil
}

func (h *BaseAPIHandler) ServerCredentialPinMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, _, failure := h.PrepareServerCredentialPin(c); failure != nil {
			h.WriteErrorResponse(c, failure)
			c.Abort()
			return
		}
		c.Next()
	}
}

// RejectServerCredentialPin explicitly closes routes that do not implement the
// protected scope; unpinned requests retain their existing handler unchanged.
func (h *BaseAPIHandler) RejectServerCredentialPin() gin.HandlerFunc {
	return func(c *gin.Context) {
		_, present, failure := h.PrepareServerCredentialPin(c)
		if failure == nil && present {
			failure = serverCredentialPinError(http.StatusBadRequest, "server credential pin is unsupported for this endpoint")
		}
		if failure != nil {
			h.WriteErrorResponse(c, failure)
			c.Abort()
			return
		}
		c.Next()
	}
}

func bridgeServerCredentialPin(parent, request context.Context) context.Context {
	parentID := protectedServerCredentialPin(parent)
	requestID := protectedServerCredentialPin(request)
	if parentID != "" && requestID != "" && parentID != requestID {
		failure := serverCredentialPinError(http.StatusBadRequest, "protected parent and request credential scopes conflict")
		return context.WithValue(parent, serverCredentialPinErrorKey{}, failure)
	}
	if parentID == "" && requestID != "" {
		return context.WithValue(parent, serverCredentialPinScopeKey{}, requestID)
	}
	return parent
}

// ValidateServerCredentialPinContext runs before routing and preserves the
// existing Manager as the sole model, provider, cooling and retry authority.
func (h *BaseAPIHandler) ValidateServerCredentialPinContext(ctx context.Context) (context.Context, *interfaces.ErrorMessage) {
	if ctx == nil {
		return ctx, nil
	}
	if failure, _ := ctx.Value(serverCredentialPinErrorKey{}).(*interfaces.ErrorMessage); failure != nil {
		return ctx, failure
	}
	id := protectedServerCredentialPin(ctx)
	if id == "" {
		return ctx, nil
	}
	if existing := pinnedAuthIDFromContext(ctx); existing != "" && existing != id {
		return ctx, serverCredentialPinError(http.StatusBadRequest, "server credential pin conflicts with execution affinity")
	}
	if c, ok := ctx.Value("gin").(*gin.Context); ok && c != nil && c.Request != nil {
		if requestID := protectedServerCredentialPin(c.Request.Context()); requestID != "" && requestID != id {
			return ctx, serverCredentialPinError(http.StatusBadRequest, "protected execution and request credential scopes conflict")
		}
		if existing := pinnedAuthIDFromContext(c.Request.Context()); existing != "" && existing != id {
			return ctx, serverCredentialPinError(http.StatusBadRequest, "server credential pin conflicts with request affinity")
		}
	}
	if failure := h.validateCurrentServerCredentialPin(id); failure != nil {
		return ctx, failure
	}
	return WithPinnedAuthID(ctx, id), nil
}
