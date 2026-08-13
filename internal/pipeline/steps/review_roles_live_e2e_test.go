//go:build e2e

package steps

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestLiveCodexReviewerRoleE2E is credentialed proof of the production role
// path: a real Codex adapter reviews a real git change through Executor and the
// resulting step log names the selected reviewer. It is intentionally opt-in;
// ordinary e2e and CI runs never spend credentials or model quota.
func TestLiveCodexReviewerRoleE2E(t *testing.T) {
	if os.Getenv("NO_MISTAKES_LIVE_AGENT_ROLES_E2E") != "1" {
		t.Skip("set NO_MISTAKES_LIVE_AGENT_ROLES_E2E=1 to run the credentialed Codex reviewer proof")
	}
	codexPath, err := exec.LookPath("codex")
	if err != nil {
		t.Fatalf("find codex: %v", err)
	}
	reviewer, err := agent.NewWithOptions(types.AgentCodex, codexPath, nil, agent.Options{})
	if err != nil {
		t.Fatalf("create Codex reviewer: %v", err)
	}
	defer reviewer.Close()

	workDir, baseSHA, headSHA := setupGitRepo(t)
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepo(workDir, "https://github.com/example/role-proof", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "refs/heads/feature", headSHA, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	root := paths.WithRoot(t.TempDir())
	cfg := &config.Config{
		Agent:  types.AgentCodex,
		Agents: []types.AgentName{types.AgentCodex},
		AgentRoles: config.AgentRoles{
			Reviewer:    config.RoleSelection{{Harness: types.AgentCodex}},
			Implementer: config.RoleSelection{{Harness: types.AgentCodex}},
		},
		Commit: config.Commit{FixMessage: config.DefaultFixMessageTemplate},
	}
	executor := pipeline.NewExecutorWithAgentRoles(database, root, cfg, pipeline.AgentRoles{
		Implementer: agent.NewNoop(),
		Reviewer:    agent.WithSteering(reviewer),
	}, []pipeline.Step{&ReviewStep{}}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- executor.Execute(ctx, run, repo, workDir) }()

	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("execute real Codex review: %v", err)
			}
			logData, readErr := os.ReadFile(filepath.Join(root.RunLogDir(run.ID), "review.log"))
			if readErr != nil {
				t.Fatalf("read review log: %v", readErr)
			}
			if !strings.Contains(string(logData), "agent selection: role=reviewer primary=codex") {
				t.Fatalf("reviewer identity missing from log:\n%s", logData)
			}
			return
		case <-time.After(100 * time.Millisecond):
			steps, listErr := database.GetStepsByRun(run.ID)
			if listErr != nil || len(steps) == 0 {
				continue
			}
			if steps[0].Status == types.StepStatusAwaitingApproval {
				if respondErr := executor.Respond(types.StepReview, types.ActionApprove, nil); respondErr != nil {
					t.Fatalf("approve real review findings: %v", respondErr)
				}
			}
		case <-ctx.Done():
			t.Fatal("real Codex reviewer proof timed out")
		}
	}
}
