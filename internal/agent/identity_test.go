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
		opts.OnLifecycle(LifecycleEvent{Agent: a.err.Error(), Phase: LifecyclePhaseExit, Message: a.err.Error()})
	}
	if opts.OnAttempt != nil {
		opts.OnAttempt(Attempt{Agent: "pi", Err: a.err, StartedAt: now, CompletedAt: now})
	}
	return nil, a.err
}
func (a *identityLeakingAgent) Close() error { return nil }

type identitySecretError struct {
	text string
}

func (e *identitySecretError) Error() string { return e.text }

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
	if provider, model := result.ObservedModelIdentity(); provider != "" || model != "" {
		t.Fatalf("declared identity was fabricated as observed: %q/%q", provider, model)
	}
}

func TestWithIdentityPreservesAdapterObservedModelSeparately(t *testing.T) {
	wrapped := WithIdentity(
		&identityTestAgent{name: "pi", result: &Result{ModelProvider: "runtime-provider", Model: "runtime-model"}},
		Identity{Name: "candidate", ModelProvider: "declared-provider", Model: "declared-model"},
	)
	result, err := wrapped.Run(context.Background(), RunOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if provider, model := result.ObservedModelIdentity(); provider != "runtime-provider" || model != "runtime-model" {
		t.Fatalf("observed identity = %q/%q", provider, model)
	}
	if result.ModelProvider != "runtime-provider" || result.Model != "runtime-model" {
		t.Fatalf("effective identity changed adapter report: %+v", result)
	}
}

func TestSafeObservedIdentityRejectsPublicBoundaryContent(t *testing.T) {
	if got, ok := SafeObservedIdentity("openai/gpt-5.6-sol"); !ok || got != "openai/gpt-5.6-sol" {
		t.Fatalf("safe observed identity = %q/%v", got, ok)
	}
	for _, unsafe := range []string{"https://user:secret@example.com/model", "file:/etc/passwd", "C:/Users/alice/.ssh/key", "/Users/test/model", "models/../../Users/alice/key", "../secret", "provider@example.com", "model with output", strings.Repeat("x", 129)} {
		if got, ok := SafeObservedIdentity(unsafe); ok {
			t.Fatalf("unsafe observed identity %q accepted as %q", unsafe, got)
		}
	}
}

func TestWithIdentityRedactsLaunchArgumentsAtEveryOutputBoundary(t *testing.T) {
	secretArg := "--api-key=top-secret-value"
	secretCause := &identitySecretError{text: "top-secret-value"}
	leaked := fmt.Errorf("pi start: argv %s flag --api-key value top-secret-value: %w", secretArg, secretCause)
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
	if errors.Is(err, secretCause) {
		t.Fatal("Run error exposes its secret-bearing cause through errors.Is")
	}
	var recovered *identitySecretError
	if errors.As(err, &recovered) {
		t.Fatal("Run error exposes its secret-bearing cause through errors.As")
	}
	if unwrapped := errors.Unwrap(err); unwrapped != nil {
		t.Fatalf("Run error unwrap = %v, want nil", unwrapped)
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

func TestWithIdentityRedactedErrorsPreserveContextCancellation(t *testing.T) {
	secretArg := "--api-key=top-secret-value"
	wrapped := WithIdentity(
		&identityTestAgent{name: "pi", err: fmt.Errorf("pi failed with %s: %w", secretArg, context.Canceled)},
		Identity{Name: "pi[provider=openai]"},
		secretArg,
	)

	_, err := wrapped.Run(context.Background(), RunOpts{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
	if unwrapped := errors.Unwrap(err); unwrapped != nil {
		t.Fatalf("Run error unwrap = %v, want nil", unwrapped)
	}
}

func TestWithIdentityRedactedArgumentDoesNotDisableFallback(t *testing.T) {
	firstBase := &identityTestAgent{name: "pi", err: errors.New("pi start: first unavailable")}
	secondBase := &identityTestAgent{name: "pi", result: &Result{Text: "ok"}}
	first := WithIdentity(firstBase, Identity{Name: "pi[first]"}, "start")
	second := WithIdentity(secondBase, Identity{Name: "pi[second]"})

	result, err := NewFallback([]Agent{first, second}).Run(context.Background(), RunOpts{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if firstBase.calls != 1 || secondBase.calls != 1 || result.Text != "ok" {
		t.Fatalf("fallback calls = %d/%d, result = %+v", firstBase.calls, secondBase.calls, result)
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
