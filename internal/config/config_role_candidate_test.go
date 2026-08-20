package config

import (
	"context"
	"encoding/json"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLoadGlobal_AgentRolesAcceptStructuredCandidatesAlongsideStrings(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte(`agent: claude
agent_roles:
  reviewer:
    - harness: pi
      provider: openai
      model: gpt-5.4
      args: [--provider, openai, --model, gpt-5.4, --thinking, high]
    - harness: pi
      provider: anthropic
      model: claude-opus-4-6
      args: [--provider, anthropic, --model, claude-opus-4-6]
  implementer: codex
`))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	if len(cfg.AgentRoles.Reviewer) != 2 {
		t.Fatalf("reviewer candidates = %+v, want 2", cfg.AgentRoles.Reviewer)
	}
	first := cfg.AgentRoles.Reviewer[0]
	if first.Harness != types.AgentPi || first.Provider != "openai" || first.Model != "gpt-5.4" {
		t.Fatalf("first reviewer = %+v", first)
	}
	if !reflect.DeepEqual(first.Args, []string{"--provider", "openai", "--model", "gpt-5.4", "--thinking", "high"}) {
		t.Fatalf("first args = %v", first.Args)
	}
	if got := cfg.AgentRoles.Implementer[0]; got.Harness != types.AgentCodex || got.Provider != "" || got.Model != "" || len(got.Args) != 0 {
		t.Fatalf("legacy implementer candidate = %+v", got)
	}
	if cfg.AgentRoles.Reviewer[0].Label() == cfg.AgentRoles.Reviewer[1].Label() {
		t.Fatalf("same-harness candidates collapsed to one identity: %+v", cfg.AgentRoles.Reviewer)
	}
}

func TestLoadGlobal_AgentRolesKeepEmptyLegacyRoleAsInheritance(t *testing.T) {
	cfg, err := LoadGlobalFromBytes([]byte("agent_roles:\n  reviewer: null\n  implementer: ''\n"))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	if len(cfg.AgentRoles.Reviewer) != 0 || len(cfg.AgentRoles.Implementer) != 0 {
		t.Fatalf("empty roles = %+v, want inheritance", cfg.AgentRoles)
	}
}

func TestLoadRepo_AgentRolesAcceptSingleStructuredCandidate(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte(`agent_roles:
  reviewer:
    harness: pi
    provider: google
    model: gemini-2.5-pro
    args: [--provider, google, --model, gemini-2.5-pro]
`))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes: %v", err)
	}
	if len(cfg.AgentRoles.Reviewer) != 1 || cfg.AgentRoles.Reviewer[0].Harness != types.AgentPi {
		t.Fatalf("reviewer candidates = %+v", cfg.AgentRoles.Reviewer)
	}
}

func TestLoadAgentRoles_RejectsInvalidStructuredCandidates(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{name: "missing harness", yaml: "agent_roles:\n  reviewer: {model: gpt-5.4}\n", want: "harness"},
		{name: "unknown field", yaml: "agent_roles:\n  reviewer: {harness: pi, command: pi}\n", want: "command"},
		{name: "duplicate field", yaml: "agent_roles:\n  reviewer: {harness: pi, harness: codex}\n", want: "duplicated"},
		{name: "empty arg", yaml: "agent_roles:\n  reviewer: {harness: pi, args: ['']}\n", want: "empty arg"},
		{name: "managed arg", yaml: "agent_roles:\n  reviewer: {harness: pi, args: [--mode, json]}\n", want: "managed by no-mistakes"},
		{name: "acp args silently ignored", yaml: "agent_roles:\n  reviewer: {harness: 'acp:gemini', args: [--model, gemini]}\n", want: "not supported"},
		{name: "unsafe identity", yaml: "agent_roles:\n  reviewer: {harness: pi, provider: 'openai\\nsecret'}\n", want: "provider"},
		{name: "credential URL provider", yaml: "agent_roles:\n  reviewer: {harness: pi, provider: 'https://user:secret@example.com'}\n", want: "provider"},
		{name: "path-like model", yaml: "agent_roles:\n  reviewer: {harness: pi, model: 'models/../../Users/alice/key'}\n", want: "model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadGlobalFromBytes([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want mention %q", err, tt.want)
			}
		})
	}
}

func TestResolveAgent_RetainsSameHarnessCandidatesWithDifferentModels(t *testing.T) {
	cfg := &Config{
		Agent:  types.AgentClaude,
		Agents: []types.AgentName{types.AgentClaude},
		AgentRoles: AgentRoles{Reviewer: RoleSelection{
			{Harness: types.AgentPi, Provider: "openai", Model: "gpt-5.4", Args: []string{"--model", "gpt-5.4"}},
			{Harness: types.AgentPi, Provider: "openai", Model: "gpt-5.3", Args: []string{"--model", "gpt-5.3"}},
		}},
	}
	if err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		if bin == "claude" || bin == "pi" {
			return "/usr/bin/" + bin, nil
		}
		return "", &exec.Error{Name: bin, Err: exec.ErrNotFound}
	}); err != nil {
		t.Fatalf("ResolveAgent: %v", err)
	}
	got := cfg.AgentRoles.Reviewer
	if len(got) != 2 || got[0].Model != "gpt-5.4" || got[1].Model != "gpt-5.3" {
		t.Fatalf("resolved reviewer candidates = %+v", got)
	}
}

