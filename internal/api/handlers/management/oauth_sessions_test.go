package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestOAuthSessionStoreCompleteKeepsShortLivedSession(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	store.Register("completed-state", "codex")

	store.Complete("completed-state")

	if _, ok := store.Get("completed-state"); !ok {
		t.Fatal("completed OAuth session was deleted instead of retained as a tombstone")
	}
	if store.IsPending("completed-state", "codex") {
		t.Fatal("completed OAuth session remained pending")
	}
}

func TestOAuthSessionStoreCompleteDoesNotExtendCompletedSession(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	store.Register("completed-state", "codex")
	store.Complete("completed-state")
	before, ok := store.Get("completed-state")
	if !ok {
		t.Fatal("completed OAuth session tombstone is missing")
	}

	store.completedTTL = 2 * time.Minute
	store.Complete("completed-state")
	after, ok := store.Get("completed-state")
	if !ok {
		t.Fatal("completed OAuth session tombstone is missing after repeated completion")
	}
	if !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatalf("repeated completion extended expiry from %s to %s", before.ExpiresAt, after.ExpiresAt)
	}
}

func TestOAuthSessionStoreCompleteProviderSkipsCompletedSessions(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	store.Register("completed-state", "codex")
	store.Register("pending-state", "codex")
	store.Complete("completed-state")
	completedBefore, ok := store.Get("completed-state")
	if !ok {
		t.Fatal("completed OAuth session tombstone is missing")
	}

	store.completedTTL = 2 * time.Minute
	if got := store.CompleteProvider("codex", oauthSessionSourceBuiltin); got != 1 {
		t.Fatalf("CompleteProvider() = %d, want 1 newly completed session", got)
	}
	completedAfter, ok := store.Get("completed-state")
	if !ok {
		t.Fatal("completed OAuth session tombstone is missing after provider completion")
	}
	if !completedAfter.ExpiresAt.Equal(completedBefore.ExpiresAt) {
		t.Fatalf("provider completion extended existing tombstone from %s to %s", completedBefore.ExpiresAt, completedAfter.ExpiresAt)
	}
	pendingAfter, ok := store.Get("pending-state")
	if !ok || !pendingAfter.Completed {
		t.Fatalf("pending session completed/ok = %t/%t, want true/true", pendingAfter.Completed, ok)
	}
}

func TestGetOAuthSessionHidesCompletedSession(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	replaceOAuthSessionStoreForTest(t, store)
	store.Register("completed-state", "codex")
	store.Complete("completed-state")

	provider, status, ok := GetOAuthSession("completed-state")
	if ok {
		t.Fatalf("GetOAuthSession() = (%q, %q, true), want completed session hidden", provider, status)
	}

	_, _, _, _, completed, detailsOK := GetOAuthSessionDetails("completed-state")
	if !detailsOK || !completed {
		t.Fatalf("GetOAuthSessionDetails() completed/ok = %t/%t, want true/true", completed, detailsOK)
	}
}

func TestGetAuthStatusRejectsUnknownStateAndAcceptsCompletedState(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	replaceOAuthSessionStoreForTest(t, store)

	handler := &Handler{}
	router := gin.New()
	router.GET("/status", handler.GetAuthStatus)

	unknown := performOAuthStatusRequest(t, router, "unknown-state")
	if unknown.Status != "error" || unknown.Error != "unknown or expired state" {
		t.Fatalf("unknown state response = %#v, want unknown/expired error", unknown)
	}

	store.Register("completed-state", "codex")
	store.Complete("completed-state")
	completed := performOAuthStatusRequest(t, router, "completed-state")
	if completed.Status != "ok" || completed.Error != "" {
		t.Fatalf("completed state response = %#v, want success", completed)
	}
}

