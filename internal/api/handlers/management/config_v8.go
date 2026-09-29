package management

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"gopkg.in/yaml.v3"
)

// ConfigV8ContextKey marks operations registered by the v8 API that save configuration.
const ConfigV8ContextKey = "management.config-v8"

// ConfigV8 serves the persisted configuration using v8 names. GET is a view;
// only a successful configuration mutation migrates the stored document.
func (h *Handler) ConfigV8(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	data, err := os.ReadFile(h.configFilePath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read_failed"})
		return
	}
	data, _, err = config.NormalizeConfigLayout(data, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid_config", "message": err.Error()})
		return
	}
	var doc yaml.Node
	if err = yaml.Unmarshal(data, &doc); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid_config"})
		return
	}
	root := doc.Content[0]
	path := strings.Trim(c.Param("path"), "/")
	parts := []string{}
	if path != "" {
		parts = strings.Split(path, "/")
	}
	yamlRequest := strings.HasSuffix(c.FullPath(), "/config.yaml")
	if c.Request.Method == http.MethodGet {
		if !yamlRequest {
			if servers := configV8Node(root, v8ICEServersPath); servers != nil && servers.Kind == yaml.SequenceNode {
				for _, server := range servers.Content {
					deleteConfigV8Path(server, []string{"username"})
					deleteConfigV8Path(server, []string{"credential"})
				}
			}
		}
		value := configV8Node(root, parts)
		if value == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
			return
		}
		c.Header("Cache-Control", "no-store")
		if yamlRequest {
			c.Data(http.StatusOK, "application/yaml; charset=utf-8", data)
			return
		}
		var result any
		if err = value.Decode(&result); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "decode_failed"})
			return
		}
		c.JSON(http.StatusOK, result)
		return
	}
	before := cloneConfigV8Node(root)
	if c.Request.Method == http.MethodDelete {
		if len(parts) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cannot_delete_config"})
			return
		}
		if !deleteConfigV8Path(root, parts) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
			return
		}
	} else {
		body, errRead := io.ReadAll(c.Request.Body)
		if errRead != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body"})
			return
		}
		if !yamlRequest && !json.Valid(body) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
			return
		}
		var update yaml.Node
		if err = yaml.Unmarshal(body, &update); err != nil || len(update.Content) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body"})
			return
		}
		if len(parts) == 0 && update.Content[0].Kind != yaml.MappingNode {
			c.JSON(http.StatusBadRequest, gin.H{"error": "config_must_be_object"})
			return
		}
		// Paths identify YAML keys, never array indexes. Lists are replaced whole.
		dst := root
		for _, part := range parts {
			if part == "" || dst.Kind != yaml.MappingNode {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_path"})
				return
			}
			next := configV8Node(dst, []string{part})
			if next == nil {
				next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				dst.Content = append(dst.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: part}, next)
			}
			dst = next
		}
		if c.Request.Method == http.MethodPatch {
			mergeConfigV8Patch(dst, update.Content[0])
		} else {
			*dst = *update.Content[0]
		}
	}
	// Full-document saves from the visual editor used to write client keys into
	// the v8 upstream map at `api-keys`, or delete that map entirely. Keep the
	// provider groups and fold a client-key list into `access.api-keys`.
	if len(parts) == 0 && c.Request.Method != http.MethodDelete {
		preserveV8UpstreamAPIKeys(before, root)
	}
	if !yamlRequest && c.Request.Method != http.MethodDelete {
		preserveV8TURNSecrets(root, before)
	}
	// Revisions are owned by Home and must not become ordinary editable settings.
	for _, field := range []string{"credentials/concurrency/lifecycle-config-revision", "credentials/concurrency/observation-barrier-revision", "plugins/auth-revision"} {
		old := configV8Node(before, strings.Split(field, "/"))
		next := configV8Node(root, strings.Split(field, "/"))
		var a, b any
		if old != nil {
			_ = old.Decode(&a)
		}
		if next != nil {
			_ = next.Decode(&b)
		}
		if !reflect.DeepEqual(a, b) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "read_only_field", "field": field})
			return
		}
	}
	data, err = yaml.Marshal(&doc)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_config", "message": err.Error()})
		return
	}
	next, err := config.ParseConfigBytes(data)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_config", "message": err.Error()})
		return
	}
	if err = config.ValidateV8Config(data); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_config", "message": err.Error()})
		return
	}
	// Prepare the normalized document before touching the live configuration.
	tmp, err := os.CreateTemp(filepath.Dir(h.configFilePath), ".config-v8-*.yaml")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed"})
		return
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	var errWrite error
	if _, errWrite = tmp.Write(data); errWrite == nil {
		errWrite = tmp.Sync()
	}
	errClose := tmp.Close()
	if errWrite == nil {
		errWrite = errClose
	}
	if errWrite == nil {
		errWrite = config.SaveConfigPreserveComments(tmpPath, next, true)
	}
	if errWrite == nil {
		data, errWrite = os.ReadFile(tmpPath)
	}
	if errWrite == nil && c.Request.Method == http.MethodDelete {
		// The general saver can materialize defaults. Keep an explicit deletion
		// absent on disk so subsequent legacy edits can still supply that field.
		var saved yaml.Node
		errWrite = yaml.Unmarshal(data, &saved)
		if errWrite == nil && len(saved.Content) > 0 && deleteConfigV8Path(saved.Content[0], parts) {
			data, errWrite = yaml.Marshal(&saved)
		}
		if errWrite == nil {
			// The final deletion must also remove its OAuth scope from the snapshot.
			next, errWrite = config.ParseConfigBytes(data)
		}
	}
	if errWrite == nil {
		// Preserve the destination inode: the standard Docker deployment mounts
		// config.yaml as a single file, which cannot be replaced with rename.
		errWrite = WriteConfig(h.configFilePath, data)
	}
	if errWrite != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": errWrite.Error()})
		return
	}
	// Runtime-only ownership state is never sourced from an uploaded YAML document.
	next.Home = h.cfg.Home
	h.cfg = next
	snapshot := h.reloadSnapshotConfigLocked()
	h.reloadConfigAfterManagementSaveAsync(c.Request.Context(), snapshot)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "config-version": 8})
}

