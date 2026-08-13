package agent

import (
	"context"
	"strings"
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
func WithIdentity(inner Agent, identity Identity, launchArgs ...string) Agent {
	if inner == nil {
		return nil
	}
	if identity.Name == "" {
		identity.Name = inner.Name()
	}
	return &identityAgent{inner: inner, identity: identity, redactor: newArgumentRedactor(launchArgs)}
}

type identityAgent struct {
	inner    Agent
	identity Identity
	redactor argumentRedactor
}

func (a *identityAgent) Name() string { return a.identity.Name }

func (a *identityAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	attempts := 0
	previousAttempt := opts.OnAttempt
	opts.OnAttempt = func(attempt Attempt) {
		attempts++
		attempt.Agent = a.identity.Name
		attempt.Err = a.redactor.redactError(attempt.Err)
		if attempt.Result == nil {
			attempt.Result = &Result{}
		}
		attempt.Result = a.resultWithIdentity(attempt.Result)
		if previousAttempt != nil {
			previousAttempt(attempt)
		}
	}
	if previousChunk := opts.OnChunk; previousChunk != nil {
		opts.OnChunk = func(text string) {
			previousChunk(a.redactor.redact(text))
		}
	}
	if previousLifecycle := opts.OnLifecycle; previousLifecycle != nil {
		opts.OnLifecycle = func(event LifecycleEvent) {
			event.Message = a.redactor.redact(event.Message)
			previousLifecycle(event)
		}
	}
	startedAt := time.Now()
	result, err := a.inner.Run(ctx, opts)
	result = a.resultWithIdentity(result)
	err = a.redactor.redactError(err)
	if attempts == 0 && previousAttempt != nil {
		attemptResult := result
		if attemptResult == nil {
			attemptResult = a.resultWithIdentity(&Result{})
		}
		previousAttempt(Attempt{
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

type argumentRedactor struct {
	tokens []string
}

func newArgumentRedactor(args []string) argumentRedactor {
	seen := make(map[string]struct{}, len(args)*3)
	var tokens []string
	add := func(token string) {
		if token == "" {
			return
		}
		if _, ok := seen[token]; ok {
			return
		}
		seen[token] = struct{}{}
		tokens = append(tokens, token)
	}
	for _, arg := range args {
		add(arg)
		if index := strings.IndexByte(arg, '='); index > 0 {
			add(arg[:index])
			add(arg[index+1:])
		}
	}
	return argumentRedactor{tokens: tokens}
}

func (r argumentRedactor) redact(text string) string {
	if text == "" || len(r.tokens) == 0 {
		return text
	}
	var output strings.Builder
	start := 0
	for start < len(text) {
		matchAt := -1
		matchLen := 0
		for _, token := range r.tokens {
			index := strings.Index(text[start:], token)
			if index < 0 {
				continue
			}
			index += start
			if matchAt == -1 || index < matchAt || index == matchAt && len(token) > matchLen {
				matchAt = index
				matchLen = len(token)
			}
		}
		if matchAt == -1 {
			output.WriteString(text[start:])
			break
		}
		output.WriteString(text[start:matchAt])
		output.WriteString("[REDACTED]")
		start = matchAt + matchLen
	}
	return output.String()
}

func (r argumentRedactor) redactError(err error) error {
	if err == nil {
		return nil
	}
	text := r.redact(err.Error())
	if text == err.Error() {
		return err
	}
	return redactedAgentError{text: text, cause: err}
}

type redactedAgentError struct {
	text  string
	cause error
}

func (e redactedAgentError) Error() string { return e.text }
func (e redactedAgentError) Unwrap() error { return e.cause }

func (a *identityAgent) Close() error { return a.redactor.redactError(a.inner.Close()) }

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