func TestMerge_StructuredRoleCandidatePrecedencePreservesCompleteSeat(t *testing.T) {
	global := &GlobalConfig{
		Agent:  types.AgentClaude,
		Agents: []types.AgentName{types.AgentClaude},
		AgentRoles: AgentRoles{Reviewer: RoleSelection{{
			Harness: types.AgentPi, Provider: "global", Model: "global-model", Args: []string{"--model", "global-model"},
		}}},
	}
	repoDefault := &RepoConfig{Agent: types.AgentCodex, Agents: []types.AgentName{types.AgentCodex}}
	mergedDefault := Merge(global, repoDefault)
	if got := mergedDefault.AgentRoles.Reviewer; len(got) != 1 || got[0].Harness != types.AgentCodex {
		t.Fatalf("repository legacy agent did not override global role: %+v", got)
	}
	if mergedDefault.AgentRoleSources.Reviewer != AgentRoleSourceRepositoryDefault {
		t.Fatalf("repository default source = %q", mergedDefault.AgentRoleSources.Reviewer)
	}
	repoRole := &RepoConfig{
		Agent:  types.AgentCodex,
		Agents: []types.AgentName{types.AgentCodex},
		AgentRoles: AgentRoles{Reviewer: RoleSelection{{
			Harness: types.AgentPi, Provider: "repo", Model: "repo-model", Args: []string{"--model", "repo-model"},
		}}},
	}
	mergedRole := Merge(global, repoRole)
	got := mergedRole.AgentRoles.Reviewer
	if len(got) != 1 || got[0].Provider != "repo" || got[0].Model != "repo-model" || !reflect.DeepEqual(got[0].Args, []string{"--model", "repo-model"}) {
		t.Fatalf("repository role did not preserve complete candidate: %+v", got)
	}
	policy := mergedRole.EffectiveAgentRolePolicy()
	if policy.Reviewer.Source != AgentRoleSourceRepositoryRole || len(policy.Reviewer.Candidates) != 1 || policy.Reviewer.Candidates[0].Status != "configured" {
		t.Fatalf("effective reviewer policy = %+v", policy.Reviewer)
	}
	if strings.Contains(policy.Reviewer.Candidates[0].Label, "repo-model") && strings.Contains(policy.Reviewer.Candidates[0].Label, "--model") {
		t.Fatalf("effective policy label exposed raw args: %q", policy.Reviewer.Candidates[0].Label)
	}
}

func TestEffectiveRepoConfig_StructuredRoleCandidatesStayTrusted(t *testing.T) {
	pushed := &RepoConfig{AgentRoles: AgentRoles{Reviewer: RoleSelection{{Harness: types.AgentPi, Model: "untrusted", Args: []string{"--model", "untrusted"}}}}}
	trusted := &RepoConfig{AgentRoles: AgentRoles{Reviewer: RoleSelection{{Harness: types.AgentPi, Model: "trusted", Args: []string{"--model", "trusted"}}}}}
	got := EffectiveRepoConfig(pushed, trusted, false)
	if len(got.AgentRoles.Reviewer) != 1 || got.AgentRoles.Reviewer[0].Model != "trusted" {
		t.Fatalf("effective reviewer = %+v, want trusted structured candidate", got.AgentRoles.Reviewer)
	}
	got.AgentRoles.Reviewer[0].Args[1] = "mutated"
	if trusted.AgentRoles.Reviewer[0].Args[1] != "trusted" {
		t.Fatal("effective structured role aliases trusted configuration")
	}
}

func TestAgentCandidateLabelSeparatesProviderAndModelBoundaries(t *testing.T) {
	providerOnly := AgentCandidate{Harness: types.AgentPi, Provider: "openai/gpt-5.4"}
	providerAndModel := AgentCandidate{Harness: types.AgentPi, Provider: "openai", Model: "gpt-5.4"}

	if providerOnly.Label() == providerAndModel.Label() {
		t.Fatalf("candidate labels collide: %q", providerOnly.Label())
	}
	if got := (AgentCandidate{Harness: types.AgentPi}).Label(); got != "pi" {
		t.Fatalf("legacy candidate label = %q, want pi", got)
	}
}

func TestAgentCandidateLabelSeparatesHarnessAndStructuredFieldBoundaries(t *testing.T) {
	embeddedField := AgentCandidate{Harness: "acp:x[provider=openai]"}
	structuredField := AgentCandidate{Harness: "acp:x", Provider: "openai"}

	if embeddedField.Label() == structuredField.Label() {
		t.Fatalf("candidate labels collide: %q", embeddedField.Label())
	}
}

