package pipeline

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type roleRecordingAgent struct {
	name  string
	mu    sync.Mutex
	calls []string
}

func (a *roleRecordingAgent) Name() string { return a.name }
func (a *roleRecordingAgent) Close() error { return nil }
func (a *roleRecordingAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.mu.Lock()
	a.calls = append(a.calls, opts.Purpose)
	a.mu.Unlock()
	return &agent.Result{}, nil
}

func TestRoleSelectionAndRuntimeFallbackAreObservable(t *testing.T) {
	first := &roleRecordingAgent{name: "codex"}
	second := &roleRecordingAgent{name: "claude"}
	failing := &runOverrideAgent{Agent: first, run: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return nil, errors.New("codex start: executable unavailable")
	}}
	fallback := agent.NewFallback([]agent.Agent{failing, second})
	var logs []string
	observable := &observableRoleAgent{
		inner:      fallback,
		role:       "reviewer",
		candidates: []string{"codex", "claude"},
		log:        func(line string) { logs = append(logs, line) },
	}
	var chunks strings.Builder
	var attempts []agent.Attempt
	if _, err := observable.Run(context.Background(), agent.RunOpts{
		OnChunk:   func(chunk string) { chunks.WriteString(chunk) },
		OnAttempt: func(attempt agent.Attempt) { attempts = append(attempts, attempt) },
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	joinedLogs := strings.Join(logs, "\n")
	for _, want := range []string{
		"role=reviewer primary=codex fallbacks=[claude]",
		"agent attempt: role=reviewer agent=codex status=failed",
		"agent attempt: role=reviewer agent=claude status=succeeded",
	} {
		if !strings.Contains(joinedLogs, want) {
			t.Fatalf("selection logs missing %q: %v", want, logs)
		}
	}
	if got := chunks.String(); !strings.Contains(got, "agent codex failed") || !strings.Contains(got, "falling back to claude") {
		t.Fatalf("fallback stream = %q", got)
	}
	if len(attempts) != 2 || attempts[0].Agent != "codex" || attempts[1].Agent != "claude" {
		t.Fatalf("structured attempts = %+v", attempts)
	}
}

type runOverrideAgent struct {
	agent.Agent
	run func(context.Context, agent.RunOpts) (*agent.Result, error)
}

func (a *runOverrideAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	return a.run(ctx, opts)
}

func TestExecutorRoutesReviewerAndImplementerRoles(t *testing.T) {
	database, p, run, repo := setupTest(t)
	implementer := &roleRecordingAgent{name: "implementer-agent"}
	reviewer := &roleRecordingAgent{name: "reviewer-agent"}
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if _, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Purpose: "review-fix"}); err != nil {
				return nil, err
			}
			if _, err := sctx.ReviewerAgent.Run(sctx.Ctx, agent.RunOpts{Purpose: "review"}); err != nil {
				return nil, err
			}
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutorWithAgentRoles(database, p, nil, AgentRoles{
		Implementer: implementer,
		Reviewer:    reviewer,
	}, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got := append([]string(nil), implementer.calls...); len(got) != 1 || got[0] != "review-fix" {
		t.Fatalf("implementer calls = %v, want [review-fix]", got)
	}
	if got := append([]string(nil), reviewer.calls...); len(got) != 1 || got[0] != "review" {
		t.Fatalf("reviewer calls = %v, want [review]", got)
	}
}
