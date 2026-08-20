package cli

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCapabilitiesJSONIsStateFreeAndExact(t *testing.T) {
	oldVersion, oldCommit, oldDate := buildinfo.Version, buildinfo.Commit, buildinfo.Date
	buildinfo.Version, buildinfo.Commit, buildinfo.Date = "v1.50.0-test", "abcdef123456", "2026-08-20T00:00:00Z"
	t.Cleanup(func() {
		buildinfo.Version, buildinfo.Commit, buildinfo.Date = oldVersion, oldCommit, oldDate
	})
	nmHome := t.TempDir() + "/missing/and/must/not/be-created"
	t.Setenv("NM_HOME", nmHome)

	output, err := executeCmd("capabilities", "--json")
	if err != nil {
		t.Fatalf("capabilities: %v\n%s", err, output)
	}
	var got struct {
		Schema string `json:"schema"`
		Build  struct {
			Version string `json:"version"`
			Commit  string `json:"commit"`
			Date    string `json:"date"`
		} `json:"build"`
		Capabilities map[string]int `json:"capabilities"`
	}
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("decode capabilities JSON: %v\n%s", err, output)
	}
	if got.Schema != "no-mistakes-capabilities/v1" || got.Build.Version != "v1.50.0-test" || got.Build.Commit != "abcdef123456" || got.Build.Date != "2026-08-20T00:00:00Z" {
		t.Fatalf("capability identity = %+v", got)
	}
	for _, key := range []string{"structured_agent_roles", "independent_reviewer_implementer", "effective_role_resolution", "role_attempt_evidence"} {
		if got.Capabilities[key] != 1 {
			t.Fatalf("capability %q = %d, want 1", key, got.Capabilities[key])
		}
	}
	if strings.Contains(output, "NM_HOME") {
		t.Fatalf("capabilities leaked ambient state: %s", output)
	}
	if _, err := os.Stat(nmHome); !os.IsNotExist(err) {
		t.Fatalf("capabilities touched app state: %v", err)
	}
}

func TestAxiAttestCommandReadsPersistedEvidenceWithoutDaemon(t *testing.T) {
	_, _, database, repo := setupAxiQueryRepo(t)
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	resolution := config.AgentRoleResolution{Schema: config.AgentRoleResolutionSchema, Reviewer: config.RoleResolution{Candidates: []config.AgentCandidateResolution{}}}
	snapshot, err := json.Marshal(resolution)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(snapshot)
	if err := database.SetRunAgentRoleSnapshot(run.ID, string(snapshot), fmt.Sprintf("%x", sum[:])); err != nil {
		t.Fatal(err)
	}
	output, err := executeCmd("axi", "attest", "--run", run.ID, "--json")
	if err != nil {
		t.Fatalf("axi attest: %v\n%s", err, output)
	}
	if !strings.Contains(output, `"schema":"no-mistakes-run-agent-evidence/v1"`) || !strings.Contains(output, `"capture_status":"captured"`) || !strings.Contains(output, `"attempts":[]`) {
		t.Fatalf("attest output = %s", output)
	}
	if output, err := executeCmd("axi", "attest", "--json"); err == nil || !strings.Contains(output, `error: "--run is required"`) {
		t.Fatalf("missing run error = %v, output %s", err, output)
	}
}

