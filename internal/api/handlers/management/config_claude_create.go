package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// PostClaudeKey adds one credential without replacing other operators' credentials.
func (h *Handler) PostClaudeKey(c *gin.Context) {
	var entry config.ClaudeKey
	if err := c.ShouldBindJSON(&entry); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	entry.APIKey = strings.TrimSpace(entry.APIKey)
	entry.BaseURL = strings.TrimRight(strings.TrimSpace(entry.BaseURL), "/")
	normalizeClaudeKey(&entry)
	if entry.APIKey == "" || entry.BaseURL == "" || len(entry.Models) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing credential fields"})
		return
	}
	if rejectInvalidCredentialWeight(c, "claude-api-key.weight", entry.Weight) || rejectInvalidFingerprintProfile(c, "claude-api-key.fingerprint-profile", entry.FingerprintProfile) {
		return
	}
	for _, model := range entry.Models {
		if strings.TrimSpace(model.Name) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "empty model"})
			return
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, existing := range h.cfg.ClaudeKey {
		if c.Query("create-only") == "true" && entry.Description != "" && existing.Description == entry.Description {
			c.JSON(http.StatusConflict, gin.H{"error": "managed provider already exists"})
			return
		}
		if existing.APIKey == entry.APIKey && existing.BaseURL == entry.BaseURL {
			c.JSON(http.StatusConflict, gin.H{"error": "credential already exists"})
			return
		}
		if conflictingManagedClaudePrefix(entry, existing) {
			c.JSON(http.StatusConflict, gin.H{"error": "managed model prefix belongs to another provider"})
			return
		}
	}
	previous := append([]config.ClaudeKey(nil), h.cfg.ClaudeKey...)
	h.cfg.ClaudeKey = append(h.cfg.ClaudeKey, entry)
	h.cfg.SanitizeClaudeKeys()
	if !h.persistLocked(c) {
		h.cfg.ClaudeKey = previous
	}
}

func conflictingManagedClaudePrefix(entry, existing config.ClaudeKey) bool {
	entryPrefix := strings.TrimSpace(entry.Prefix)
	return entryPrefix != "" && entryPrefix == strings.TrimSpace(existing.Prefix) && entry.Description != existing.Description &&
		(strings.HasPrefix(entry.Description, "gateway-provider/v1:") || strings.HasPrefix(existing.Description, "gateway-provider/v1:"))
}
