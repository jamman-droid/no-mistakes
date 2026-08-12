package agent

import (
	"context"
	"errors"
	"testing"
)

type identityTestAgent struct {
	name      string
	result    *Result
	err       error
	resumable bool
	calls     int
}

func (a *identityTestAgent) Name() string { return a.name }
func (a *identityTestAgent) Run(_ context.Context, _ RunOpts) (*Result, error) {
	a.calls++
	return a.result, a.err
}
func (a *identityTestAgent) Close() error                { return nil }
func (a *identityTestAgent) SupportsSessionResume() bool { return a.resumable }

func TestWithIdentityMakesSameHarnessFallbacksDistinctAndOrdered(t *testing.T) {
	firstBase := &identityTestAgent{name: "pi", err: errors.New("pi start: first unavailable")}
	secondBase := &identityTestAgent{name: "pi", result: &Result{Text: "ok"}}
	first := WithIdentity(firstBase, Identity{Name: "pi[openai/gpt-5.4]", ModelProvider: "openai", Model: "gpt-5.4"})
	second := WithIdentity(secondBase, Identity{Name: "pi[anthropic/claude-opus-4-6]", ModelProvider: "anthropic", Model: "claude-opus-4-6"})

	var attempts []Attempt
	result, err := NewFallback([]Agent{first, second}).Run(context.Background(), RunOpts{OnAttempt: func(attempt Attempt) {
		attempts = append(attempts, attempt)
	}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if firstBase.calls != 1 || secondBase.calls != 1 {
		t.Fatalf("calls = first %d, second %d", firstBase.calls, secondBase.calls)
	}
	if len(attempts) != 2 || attempts[0].Agent != first.Name() || attempts[1].Agent != second.Name() {
		t.Fatalf("attempts = %+v", attempts)
	}
	if attempts[0].Result == nil || attempts[0].Result.Model != "gpt-5.4" || attempts[0].Result.ModelProvider != "openai" {
		t.Fatalf("failed candidate identity = %+v", attempts[0].Result)
	}
	if result.Provider != second.Name() || result.Model != "claude-opus-4-6" || result.ModelProvider != "anthropic" {
		t.Fatalf("result identity = %+v", result)
	}
}

func TestWithIdentityBindsSessionsToExactCandidate(t *testing.T) {
	firstBase := &identityTestAgent{name: "pi", result: &Result{SessionID: "wrong"}, resumable: true}
	secondBase := &identityTestAgent{name: "pi", result: &Result{SessionID: "right"}, resumable: true}
	first := WithIdentity(firstBase, Identity{Name: "pi[openai/gpt-5.4]", ModelProvider: "openai", Model: "gpt-5.4"})
	second := WithIdentity(secondBase, Identity{Name: "pi[openai/gpt-5.3]", ModelProvider: "openai", Model: "gpt-5.3"})

	result, err := NewFallback([]Agent{first, second}).Run(context.Background(), RunOpts{Session: &SessionRef{ID: "session", Agent: second.Name()}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if firstBase.calls != 0 || secondBase.calls != 1 || result.Provider != second.Name() {
		t.Fatalf("session resumed on wrong candidate: first=%d second=%d result=%+v", firstBase.calls, secondBase.calls, result)
	}
	if SupportsSessionProvider(first, "pi") {
		t.Fatal("structured candidate must not claim a legacy harness-only session identity")
	}
}
