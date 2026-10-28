package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// PostGeminiKey adds one credential without replacing operators' keys.
func (h *Handler) PostGeminiKey(c *gin.Context) {
	var entry config.GeminiKey
	if err := c.ShouldBindJSON(&entry); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	entry.APIKey = strings.TrimSpace(entry.APIKey)
	entry.BaseURL = strings.TrimRight(strings.TrimSpace(entry.BaseURL), "/")
	if entry.APIKey == "" || entry.BaseURL == "" || len(entry.Models) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing credential fields"})
		return
	}
	if rejectInvalidCredentialWeight(c, "gemini-api-key.weight", entry.Weight) {
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
	for _, existing := range h.cfg.GeminiKey {
		if c.Query("create-only") == "true" && entry.Description != "" && existing.Description == entry.Description {
			c.JSON(http.StatusConflict, gin.H{"error": "managed provider already exists"})
			return
		}
		if existing.APIKey == entry.APIKey && existing.BaseURL == entry.BaseURL {
			c.JSON(http.StatusConflict, gin.H{"error": "credential already exists"})
			return
		}
		if existing.Description != entry.Description {
			for _, configured := range existing.Models {
				advertised := configured.Alias
				if advertised == "" {
					advertised = configured.Name
				}
				for _, requested := range entry.Models {
					alias := requested.Alias
					if alias == "" {
						alias = requested.Name
					}
					if strings.EqualFold(advertised, alias) {
						c.JSON(http.StatusConflict, gin.H{"error": "model route already belongs to another credential"})
						return
					}
				}
			}
		}
	}
	previous := append([]config.GeminiKey(nil), h.cfg.GeminiKey...)
	h.cfg.GeminiKey = append(h.cfg.GeminiKey, entry)
	h.cfg.SanitizeGeminiKeys()
	if !h.persistLocked(c) {
		h.cfg.GeminiKey = previous
	}
}
