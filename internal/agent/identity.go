package agent

import (
	"context"
	"time"
)

// Identity is a public, argument-free role-candidate identity. Name is the
// stable candidate label used for fallback attempts and session ownership;
// ModelProvider and Model fill durable invocation identity when an adapter does
// not report those values itself.
type Identity struct {
	Name          string
	ModelProvider string
	Model         string
}

// WithIdentity binds an adapter instance to one structured role candidate.
// This is what keeps two candidates backed by the same harness distinct without
// exposing their raw launch arguments in logs or persistence.
func WithIdentity(inner Agent, identity Identity) Agent {
	if inner == nil {
		return nil
	}
	if identity.Name == "" {
		identity.Name = inner.Name()
	}
	return &identityAgent{inner: inner, identity: identity}
}

type identityAgent struct {
	inner    Agent
	identity Identity
}

func (a *identityAgent) Name() string { return a.identity.Name }

func (a *identityAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	attempts := 0
	previous := opts.OnAttempt
	opts.OnAttempt = func(attempt Attempt) {
		attempts++
		attempt.Agent = a.identity.Name
		if attempt.Result == nil {
			attempt.Result = &Result{}
		}
		attempt.Result = a.resultWithIdentity(attempt.Result)
		if previous != nil {
			previous(attempt)
		}
	}
	startedAt := time.Now()
	result, err := a.inner.Run(ctx, opts)
	result = a.resultWithIdentity(result)
	if attempts == 0 && previous != nil {
		attemptResult := result
		if attemptResult == nil {
			attemptResult = a.resultWithIdentity(&Result{})
		}
		previous(Attempt{
			Agent:           a.identity.Name,
			Result:          attemptResult,
			Err:             err,
			StartedAt:       startedAt,
			CompletedAt:     time.Now(),
			Session:         cloneSessionRef(opts.Session),
			SessionFallback: opts.SessionFallback,
		})
	}
	return result, err
}

func (a *identityAgent) resultWithIdentity(result *Result) *Result {
	if result == nil {
		return nil
	}
	result.Provider = a.identity.Name
	if result.ModelProvider == "" {
		result.ModelProvider = a.identity.ModelProvider
	}
	if result.Model == "" {
		result.Model = a.identity.Model
	}
	return result
}

func (a *identityAgent) Close() error { return a.inner.Close() }

func (a *identityAgent) SupportsSessionResume() bool {
	return SupportsSessionResume(a.inner)
}

func (a *identityAgent) SupportsSessionProvider(provider string) bool {
	return provider == a.identity.Name && SupportsSessionResume(a.inner)
}

// ReportsAgentAttempts is true because Run forwards every inner attempt and
// synthesizes one adapter attempt when the wrapped adapter does not report its
// own.
func (a *identityAgent) ReportsAgentAttempts() bool { return true }

func (a *identityAgent) NeutralizesGateInstructions() bool {
	return NeutralizesGateInstructions(a.inner)
}
