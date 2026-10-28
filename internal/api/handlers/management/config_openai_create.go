package management

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// PostOpenAICompat atomically appends a named OpenAI-compatible provider.
func (h *Handler) PostOpenAICompat(c *gin.Context) {
	var entry config.OpenAICompatibility
	if err := c.ShouldBindJSON(&entry); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	entry.Name = strings.TrimSpace(entry.Name)
	normalizeOpenAICompatibilityEntry(&entry)
	if entry.Name == "" || entry.BaseURL == "" || len(entry.Models) == 0 || len(entry.APIKeyEntries) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing provider fields"})
		return
	}
	for _, key := range entry.APIKeyEntries {
		if key.APIKey == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "empty api key"})
			return
		}
		if rejectInvalidCredentialWeight(c, "api-key-entries.weight", key.Weight) {
			return
		}
	}
	for _, model := range entry.Models {
		if strings.TrimSpace(model.Name) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "empty model"})
			return
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, existing := range h.cfg.OpenAICompatibility {
		if existing.Name == entry.Name {
			c.JSON(http.StatusConflict, gin.H{"error": "provider name already exists"})
			return
		}
		if entry.Prefix != "" && existing.Prefix == entry.Prefix {
			c.JSON(http.StatusConflict, gin.H{"error": "provider model prefix already exists"})
			return
		}
	}
	previous := append([]config.OpenAICompatibility(nil), h.cfg.OpenAICompatibility...)
	h.cfg.OpenAICompatibility = append(h.cfg.OpenAICompatibility, entry)
	h.cfg.SanitizeOpenAICompatibility()
	if !h.persistLocked(c) {
		h.cfg.OpenAICompatibility = previous
	}
}
