package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestResolveEffectiveAgentRolesUsesPinnedTrustedPrecedenceWithoutAvailabilityResolution(t *testing.T) {
	ctx := context.Background()
	source := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, source, "init", "--initial-branch=main")
	gitCmd(t, source, "config", "user.email", "test@example.com")
	gitCmd(t, source, "config", "user.name", "Test")
	gitCmd(t, source, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(source, ".no-mistakes.yaml"), []byte("agent: codex\nagent_roles:\n  reviewer:\n    harness: codex\n    provider: trusted-provider\n    model: trusted-reviewer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, source, "add", ".")
	gitCmd(t, source, "commit", "-m", "trusted config")

	bare := filepath.Join(t.TempDir(), "origin.git")
	gitCmd(t, "", "init", "--bare", bare)
	gitCmd(t, source, "remote", "add", "origin", bare)
	gitCmd(t, source, "push", "-u", "origin", "main")
	gitCmd(t, source, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(source, ".no-mistakes.yaml"), []byte("agent: claude\nagent_roles:\n  reviewer: pi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, source, "add", ".")
	gitCmd(t, source, "commit", "-m", "untrusted feature config")
	featureSHA := gitOutput(t, source, "rev-parse", "HEAD")

	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := ResolveEffectiveAgentRoles(ctx, p, &db.Repo{ID: "repo", WorkingPath: source, DefaultBranch: "main"}, source)
	if err != nil {
		t.Fatalf("ResolveEffectiveAgentRoles: %v", err)
	}
	if result.TargetSHA != featureSHA || result.TrustedConfigSHA == "" || result.AllowRepoCommands {
		t.Fatalf("resolution provenance = %+v", result)
	}
	got := result.Roles.Reviewer
	if got.Source != config.AgentRoleSourceRepositoryRole || len(got.Candidates) != 1 || got.Candidates[0].Harness != types.AgentCodex || got.Candidates[0].Provider != "trusted-provider" || got.Candidates[0].Model != "trusted-reviewer" {
		t.Fatalf("effective reviewer = %+v", got)
	}
	if got.Candidates[0].Status != config.AgentCandidateConfigured {
		t.Fatalf("effective query performed host availability resolution: %+v", got.Candidates[0])
	}
}

func TestValidateRecoveredAgentRoleResolutionRejectsChangedCandidateBinding(t *testing.T) {
	resolution := config.AgentRoleResolution{
		Schema:   config.AgentRoleResolutionSchema,
		Reviewer: config.RoleResolution{Candidates: []config.AgentCandidateResolution{{Index: 0, Harness: types.AgentCodex, Label: "codex", Status: config.AgentCandidateAvailable}}},
	}
	encoded, err := json.Marshal(resolution)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(encoded)
	snapshot, hash := string(encoded), fmt.Sprintf("%x", sum[:])
	run := &db.Run{AgentRoleSnapshot: &snapshot, AgentRoleSnapshotHash: &hash}
	cfg := &config.Config{AgentRoleResolution: &resolution}
	if err := validateRecoveredAgentRoleResolution(run, cfg); err != nil {
		t.Fatalf("matching recovery evidence: %v", err)
	}
	cfg.AgentRoleResolution.Reviewer.Candidates[0].Harness = types.AgentPi
	if err := validateRecoveredAgentRoleResolution(run, cfg); err == nil {
		t.Fatal("changed recovery candidate binding was accepted")
	}
}

func TestDemoAgentConstructionPreservesConfiguredSnapshotForRecovery(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	cfg := &config.Config{
		AgentRoles: config.AgentRoles{
			Reviewer:    config.RoleSelection{{Harness: types.AgentCodex}},
			Implementer: config.RoleSelection{{Harness: types.AgentCodex}},
		},
	}
	policy := cfg.EffectiveAgentRolePolicy()
	encoded, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(encoded)
	snapshot, hash := string(encoded), fmt.Sprintf("%x", sum[:])
	set, err := newPipelineAgents(context.Background(), cfg, func(string) (string, error) {
		t.Fatal("demo mode probed an executable")
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	if err := validateRecoveredAgentRoleResolution(&db.Run{AgentRoleSnapshot: &snapshot, AgentRoleSnapshotHash: &hash}, cfg); err != nil {
		t.Fatalf("demo recovery evidence mismatch: %v", err)
	}
}

func TestLoadRepoConfigAtSHAIgnoresDirtyWorkingTree(t *testing.T) {
	repo := t.TempDir()
	gitCmd(t, repo, "init")
	gitCmd(t, repo, "config", "user.email", "test@example.com")
	gitCmd(t, repo, "config", "user.name", "Test")
	gitCmd(t, repo, "config", "commit.gpgsign", "false")
	path := filepath.Join(repo, ".no-mistakes.yaml")
	if err := os.WriteFile(path, []byte("agent: codex\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo, "add", ".")
	gitCmd(t, repo, "commit", "-m", "config")
	sha := gitOutput(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(path, []byte("agent: pi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadRepoConfigAtSHA(context.Background(), repo, sha)
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != types.AgentCodex {
		t.Fatalf("pinned config agent = %q, want codex", got.Agent)
	}
}
