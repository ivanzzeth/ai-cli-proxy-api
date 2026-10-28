package management

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type managedPoolPatch struct {
	Description     string               `json:"description"`
	ExpectedPrefix  string               `json:"expected-prefix"`
	BaseURL         string               `json:"base-url"`
	Keys            []string             `json:"expected-api-keys"`
	PreviousBaseURL string               `json:"expected-base-url"`
	PreviousModels  []config.GeminiModel `json:"expected-models"`
	Models          []config.GeminiModel `json:"models"`
}

func validManagedPoolPatch(c *gin.Context, patch *managedPoolPatch) bool {
	patch.Description = strings.TrimSpace(patch.Description)
	patch.BaseURL = strings.TrimRight(strings.TrimSpace(patch.BaseURL), "/")
	parsed, err := url.Parse(patch.BaseURL)
	if !strings.HasPrefix(patch.Description, "gateway-provider/v1:") || patch.Description == "gateway-provider/v1:" || err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || patch.PreviousBaseURL == "" || len(patch.Keys) < 2 || len(patch.Models) == 0 || len(patch.PreviousModels) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid managed credential pool"})
		return false
	}
	seenKeys := make(map[string]bool, len(patch.Keys))
	for _, key := range patch.Keys {
		if strings.TrimSpace(key) == "" || seenKeys[key] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid expected credential keys"})
			return false
		}
		seenKeys[key] = true
	}
	seenAliases := make(map[string]bool, len(patch.Models))
	for _, model := range patch.Models {
		if strings.TrimSpace(model.Name) == "" || strings.TrimSpace(model.Alias) == "" || seenAliases[model.Alias] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid managed model routes"})
			return false
		}
		seenAliases[model.Alias] = true
	}
	return true
}

func (h *Handler) PatchManagedClaudePool(c *gin.Context) {
	var patch managedPoolPatch
	if c.ShouldBindJSON(&patch) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if !validManagedPoolPatch(c, &patch) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if patch.ExpectedPrefix == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "expected model prefix is required"})
		return
	}
	indexes := make(map[string]int, len(patch.Keys))
	for index, entry := range h.cfg.ClaudeKey {
		if entry.Description != patch.Description {
			for _, key := range patch.Keys {
				if entry.APIKey == key || entry.Prefix == patch.ExpectedPrefix {
					c.JSON(http.StatusConflict, gin.H{"error": "managed credential identity or route is shared"})
					return
				}
			}
			continue
		}
		_, duplicate := indexes[entry.APIKey]
		if entry.Prefix != patch.ExpectedPrefix || entry.BaseURL != patch.PreviousBaseURL || !sameClaudePoolRoutes(entry.Models, patch.PreviousModels) || duplicate {
			c.JSON(http.StatusConflict, gin.H{"error": "managed credential pool changed"})
			return
		}
		indexes[entry.APIKey] = index
	}
	if !managedKeysMatch(indexes, patch.Keys) {
		c.JSON(http.StatusConflict, gin.H{"error": "managed credential pool changed"})
		return
	}
	previous := append([]config.ClaudeKey(nil), h.cfg.ClaudeKey...)
	for _, index := range indexes {
		h.cfg.ClaudeKey[index].BaseURL = patch.BaseURL
		h.cfg.ClaudeKey[index].Models = updatedClaudePoolModels(h.cfg.ClaudeKey[index].Models, patch.Models)
	}
	h.cfg.SanitizeClaudeKeys()
	if !h.persistLocked(c) {
		h.cfg.ClaudeKey = previous
	}
}

func (h *Handler) PatchManagedGeminiPool(c *gin.Context) {
	var patch managedPoolPatch
	if c.ShouldBindJSON(&patch) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if !validManagedPoolPatch(c, &patch) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	indexes := make(map[string]int, len(patch.Keys))
	for index, entry := range h.cfg.GeminiKey {
		if entry.Description != patch.Description {
			for _, key := range patch.Keys {
				if entry.APIKey == key {
					c.JSON(http.StatusConflict, gin.H{"error": "managed credential identity is shared"})
					return
				}
			}
			for _, configured := range entry.Models {
				for _, requested := range patch.Models {
					if configured.Alias == requested.Alias && requested.Alias != "" {
						c.JSON(http.StatusConflict, gin.H{"error": "managed model alias is shared"})
						return
					}
				}
			}
			continue
		}
		_, duplicate := indexes[entry.APIKey]
		if entry.BaseURL != patch.PreviousBaseURL || !sameGeminiPoolRoutes(entry.Models, patch.PreviousModels) || duplicate {
			c.JSON(http.StatusConflict, gin.H{"error": "managed credential pool changed"})
			return
		}
		indexes[entry.APIKey] = index
	}
	if !managedKeysMatch(indexes, patch.Keys) {
		c.JSON(http.StatusConflict, gin.H{"error": "managed credential pool changed"})
		return
	}
	previous := append([]config.GeminiKey(nil), h.cfg.GeminiKey...)
	for _, index := range indexes {
		h.cfg.GeminiKey[index].BaseURL = patch.BaseURL
		h.cfg.GeminiKey[index].Models = updatedGeminiPoolModels(h.cfg.GeminiKey[index].Models, patch.Models)
	}
	h.cfg.SanitizeGeminiKeys()
	if !h.persistLocked(c) {
		h.cfg.GeminiKey = previous
	}
}

func managedKeysMatch(indexes map[string]int, expected []string) bool {
	if len(indexes) != len(expected) {
		return false
	}
	for _, key := range expected {
		if _, ok := indexes[key]; !ok {
			return false
		}
	}
	return true
}

func claudeModels(models []config.GeminiModel) []config.ClaudeModel {
	converted := make([]config.ClaudeModel, 0, len(models))
	for _, model := range models {
		converted = append(converted, config.ClaudeModel{Name: model.Name, Alias: model.Alias})
	}
	return converted
}

func updatedClaudePoolModels(current []config.ClaudeModel, requested []config.GeminiModel) []config.ClaudeModel {
	updated := make([]config.ClaudeModel, 0, len(requested))
	for _, route := range requested {
		model := config.ClaudeModel{Name: route.Name, Alias: route.Alias}
		for _, existing := range current {
			if existing.Name == route.Name {
				model = existing
				model.Alias = route.Alias
				break
			}
		}
		updated = append(updated, model)
	}
	return updated
}

func updatedGeminiPoolModels(current, requested []config.GeminiModel) []config.GeminiModel {
	updated := make([]config.GeminiModel, 0, len(requested))
	for _, route := range requested {
		model := config.GeminiModel{Name: route.Name, Alias: route.Alias}
		for _, existing := range current {
			if existing.Name == route.Name {
				model = existing
				model.Alias = route.Alias
				break
			}
		}
		updated = append(updated, model)
	}
	return updated
}

func sameClaudePoolRoutes(current []config.ClaudeModel, expected []config.GeminiModel) bool {
	if len(current) != len(expected) {
		return false
	}
	for index, model := range current {
		if model.Name != expected[index].Name || model.Alias != expected[index].Alias {
			return false
		}
	}
	return true
}

func sameGeminiPoolRoutes(current, expected []config.GeminiModel) bool {
	if len(current) != len(expected) {
		return false
	}
	for index, model := range current {
		if model.Name != expected[index].Name || model.Alias != expected[index].Alias {
			return false
		}
	}
	return true
}
