package config

import (
	"context"
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
	if got := Merge(global, repoDefault).AgentRoles.Reviewer; len(got) != 1 || got[0].Harness != types.AgentCodex {
		t.Fatalf("repository legacy agent did not override global role: %+v", got)
	}
	repoRole := &RepoConfig{
		Agent:  types.AgentCodex,
		Agents: []types.AgentName{types.AgentCodex},
		AgentRoles: AgentRoles{Reviewer: RoleSelection{{
			Harness: types.AgentPi, Provider: "repo", Model: "repo-model", Args: []string{"--model", "repo-model"},
		}}},
	}
	got := Merge(global, repoRole).AgentRoles.Reviewer
	if len(got) != 1 || got[0].Provider != "repo" || got[0].Model != "repo-model" || !reflect.DeepEqual(got[0].Args, []string{"--model", "repo-model"}) {
		t.Fatalf("repository role did not preserve complete candidate: %+v", got)
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
