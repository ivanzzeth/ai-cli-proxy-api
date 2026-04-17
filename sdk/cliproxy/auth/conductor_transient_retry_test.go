package auth

import (
	"context"
	"testing"
	"time"
)

func TestNextTransientCooldown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		prevLevel      int
		disableCooling bool
		wantCooldown   time.Duration
		wantLevel      int
	}{
		{"level 0", 0, false, 1 * time.Second, 1},
		{"level 1", 1, false, 2 * time.Second, 2},
		{"level 2", 2, false, 4 * time.Second, 3},
		{"level 3", 3, false, 8 * time.Second, 4},
		{"level 4", 4, false, 16 * time.Second, 5},
		{"level 5 caps at max", 5, false, 30 * time.Second, 5},
		{"level 10 still capped", 10, false, 30 * time.Second, 10},
		{"negative level treated as 0", -1, false, 1 * time.Second, 1},
		{"disabled cooling", 0, false, 1 * time.Second, 1},
		{"cooling disabled returns zero", 3, true, 0, 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cooldown, level := nextTransientCooldown(tt.prevLevel, tt.disableCooling)
			if cooldown != tt.wantCooldown {
				t.Errorf("cooldown = %v, want %v", cooldown, tt.wantCooldown)
			}
			if level != tt.wantLevel {
				t.Errorf("level = %d, want %d", level, tt.wantLevel)
			}
		})
	}
}

func newTestManager(auths map[string]*Auth) *Manager {
	return &Manager{
		auths: auths,
		hook:  NoopHook{},
	}
}

func TestMarkResult_529SetsTransientBackoff(t *testing.T) {
	t.Parallel()

	model := "MiniMax-M1"
	auth := &Auth{
		ID:       "test-auth",
		Provider: "openai",
		ModelStates: map[string]*ModelState{
			model: {Status: StatusActive},
		},
	}

	m := newTestManager(map[string]*Auth{"test-auth": auth})

	m.MarkResult(context.Background(), Result{
		AuthID:   "test-auth",
		Provider: "openai",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: 529, Message: "server overloaded"},
	})

	state := auth.ModelStates[model]
	if state.TransientBackoffLevel != 1 {
		t.Fatalf("TransientBackoffLevel = %d, want 1", state.TransientBackoffLevel)
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatal("NextRetryAfter should be set after 529")
	}
	if !state.Unavailable {
		t.Fatal("state should be marked unavailable")
	}
}

func TestMarkResult_500SetsTransientBackoff(t *testing.T) {
	t.Parallel()

	model := "deepseek-chat"
	auth := &Auth{
		ID:       "test-auth",
		Provider: "openai",
		ModelStates: map[string]*ModelState{
			model: {Status: StatusActive},
		},
	}

	m := newTestManager(map[string]*Auth{"test-auth": auth})

	m.MarkResult(context.Background(), Result{
		AuthID:   "test-auth",
		Provider: "openai",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: 500, Message: "internal server error"},
	})

	state := auth.ModelStates[model]
	if state.TransientBackoffLevel != 1 {
		t.Fatalf("TransientBackoffLevel = %d, want 1", state.TransientBackoffLevel)
	}
	if state.NextRetryAfter.IsZero() {
		t.Fatal("NextRetryAfter should be set after 500")
	}
}

func TestTransientBackoff_ExponentialGrowth(t *testing.T) {
	t.Parallel()

	model := "test-model"
	auth := &Auth{
		ID:       "test-auth",
		Provider: "openai",
		ModelStates: map[string]*ModelState{
			model: {Status: StatusActive},
		},
	}

	m := newTestManager(map[string]*Auth{"test-auth": auth})

	expectedLevels := []int{1, 2, 3, 4, 5, 5}

	for i, wantLevel := range expectedLevels {
		before := time.Now()
		m.MarkResult(context.Background(), Result{
			AuthID:   "test-auth",
			Provider: "openai",
			Model:    model,
			Success:  false,
			Error:    &Error{HTTPStatus: 529, Message: "overloaded"},
		})

		state := auth.ModelStates[model]
		if state.TransientBackoffLevel != wantLevel {
			t.Fatalf("iteration %d: TransientBackoffLevel = %d, want %d", i, state.TransientBackoffLevel, wantLevel)
		}
		if state.NextRetryAfter.Before(before) {
			t.Fatalf("iteration %d: NextRetryAfter should be in the future", i)
		}
	}

	state := auth.ModelStates[model]
	if state.TransientBackoffLevel != 5 {
		t.Fatalf("after cap: TransientBackoffLevel = %d, want 5 (capped)", state.TransientBackoffLevel)
	}
}

func TestTransientBackoff_ResetOnSuccess(t *testing.T) {
	t.Parallel()

	model := "test-model"
	auth := &Auth{
		ID:       "test-auth",
		Provider: "openai",
		ModelStates: map[string]*ModelState{
			model: {
				Status:                StatusError,
				Unavailable:           true,
				TransientBackoffLevel: 3,
			},
		},
	}

	m := newTestManager(map[string]*Auth{"test-auth": auth})

	m.MarkResult(context.Background(), Result{
		AuthID:   "test-auth",
		Provider: "openai",
		Model:    model,
		Success:  true,
	})

	state := auth.ModelStates[model]
	if state.TransientBackoffLevel != 0 {
		t.Fatalf("TransientBackoffLevel = %d, want 0 after success", state.TransientBackoffLevel)
	}
	if state.Unavailable {
		t.Fatal("state should not be unavailable after success")
	}
	if !state.NextRetryAfter.IsZero() {
		t.Fatal("NextRetryAfter should be zero after success")
	}
}

func TestTransientBackoff_DisabledCooling(t *testing.T) {
	t.Parallel()

	model := "test-model"
	auth := &Auth{
		ID:       "test-auth",
		Provider: "openai",
		Metadata: map[string]any{"disable_cooling": true},
		ModelStates: map[string]*ModelState{
			model: {Status: StatusActive},
		},
	}

	m := newTestManager(map[string]*Auth{"test-auth": auth})

	m.MarkResult(context.Background(), Result{
		AuthID:   "test-auth",
		Provider: "openai",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: 529, Message: "overloaded"},
	})

	state := auth.ModelStates[model]
	if !state.NextRetryAfter.IsZero() {
		t.Fatal("NextRetryAfter should be zero when cooling is disabled")
	}
}
