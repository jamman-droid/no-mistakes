package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
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

type identityLeakingAgent struct {
	err error
}

func (a *identityLeakingAgent) Name() string { return "pi" }
func (a *identityLeakingAgent) Run(_ context.Context, opts RunOpts) (*Result, error) {
	now := time.Now()
	if opts.OnChunk != nil {
		opts.OnChunk(a.err.Error())
	}
	if opts.OnLifecycle != nil {
		opts.OnLifecycle(LifecycleEvent{Agent: "pi", Phase: LifecyclePhaseExit, Message: a.err.Error()})
	}
	if opts.OnAttempt != nil {
		opts.OnAttempt(Attempt{Agent: "pi", Err: a.err, StartedAt: now, CompletedAt: now})
	}
	return nil, a.err
}
func (a *identityLeakingAgent) Close() error { return nil }

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

func TestWithIdentityRedactsLaunchArgumentsAtEveryOutputBoundary(t *testing.T) {
	secretArg := "--api-key=top-secret-value"
	sentinel := errors.New("unavailable")
	leaked := fmt.Errorf("pi start: argv %s flag --api-key value top-secret-value: %w", secretArg, sentinel)
	wrapped := WithIdentity(
		&identityLeakingAgent{err: leaked},
		Identity{Name: "pi[provider=openai;model=gpt-5.4]", ModelProvider: "openai", Model: "gpt-5.4"},
		secretArg,
	)

	var observed []string
	_, err := wrapped.Run(context.Background(), RunOpts{
		OnChunk: func(text string) { observed = append(observed, text) },
		OnLifecycle: func(event LifecycleEvent) {
			observed = append(observed, event.Agent, event.Message)
		},
		OnAttempt: func(attempt Attempt) {
			if attempt.Err != nil {
				observed = append(observed, attempt.Err.Error())
			}
		},
	})
	if err == nil {
		t.Fatal("Run error = nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run error lost wrapped cause: %v", err)
	}
	observed = append(observed, err.Error())

	success := WithIdentity(&identityTestAgent{name: "pi", result: &Result{Text: "ok"}}, Identity{Name: "pi[provider=backup]"})
	_, fallbackErr := NewFallback([]Agent{wrapped, success}).Run(context.Background(), RunOpts{
		OnChunk: func(text string) { observed = append(observed, text) },
	})
	if fallbackErr != nil {
		t.Fatalf("fallback Run: %v", fallbackErr)
	}

	for _, text := range observed {
		for _, secret := range []string{secretArg, "--api-key", "top-secret-value"} {
			if strings.Contains(text, secret) {
				t.Fatalf("output %q leaked configured argument token %q", text, secret)
			}
		}
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