func TestOAuthCallbackRejectsCompletedSession(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	replaceOAuthSessionStoreForTest(t, store)
	store.Register("completed-state", "codex")
	store.Complete("completed-state")

	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	router := gin.New()
	router.POST("/oauth-callback", handler.PostOAuthCallback)

	req := httptest.NewRequest(
		http.MethodPost,
		"/oauth-callback",
		strings.NewReader(`{"provider":"codex","state":"completed-state","code":"test-code"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("completed callback status = %d, want %d; body=%s", w.Code, http.StatusConflict, w.Body.String())
	}
}

type oauthStatusResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}

func performOAuthStatusRequest(t *testing.T, router http.Handler, state string) oauthStatusResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/status?state="+state, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status request returned %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var response oauthStatusResponse
	if errDecode := json.Unmarshal(w.Body.Bytes(), &response); errDecode != nil {
		t.Fatalf("decode status response: %v", errDecode)
	}
	return response
}

func TestOAuthSessionStoreCancelRemovesPendingSession(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	store.Register("pending-state", "xai")

	if !store.Cancel("pending-state") {
		t.Fatal("Cancel() = false, want true for pending session")
	}
	if store.IsPending("pending-state", "xai") {
		t.Fatal("cancelled session remained pending")
	}
	if _, ok := store.Get("pending-state"); ok {
		t.Fatal("cancelled session still present in store")
	}
	if store.Cancel("pending-state") {
		t.Fatal("second Cancel() = true, want false")
	}
}

func TestOAuthSessionStoreCancelIgnoresCompletedAndUnknown(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	store.Register("completed-state", "codex")
	store.Complete("completed-state")

	if store.Cancel("completed-state") {
		t.Fatal("Cancel() completed session = true, want false")
	}
	if _, ok := store.Get("completed-state"); !ok {
		t.Fatal("completed tombstone was removed by Cancel")
	}
	if store.Cancel("missing-state") {
		t.Fatal("Cancel() unknown session = true, want false")
	}
}

func TestOAuthSessionStoreCancelIgnoresErrorSession(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	store.Register("error-state", "kimi")
	store.SetError("error-state", "Authentication failed")

	if store.IsPending("error-state", "kimi") {
		t.Fatal("error session should not be pending")
	}
	if store.Cancel("error-state") {
		t.Fatal("Cancel() error session = true, want false")
	}
}

func TestCancelOAuthSessionAndCallbackRejectAfterCancel(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	replaceOAuthSessionStoreForTest(t, store)
	store.Register("callback-state", "anthropic")

	if !CancelOAuthSession("callback-state") {
		t.Fatal("CancelOAuthSession() = false, want true")
	}
	if IsOAuthSessionPending("callback-state", "anthropic") {
		t.Fatal("session still pending after cancel")
	}

	_, errWrite := WriteOAuthCallbackFileForPendingSession(t.TempDir(), "anthropic", "callback-state", "code", "")
	if errWrite == nil {
		t.Fatal("expected callback write to fail after cancel")
	}
	if !errors.Is(errWrite, errOAuthSessionNotPending) {
		t.Fatalf("callback write error = %v, want %v", errWrite, errOAuthSessionNotPending)
	}
}

func TestGuardOAuthSessionPendingForSave(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	replaceOAuthSessionStoreForTest(t, store)

	providers := []string{"anthropic", "codex", "antigravity", "xai", "kimi", "kimi-ai", "kimi.ai", "meta"}
	for _, provider := range providers {
		state := provider + "-save-guard"
		store.Register(state, provider)

		if errGuard := guardOAuthSessionPendingForSave(state, provider); errGuard != nil {
			t.Fatalf("%s pending guard error = %v, want nil", provider, errGuard)
		}

		if !CancelOAuthSession(state) {
			t.Fatalf("%s CancelOAuthSession() = false, want true", provider)
		}
		if errGuard := guardOAuthSessionPendingForSave(state, provider); !errors.Is(errGuard, errOAuthSessionNotPending) {
			t.Fatalf("%s after cancel guard error = %v, want %v", provider, errGuard, errOAuthSessionNotPending)
		}
	}

	// Completed and errored sessions must also refuse save.
	store.Register("completed-save", "codex")
	store.Complete("completed-save")
	if errGuard := guardOAuthSessionPendingForSave("completed-save", "codex"); !errors.Is(errGuard, errOAuthSessionNotPending) {
		t.Fatalf("completed guard error = %v, want %v", errGuard, errOAuthSessionNotPending)
	}

	store.Register("error-save", "anthropic")
	store.SetError("error-save", "Authentication failed")
	if errGuard := guardOAuthSessionPendingForSave("error-save", "anthropic"); !errors.Is(errGuard, errOAuthSessionNotPending) {
		t.Fatalf("error guard error = %v, want %v", errGuard, errOAuthSessionNotPending)
	}
}

func TestCancelAuthSessionHandler(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	replaceOAuthSessionStoreForTest(t, store)
	store.Register("device-state", "xai")

	handler := &Handler{}
	router := gin.New()
	router.DELETE("/oauth-session", handler.CancelAuthSession)

	missing := performOAuthCancelRequest(t, router, "")
	if missing.status != http.StatusBadRequest {
		t.Fatalf("missing state status = %d, want %d", missing.status, http.StatusBadRequest)
	}

	invalid := performOAuthCancelRequest(t, router, "bad/state")
	if invalid.status != http.StatusBadRequest {
		t.Fatalf("invalid state status = %d, want %d", invalid.status, http.StatusBadRequest)
	}

	cancelled := performOAuthCancelRequest(t, router, "device-state")
	if cancelled.status != http.StatusOK || !cancelled.cancelled || cancelled.bodyStatus != "ok" {
		t.Fatalf("cancel pending response = %#v, want ok/cancelled", cancelled)
	}
	if IsOAuthSessionPending("device-state", "xai") {
		t.Fatal("device session still pending after cancel API")
	}

	repeat := performOAuthCancelRequest(t, router, "device-state")
	if repeat.status != http.StatusOK || repeat.cancelled {
		t.Fatalf("repeat cancel response = %#v, want ok with cancelled=false", repeat)
	}

	// Status after cancel should not report success.
	statusRouter := gin.New()
	statusRouter.GET("/status", handler.GetAuthStatus)
	unknown := performOAuthStatusRequest(t, statusRouter, "device-state")
	if unknown.Status != "error" || unknown.Error != "unknown or expired state" {
		t.Fatalf("status after cancel = %#v, want unknown/expired error", unknown)
	}
}

type oauthCancelResponse struct {
	status     int
	bodyStatus string
	cancelled  bool
}

func performOAuthCancelRequest(t *testing.T, router http.Handler, state string) oauthCancelResponse {
	t.Helper()
	path := "/oauth-session"
	if state != "" {
		path += "?state=" + state
	}
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var body struct {
		Status    string `json:"status"`
		Cancelled bool   `json:"cancelled"`
		Error     string `json:"error"`
	}
	if w.Body.Len() > 0 {
		if errDecode := json.Unmarshal(w.Body.Bytes(), &body); errDecode != nil {
			t.Fatalf("decode cancel response: %v body=%s", errDecode, w.Body.String())
		}
	}
	return oauthCancelResponse{
		status:     w.Code,
		bodyStatus: body.Status,
		cancelled:  body.Cancelled,
	}
}

func replaceOAuthSessionStoreForTest(t *testing.T, store *oauthSessionStore) {
	t.Helper()
	original := oauthSessions
	oauthSessions = store
	t.Cleanup(func() {
		oauthSessions = original
	})
}

func TestPluginCompletionCredentialIDsAreSessionBoundAndCopied(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	if err := store.RegisterPlugin("bound", "cursor", map[string]any{"private": "must-clear"}); err != nil {
		t.Fatal(err)
	}
	if store.ClaimPluginCompletion("bound", "other") != nil {
		t.Fatal("wrong provider claimed")
	}
	claim := store.ClaimPluginCompletion("bound", "cursor")
	if claim == nil || store.ClaimPluginCompletion("bound", "cursor") != nil {
		t.Fatal("completion must have one owner")
	}
	store.SetError("bound", "competing poll error")
	store.Complete("bound")
	if store.CompleteProvider("cursor", oauthSessionSourcePlugin) != 0 {
		t.Fatal("generic completion stole active owner")
	}
	ids := []string{"saved-id"}
	if !store.CompletePlugin(claim, ids) {
		t.Fatal("owned completion failed")
	}
	ids[0] = "mutated"
	first, _ := store.Get("bound")
	if len(first.CredentialIDs) != 1 || first.CredentialIDs[0] != "saved-id" || first.Metadata != nil {
		t.Fatal("identity or metadata invariant")
	}
	first.CredentialIDs[0] = "changed-copy"
	if store.CompletePlugin(claim, []string{"other-id"}) {
		t.Fatal("completed session overwritten")
	}
	second, _ := store.Get("bound")
	if second.CredentialIDs[0] != "saved-id" || !second.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatal("replay changed identity or TTL")
	}
	second.ExpiresAt = time.Now().Add(-time.Second)
	store.mu.Lock()
	store.sessions["bound"] = second
	store.mu.Unlock()
	if _, ok := store.Get("bound"); ok {
		t.Fatal("expired identity leaked")
	}
}

func TestPluginCompletionOwnerReleaseCancelAndReplacement(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	if err := store.RegisterPlugin("reuse", "cursor", nil); err != nil {
		t.Fatal(err)
	}
	old := store.ClaimPluginCompletion("reuse", "cursor")
	store.ReleasePluginCompletion(old)
	next := store.ClaimPluginCompletion("reuse", "cursor")
	if next == nil || next == old {
		t.Fatal("pending release did not allow a new owner")
	}
	if store.CompletePlugin(old, []string{"stale-id"}) {
		t.Fatal("released owner completed")
	}
	if !store.Cancel("reuse") {
		t.Fatal("active poll must remain cancellable")
	}
	if err := store.RegisterPlugin("reuse", "cursor", nil); err != nil {
		t.Fatal(err)
	}
	fresh := store.ClaimPluginCompletion("reuse", "cursor")
	store.FailPluginCompletion(next, "stale error")
	if store.CompletePlugin(next, []string{"cancelled-id"}) {
		t.Fatal("cancelled owner completed replacement")
	}
	if !store.CompletePlugin(fresh, []string{"fresh-id"}) {
		t.Fatal("stale owner corrupted replacement")
	}
}

func TestGetAuthStatusReturnsOnlyBoundPluginIdentityOnRepeat(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	replaceOAuthSessionStoreForTest(t, store)
	if err := store.RegisterPlugin("bound-status", "cursor", nil); err != nil {
		t.Fatal(err)
	}
	claim := store.ClaimPluginCompletion("bound-status", "cursor")
	if claim == nil || !store.CompletePlugin(claim, []string{"same-existing-id"}) {
		t.Fatal("fixture completion")
	}
	router := gin.New()
	router.GET("/status", (&Handler{}).GetAuthStatus)
	for i := 0; i < 2; i++ {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status?state=bound-status", nil))
		var got struct {
			Status   string   `json:"status"`
			Provider string   `json:"provider"`
			IDs      []string `json:"credential_ids"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Status != "ok" || got.Provider != "cursor" || len(got.IDs) != 1 || got.IDs[0] != "same-existing-id" {
			t.Fatalf("unexpected identity response: %#v", got)
		}
	}
}

func TestPluginClaimCapturesCurrentSessionMetadataAfterReplacement(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	if err := store.RegisterPlugin("metadata-reuse", "cursor", map[string]any{"login": "old"}); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := store.Get("metadata-reuse")
	if !store.Cancel("metadata-reuse") {
		t.Fatal("cancel fixture")
	}
	if err := store.RegisterPlugin("metadata-reuse", "cursor", map[string]any{"login": "new"}); err != nil {
		t.Fatal(err)
	}
	claim := store.ClaimPluginCompletion("metadata-reuse", "cursor")
	if claim == nil || claim.metadata["login"] != "new" {
		t.Fatal("claim reused stale metadata")
	}
	snapshot.Metadata["login"] = "corrupted-old-snapshot"
	fresh, _ := store.Get("metadata-reuse")
	fresh.Metadata["login"] = "changed-copy"
	if claim.metadata["login"] != "new" {
		t.Fatal("claim metadata aliases a read snapshot")
	}
}

func TestBuiltinCompletedStatusDoesNotGainPluginIdentityFields(t *testing.T) {
	store := newOAuthSessionStore(time.Minute)
	replaceOAuthSessionStoreForTest(t, store)
	store.Register("builtin-contract", "codex")
	store.Complete("builtin-contract")
	router := gin.New()
	router.GET("/status", (&Handler{}).GetAuthStatus)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status?state=builtin-contract", nil))
	var got map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || string(got["status"]) != `"ok"` {
		t.Fatalf("builtin contract drift: %s", response.Body.String())
	}
}