func TestRolesResolveCommandUsesTrustedPolicyWithoutAgentProbe(t *testing.T) {
	repoDir := t.TempDir()
	origin := filepath.Join(t.TempDir(), "origin.git")
	run(t, "", "git", "init", "--bare", origin)
	run(t, repoDir, "git", "init", "--initial-branch=main")
	run(t, repoDir, "git", "config", "user.email", "test@example.com")
	run(t, repoDir, "git", "config", "user.name", "Test")
	run(t, repoDir, "git", "remote", "add", "origin", origin)
	if err := os.WriteFile(filepath.Join(repoDir, ".no-mistakes.yaml"), []byte("agent: codex\nagent_roles:\n  reviewer:\n    harness: codex\n    provider: openai\n    model: reviewer-model\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, repoDir, "git", "add", ".")
	run(t, repoDir, "git", "commit", "-m", "config")
	run(t, repoDir, "git", "push", "-u", "origin", "main")

	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: codex\nagent_path_override:\n  codex: /definitely/not/an/agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(repoDir)
	if err != nil {
		root = repoDir
	}
	if _, err := database.InsertRepoWithID("repo-roles", root, origin, "main"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	output, err := executeCmd("roles", "resolve", "--repo", repoDir, "--json")
	if err != nil {
		t.Fatalf("roles resolve: %v\n%s", err, output)
	}
	if !strings.Contains(output, `"schema":"no-mistakes-effective-agent-roles/v1"`) || !strings.Contains(output, `"model":"reviewer-model"`) || !strings.Contains(output, `"source":"repository.agent_roles"`) {
		t.Fatalf("roles resolve output = %s", output)
	}
	if strings.Contains(output, "/definitely/not/an/agent") || strings.Contains(output, `"status"`) {
		t.Fatalf("roles resolve exposed host resolution/internal path: %s", output)
	}
}

func TestRunEvidenceSeparatesDeclaredFromObservedAndRedactsUnavailableCandidates(t *testing.T) {
	resolution := config.AgentRoleResolution{
		Schema: config.AgentRoleResolutionSchema,
		Reviewer: config.RoleResolution{Source: config.AgentRoleSourceGlobalRole, Candidates: []config.AgentCandidateResolution{
			{Index: 0, Harness: types.AgentPi, Provider: "openai", Model: "declared-unavailable", Label: "candidate[harness=pi;args_sha256=abc]", Status: config.AgentCandidateUnavailable, Reason: config.AgentCandidateReasonExecutableNotFound},
			{Index: 1, Harness: types.AgentCodex, Provider: "openai", Model: "declared-model", Label: "candidate[harness=codex;provider=openai;model=declared-model]", Status: config.AgentCandidateAvailable},
		}},
		Implementer: config.RoleResolution{Source: config.AgentRoleSourceGlobalDefault, Candidates: []config.AgentCandidateResolution{}},
	}
	snapshot, err := json.Marshal(resolution)
	if err != nil {
		t.Fatal(err)
	}
	version, commit := "v1.50.0", "abc123"
	sum := sha256.Sum256(snapshot)
	hash := fmt.Sprintf("%x", sum[:])
	role, harness, provider, model := "reviewer", "codex", "openai", "declared-model"
	unsafeObserved := "https://user:credential@example.com/model"
	unsafeDeclared := "models/../../Users/alice/key"
	index := 1
	run := &db.Run{ID: "run-1", NoMistakesVersion: &version, NoMistakesBuildSHA: &commit, AgentRoleSnapshot: strPtrPublic(string(snapshot)), AgentRoleSnapshotHash: &hash}
	invocations := []db.AgentInvocation{{
		ID: "inv-1", RunID: run.ID, StepName: "review", Round: 1, Purpose: "review",
		Agent: "https://user:agent-secret@example.com", Role: &role, CandidateIndex: &index,
		DeclaredHarness: &harness, DeclaredProvider: &provider, DeclaredModel: &model,
		ObservedProvider: &unsafeDeclared, ObservedModel: &unsafeObserved,
		SessionMode: db.InvocationModeStarted, SessionKey: "must-not-be-public", ExitStatus: "ok",
		StartedAt: 10, CompletedAt: 20, DurationMS: 10,
	}}
	evidence, err := makeRunEvidence(run, invocations)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{"must-not-be-public", "candidate-secret", "agent-secret", "credential@example.com", "../../Users", "session_id", "args\"", "label"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("run evidence exposed %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, `"availability":"unavailable","outcome":"skipped","reason":"executable_not_found"`) {
		t.Fatalf("unavailable candidate evidence missing: %s", text)
	}
	if !strings.Contains(text, `"outcome":"selected"`) || !strings.Contains(text, `"declared_model":"declared-model"`) {
		t.Fatalf("selected/declared evidence missing: %s", text)
	}
	if strings.Contains(text, `"observed_model"`) {
		t.Fatalf("declared model was fabricated as observed: %s", text)
	}
}

func TestRunEvidenceMarksLegacySnapshotUnavailableAndUnknownOutcomesHonestly(t *testing.T) {
	evidence, err := makeRunEvidence(&db.Run{ID: "legacy"}, []db.AgentInvocation{{ID: "old", ExitStatus: "future-status"}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if !strings.Contains(text, `"capture_status":"unavailable_legacy"`) || !strings.Contains(text, `"outcome":"unknown"`) {
		t.Fatalf("legacy/unknown evidence = %s", text)
	}
	if strings.Contains(text, config.AgentRoleResolutionSchema) {
		t.Fatalf("legacy run fabricated current snapshot schema: %s", text)
	}
}

func TestRunEvidenceRejectsTamperedRoleSnapshot(t *testing.T) {
	snapshot, hash := `{"schema":"no-mistakes-agent-role-resolution/v1"}`, "wrong"
	if _, err := makeRunEvidence(&db.Run{ID: "run-1", AgentRoleSnapshot: &snapshot, AgentRoleSnapshotHash: &hash}, nil); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("tampered snapshot error = %v", err)
	}
	if _, err := makeRunEvidence(&db.Run{ID: "run-2", AgentRoleSnapshotHash: &hash}, nil); err == nil || !strings.Contains(err.Error(), "no snapshot") {
		t.Fatalf("orphaned hash error = %v", err)
	}
}

func strPtrPublic(value string) *string { return &value }