func TestAgentCandidateLabelDoesNotExposeArguments(t *testing.T) {
	candidate := AgentCandidate{
		Harness:  types.AgentPi,
		Provider: "openai",
		Model:    "gpt-5.4",
		Args:     []string{"--api-key", "top-secret-value"},
	}
	label := candidate.Label()
	if !strings.Contains(label, "pi") || !strings.Contains(label, "openai") || !strings.Contains(label, "gpt-5.4") {
		t.Fatalf("label %q loses public seat identity", label)
	}
	if strings.Contains(label, "top-secret-value") || strings.Contains(label, "--api-key") {
		t.Fatalf("label %q exposes candidate arguments", label)
	}
}

func TestAgentArgsForCandidate_AppendsCandidateArgsWithoutMutatingConfig(t *testing.T) {
	cfg := &Config{AgentArgsOverride: map[string][]string{"pi": {"--thinking", "low"}}}
	candidate := AgentCandidate{Harness: types.AgentPi, Args: []string{"--provider", "openai", "--model", "gpt-5.4"}}
	got := cfg.AgentArgsForCandidate(candidate)
	want := []string{"--thinking", "low", "--provider", "openai", "--model", "gpt-5.4"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	got[0] = "changed"
	if cfg.AgentArgsOverride["pi"][0] != "--thinking" || candidate.Args[0] != "--provider" {
		t.Fatal("AgentArgsForCandidate aliased its config inputs")
	}
}

func TestEffectiveAgentRolePolicyFingerprintsCompleteLaunchArguments(t *testing.T) {
	candidate := AgentCandidate{Harness: types.AgentCodex, Args: []string{"--sandbox", "workspace-write"}}
	first := &Config{AgentRoles: AgentRoles{Reviewer: RoleSelection{candidate}}, AgentArgsOverride: map[string][]string{"codex": {"--model", "first"}}}
	second := &Config{AgentRoles: AgentRoles{Reviewer: RoleSelection{candidate}}, AgentArgsOverride: map[string][]string{"codex": {"--model", "second"}}}
	firstHash := first.EffectiveAgentRolePolicy().Reviewer.Candidates[0].ArgsSHA256
	secondHash := second.EffectiveAgentRolePolicy().Reviewer.Candidates[0].ArgsSHA256
	if firstHash == "" || secondHash == "" || firstHash == secondHash {
		t.Fatalf("effective launch hashes = %q/%q", firstHash, secondHash)
	}
}

func TestResolveAgentWithReportRetainsUnavailableAndDuplicateCandidates(t *testing.T) {
	secret := "candidate-secret-must-not-leak"
	cfg := &Config{
		Agent:  types.AgentClaude,
		Agents: []types.AgentName{types.AgentClaude},
		AgentRoles: AgentRoles{
			Reviewer: RoleSelection{
				{Harness: types.AgentPi, Provider: "openai", Model: "gpt-5.4", Args: []string{"--token", secret}},
				{Harness: types.AgentCodex, Provider: "openai", Model: "gpt-5.4"},
				{Harness: types.AgentCodex, Provider: "openai", Model: "gpt-5.4"},
			},
			Implementer: RoleSelection{{Harness: types.AgentClaude, Provider: "anthropic", Model: "claude-opus-5"}},
		},
	}
	lookPath := func(path string) (string, error) {
		if path == "pi" {
			return "", exec.ErrNotFound
		}
		return "/bin/" + path, nil
	}

	report, err := cfg.ResolveAgentWithReport(context.Background(), lookPath)
	if err != nil {
		t.Fatalf("ResolveAgentWithReport: %v", err)
	}
	if got := cfg.AgentRoles.Reviewer; len(got) != 1 || got[0].Harness != types.AgentCodex {
		t.Fatalf("runnable reviewer selection = %+v, want one codex candidate", got)
	}
	got := report.Reviewer.Candidates
	if len(got) != 3 {
		t.Fatalf("reviewer report = %+v, want all 3 configured candidates", got)
	}
	if got[0].Index != 0 || got[0].Status != AgentCandidateUnavailable || got[0].Reason != AgentCandidateReasonExecutableNotFound {
		t.Fatalf("unavailable candidate = %+v", got[0])
	}
	if got[1].Index != 1 || got[1].Status != AgentCandidateAvailable {
		t.Fatalf("available candidate = %+v", got[1])
	}
	if got[2].Index != 2 || got[2].Status != AgentCandidateSkipped || got[2].Reason != AgentCandidateReasonDuplicate {
		t.Fatalf("duplicate candidate = %+v", got[2])
	}
	if strings.Contains(report.Reviewer.Candidates[0].Label, secret) {
		t.Fatalf("public resolution label exposes arguments: %q", report.Reviewer.Candidates[0].Label)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "--token") {
		t.Fatalf("resolution snapshot exposes raw arguments: %s", encoded)
	}
	if report.Reviewer.Candidates[0].ArgsSHA256 == "" {
		t.Fatal("resolution snapshot lost redacted argument identity")
	}
}