var v8ICEServersPath = []string{"oauth", "providers", "codex", "live-media-relay", "ice-servers"}

// JSON reads redact TURN secrets. Preserve omitted credentials when a client
// writes that JSON back, matching by endpoint rather than array position so a
// changed URL cannot inherit credentials for a different server. Explicit empty
// strings/null clear a secret; YAML writes retain full replacement semantics.
func preserveV8TURNSecrets(root, before *yaml.Node) {
	next := configV8Node(root, v8ICEServersPath)
	previous := configV8Node(before, v8ICEServersPath)
	if next == nil || previous == nil || next.Kind != yaml.SequenceNode || previous.Kind != yaml.SequenceNode {
		return
	}
	matched := make([]bool, len(previous.Content))
	for _, server := range next.Content {
		urls := configV8Node(server, []string{"urls"})
		if urls == nil {
			continue
		}
		var newURLs []string
		if urls.Decode(&newURLs) != nil {
			continue
		}
		for i, old := range previous.Content {
			if matched[i] {
				continue
			}
			oldURLs := configV8Node(old, []string{"urls"})
			var previousURLs []string
			if oldURLs == nil || oldURLs.Decode(&previousURLs) != nil || !reflect.DeepEqual(newURLs, previousURLs) {
				continue
			}
			matched[i] = true
			for _, name := range []string{"username", "credential"} {
				if configV8Node(server, []string{name}) == nil {
					if secret := configV8Node(old, []string{name}); secret != nil {
						server.Content = append(server.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}, cloneConfigV8Node(secret))
					}
				}
			}
			break
		}
	}
}

