package management

import (
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"sort"
)

type executionCredential struct {
	ID                   string                               `json:"id"`
	Provider             string                               `json:"provider"`
	AuthKind             string                               `json:"auth_kind"`
	SourceKind           string                               `json:"source_kind"`
	Prefix               string                               `json:"prefix"`
	SourceRef            string                               `json:"source_ref"`
	Disabled             bool                                 `json:"disabled"`
	CompatibleOperations []runtimeexecutor.VoiceHTTPOperation `json:"compatible_operations"`
}

// buildExecutionCredentials is independent of legacy files filtering/pagination.
func (h *Handler) buildExecutionCredentials() []executionCredential {
	out := []executionCredential{}
	if h == nil || h.authManager == nil {
		return out
	}
	for _, auth := range h.authManager.List() {
		if auth == nil {
			continue
		}
		executor, _ := h.authManager.Executor(auth.Provider)
		out = append(out, executionCredential{ID: auth.ID, Provider: auth.Provider, AuthKind: auth.AuthKind(), SourceKind: auth.AuthSourceKind(), Prefix: auth.Prefix, SourceRef: auth.EnsureIndex(), Disabled: auth.Disabled || auth.Status == coreauth.StatusDisabled, CompatibleOperations: runtimeexecutor.VoiceHTTPCompatibleOperations(auth, executor, h.authManager.SupportsVoiceHTTPTransport(auth.ID), h.authManager.HomeEnabled())})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