// preserveV8UpstreamAPIKeys keeps upstream provider groups when a full-document
// write omits them or replaces the `api-keys` mapping with a client-key list.
func preserveV8UpstreamAPIKeys(previous, next *yaml.Node) {
	prevGroups := configV8Node(previous, []string{"api-keys"})
	if prevGroups == nil || prevGroups.Kind != yaml.MappingNode || next == nil {
		return
	}
	nextGroups := configV8Node(next, []string{"api-keys"})
	if nextGroups != nil && nextGroups.Kind == yaml.MappingNode {
		return
	}
	var incoming []string
	if nextGroups != nil && nextGroups.Kind == yaml.SequenceNode {
		if err := nextGroups.Decode(&incoming); err != nil {
			incoming = nil
		}
	}
	if len(incoming) > 0 {
		var have []string
		if existing := configV8Node(next, []string{"access", "api-keys"}); existing != nil {
			_ = existing.Decode(&have)
		}
		if len(have) == 0 {
			if existing := configV8Node(previous, []string{"access", "api-keys"}); existing != nil {
				_ = existing.Decode(&have)
			}
		}
		merged := append([]string(nil), have...)
		seen := make(map[string]bool, len(merged))
		for _, key := range merged {
			seen[key] = true
		}
		for _, key := range incoming {
			key = strings.TrimSpace(key)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, key)
		}
		setConfigV8Node(next, []string{"access", "api-keys"}, yamlStringList(merged))
	}
	setConfigV8Node(next, []string{"api-keys"}, cloneConfigV8Node(prevGroups))
}

func yamlStringList(values []string) *yaml.Node {
	node := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, value := range values {
		node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
	}
	return node
}

func setConfigV8Node(root *yaml.Node, parts []string, value *yaml.Node) {
	if root == nil || root.Kind != yaml.MappingNode || len(parts) == 0 || value == nil {
		return
	}
	for _, part := range parts[:len(parts)-1] {
		next := configV8Node(root, []string{part})
		if next == nil || next.Kind != yaml.MappingNode {
			next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: part}, next)
		}
		root = next
	}
	key := parts[len(parts)-1]
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			root.Content[i+1] = value
			return
		}
	}
	root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

func configV8Node(root *yaml.Node, parts []string) *yaml.Node {
	for _, part := range parts {
		if root == nil || root.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for i := 0; i < len(root.Content); i += 2 {
			if root.Content[i].Value == part {
				next = root.Content[i+1]
				break
			}
		}
		root = next
	}
	return root
}

// deleteConfigV8Path prunes only empty ancestors of the removed field. Other
// explicit empty maps can carry inheritance or plugin semantics and stay intact.
func deleteConfigV8Path(root *yaml.Node, parts []string) bool {
	if root == nil || root.Kind != yaml.MappingNode || len(parts) == 0 {
		return false
	}
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value != parts[0] {
			continue
		}
		if len(parts) > 1 {
			child := root.Content[i+1]
			if !deleteConfigV8Path(child, parts[1:]) {
				return false
			}
			if len(child.Content) > 0 {
				return true
			}
		}
		root.Content = append(root.Content[:i], root.Content[i+2:]...)
		return true
	}
	return false
}

func cloneConfigV8Node(node *yaml.Node) *yaml.Node {
	copy := *node
	copy.Content = make([]*yaml.Node, len(node.Content))
	for i, child := range node.Content {
		copy.Content[i] = cloneConfigV8Node(child)
	}
	return &copy
}

// Unlike JSON merge-patch, null is retained: optional key overrides use it to
// inherit the group value. DELETE is the explicit field-removal operation.
func mergeConfigV8Patch(dst, src *yaml.Node) {
	if dst.Kind != yaml.MappingNode || src.Kind != yaml.MappingNode {
		*dst = *src
		return
	}
	for i := 0; i < len(src.Content); i += 2 {
		key, value := src.Content[i], src.Content[i+1]
		if old := configV8Node(dst, []string{key.Value}); old != nil {
			mergeConfigV8Patch(old, value)
		} else {
			dst.Content = append(dst.Content, key, value)
		}
	}
}
