package cli

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/spf13/cobra"
)

const (
	capabilitiesSchema   = "no-mistakes-capabilities/v1"
	effectiveRolesSchema = "no-mistakes-effective-agent-roles/v1"
	runEvidenceSchema    = "no-mistakes-run-agent-evidence/v1"
)

type publicBuild struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date,omitempty"`
}

func currentPublicBuild() publicBuild {
	return publicBuild{Version: buildinfo.CurrentVersion(), Commit: buildinfo.Commit, Date: buildinfo.Date}
}

func writeJSON(cmd *cobra.Command, value any) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func newCapabilitiesCmd() *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "capabilities",
		Short: "Print immutable binary capabilities as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !jsonOutput {
				return fmt.Errorf("capabilities is a machine-readable surface; pass --json")
			}
			return writeJSON(cmd, struct {
				Schema       string         `json:"schema"`
				Build        publicBuild    `json:"build"`
				Capabilities map[string]int `json:"capabilities"`
			}{
				Schema: capabilitiesSchema,
				Build:  currentPublicBuild(),
				Capabilities: map[string]int{
					"structured_agent_roles":            1,
					"independent_reviewer_implementer":  1,
					"ordered_candidate_fallback":        1,
					"effective_role_resolution":         1,
					"role_attempt_evidence":             1,
					"unavailable_candidate_persistence": 1,
				},
			})
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit versioned JSON")
	return cmd
}

type publicConfiguredCandidate struct {
	Index      int    `json:"index"`
	Harness    string `json:"harness"`
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	ArgsSHA256 string `json:"args_sha256,omitempty"`
}

type publicRolePolicy struct {
	Source     string                      `json:"source"`
	Candidates []publicConfiguredCandidate `json:"candidates"`
}

func makePublicRolePolicy(role config.RoleResolution) publicRolePolicy {
	out := publicRolePolicy{Source: role.Source, Candidates: make([]publicConfiguredCandidate, 0, len(role.Candidates))}
	for _, candidate := range role.Candidates {
		out.Candidates = append(out.Candidates, publicConfiguredCandidate{
			Index: candidate.Index, Harness: safePublicHarness(string(candidate.Harness)),
			Provider: safePublicValue(candidate.Provider), Model: safePublicValue(candidate.Model),
			ArgsSHA256: candidate.ArgsSHA256,
		})
	}
	return out
}

func newRolesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "roles", Short: "Inspect effective agent role policy", Args: cobra.NoArgs}
	cmd.AddCommand(newRolesResolveCmd())
	return cmd
}

func newRolesResolveCmd() *cobra.Command {
	var repoPath string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "resolve",
		Short: "Resolve trusted/global role precedence without launching agents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !jsonOutput {
				return fmt.Errorf("roles resolve is a machine-readable surface; pass --json")
			}
			p, err := paths.New()
			if err != nil {
				return fmt.Errorf("resolve paths: %w", err)
			}
			database, err := db.OpenReadOnly(p.DB())
			if err != nil {
				return fmt.Errorf("open state read-only: %w", err)
			}
			defer database.Close()

			root, err := git.FindGitRoot(repoPath)
			if err != nil {
				return fmt.Errorf("resolve repository %q: %w", repoPath, err)
			}
			root, err = filepath.Abs(root)
			if err != nil {
				return fmt.Errorf("resolve repository path: %w", err)
			}
			repo, err := database.GetRepoByPath(root)
			if err != nil {
				return fmt.Errorf("get repository: %w", err)
			}
			if repo == nil {
				if mainRoot, mainErr := git.FindMainRepoRoot(root); mainErr == nil {
					repo, err = database.GetRepoByPath(mainRoot)
					if err != nil {
						return fmt.Errorf("get main repository: %w", err)
					}
				}
			}
			if repo == nil {
				return fmt.Errorf("repo not initialized (run 'no-mistakes init' first)")
			}
			resolved, err := daemon.ResolveEffectiveAgentRoles(cmd.Context(), p, repo, root)
			if err != nil {
				return err
			}
			return writeJSON(cmd, struct {
				Schema            string      `json:"schema"`
				Build             publicBuild `json:"build"`
				RepoID            string      `json:"repo_id"`
				TargetSHA         string      `json:"target_sha"`
				TrustedConfigSHA  string      `json:"trusted_config_sha"`
				AllowRepoCommands bool        `json:"allow_repo_commands"`
				Roles             struct {
					Reviewer    publicRolePolicy `json:"reviewer"`
					Implementer publicRolePolicy `json:"implementer"`
				} `json:"roles"`
			}{
				Schema: effectiveRolesSchema, Build: currentPublicBuild(), RepoID: repo.ID,
				TargetSHA: resolved.TargetSHA, TrustedConfigSHA: resolved.TrustedConfigSHA,
				AllowRepoCommands: resolved.AllowRepoCommands,
				Roles: struct {
					Reviewer    publicRolePolicy `json:"reviewer"`
					Implementer publicRolePolicy `json:"implementer"`
				}{makePublicRolePolicy(resolved.Roles.Reviewer), makePublicRolePolicy(resolved.Roles.Implementer)},
			})
		},
	}
	cmd.Flags().StringVar(&repoPath, "repo", ".", "registered repository or worktree path")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit versioned JSON")
	return cmd
}

type publicCandidateEvidence struct {
	Index        int    `json:"index"`
	Harness      string `json:"harness"`
	Provider     string `json:"provider,omitempty"`
	Model        string `json:"model,omitempty"`
	ArgsSHA256   string `json:"args_sha256,omitempty"`
	Availability string `json:"availability"`
	Outcome      string `json:"outcome"`
	Reason       string `json:"reason,omitempty"`
}

type publicResolvedRoleEvidence struct {
	Source     string                    `json:"source"`
	Candidates []publicCandidateEvidence `json:"candidates"`
}

type publicAttemptEvidence struct {
	ID               string  `json:"id"`
	Step             string  `json:"step"`
	Round            int     `json:"round"`
	Purpose          string  `json:"purpose"`
	Role             *string `json:"role,omitempty"`
	CandidateIndex   *int    `json:"candidate_index,omitempty"`
	Candidate        string  `json:"candidate,omitempty"`
	Harness          *string `json:"declared_harness,omitempty"`
	DeclaredProvider *string `json:"declared_provider,omitempty"`
	DeclaredModel    *string `json:"declared_model,omitempty"`
	ObservedProvider *string `json:"observed_provider,omitempty"`
	ObservedModel    *string `json:"observed_model,omitempty"`
	Outcome          string  `json:"outcome"`
	Reason           string  `json:"reason,omitempty"`
	FallbackReason   *string `json:"fallback_reason,omitempty"`
	SessionMode      string  `json:"session_mode"`
	SessionOwner     string  `json:"session_owner,omitempty"`
	StartedAt        int64   `json:"started_at"`
	CompletedAt      int64   `json:"completed_at"`
	DurationMS       int64   `json:"duration_ms"`
}

func newAxiAttestCmd() *cobra.Command {
	var runID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "attest",
		Short: "Print redacted role and attempt evidence for a run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(runID) == "" {
				return emitError(cmd, 2, "--run is required", "Run `no-mistakes axi attest --run <run-id> --json`")
			}
			if !jsonOutput {
				return emitError(cmd, 2, "--json is required", "Run `no-mistakes axi attest --run <run-id> --json`")
			}
			p, err := paths.New()
			if err != nil {
				return emitError(cmd, 1, "agent evidence is unavailable")
			}
			database, err := db.OpenReadOnly(p.DB())
			if err != nil {
				return emitError(cmd, 1, "agent evidence is unavailable")
			}
			defer database.Close()
			run, err := database.GetRunForAgentEvidence(runID)
			if err != nil {
				return emitError(cmd, 1, "agent evidence is unavailable")
			}
			if run == nil {
				return emitError(cmd, 1, "run not found")
			}
			invocations, err := database.GetAgentInvocationsByRunForEvidence(runID)
			if err != nil {
				return emitError(cmd, 1, "agent evidence is unavailable")
			}
			evidence, err := makeRunEvidence(run, invocations)
			if err != nil {
				return emitError(cmd, 1, "persisted agent evidence is invalid")
			}
			if err := writeJSON(cmd, evidence); err != nil {
				return emitError(cmd, 1, "agent evidence could not be encoded")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&runID, "run", "", "run ID")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit versioned JSON")
	return cmd
}

func makeRunEvidence(run *db.Run, invocations []db.AgentInvocation) (any, error) {
	resolution := config.AgentRoleResolution{}
	captureStatus := "unavailable_legacy"
	if run.AgentRoleSnapshot == nil && run.AgentRoleSnapshotHash != nil {
		return nil, fmt.Errorf("persisted agent role snapshot hash has no snapshot")
	}
	if run.AgentRoleSnapshot != nil {
		if err := json.Unmarshal([]byte(*run.AgentRoleSnapshot), &resolution); err != nil {
			return nil, fmt.Errorf("decode persisted agent role snapshot: %w", err)
		}
		if run.AgentRoleSnapshotHash == nil {
			return nil, fmt.Errorf("persisted agent role snapshot has no hash")
		}
		sum := sha256.Sum256([]byte(*run.AgentRoleSnapshot))
		if got := fmt.Sprintf("%x", sum[:]); got != *run.AgentRoleSnapshotHash {
			return nil, fmt.Errorf("persisted agent role snapshot hash mismatch")
		}
		captureStatus = "captured"
	}
	attempts := make([]publicAttemptEvidence, 0, len(invocations))
	attempted := make(map[string]bool)
	succeeded := make(map[string]bool)
	for _, invocation := range invocations {
		outcome := "unknown"
		switch invocation.ExitStatus {
		case "ok":
			outcome = "succeeded"
		case "error":
			outcome = "failed"
		case "cancelled":
			outcome = "cancelled"
		}
		reason := safePublicReason(invocation.FailureCategory)
		role := safeRolePointer(invocation.Role)
		candidateID := publicAttemptCandidateID(role, invocation.CandidateIndex, invocation.Agent)
		owner := ""
		if invocation.SessionMode != db.InvocationModeCold {
			owner = candidateID
		}
		attempts = append(attempts, publicAttemptEvidence{
			ID: invocation.ID, Step: invocation.StepName, Round: invocation.Round, Purpose: invocation.Purpose,
			Role: role, CandidateIndex: invocation.CandidateIndex, Candidate: candidateID,
			Harness: safeHarnessPointer(invocation.DeclaredHarness), DeclaredProvider: safeObservedPointer(invocation.DeclaredProvider), DeclaredModel: safeObservedPointer(invocation.DeclaredModel),
			ObservedProvider: safeObservedPointer(invocation.ObservedProvider), ObservedModel: safeObservedPointer(invocation.ObservedModel),
			Outcome: outcome, Reason: reason, FallbackReason: safePublicReasonPointer(invocation.FallbackReason),
			SessionMode: invocation.SessionMode, SessionOwner: owner,
			StartedAt: invocation.StartedAt, CompletedAt: invocation.CompletedAt, DurationMS: invocation.DurationMS,
		})
		if invocation.CandidateIndex != nil && role != nil {
			key := fmt.Sprintf("%s:%d", *role, *invocation.CandidateIndex)
			attempted[key] = true
			if outcome == "succeeded" {
				succeeded[key] = true
			}
		}
	}
	candidates := func(roleName string, role config.RoleResolution) []publicCandidateEvidence {
		out := make([]publicCandidateEvidence, 0, len(role.Candidates))
		for _, candidate := range role.Candidates {
			key := fmt.Sprintf("%s:%d", roleName, candidate.Index)
			outcome, reason := "skipped", "not_selected"
			if candidate.Status != config.AgentCandidateAvailable {
				reason = candidate.Reason
			} else if succeeded[key] {
				outcome, reason = "selected", ""
			} else if attempted[key] {
				outcome, reason = "attempted", ""
			}
			out = append(out, publicCandidateEvidence{
				Index: candidate.Index, Harness: safePublicHarness(string(candidate.Harness)), Provider: safePublicValue(candidate.Provider),
				Model: safePublicValue(candidate.Model), ArgsSHA256: candidate.ArgsSHA256,
				Availability: candidate.Status,
				Outcome:      outcome, Reason: reason,
			})
		}
		return out
	}
	version, commit := "", ""
	if run.NoMistakesVersion != nil {
		version = *run.NoMistakesVersion
	}
	if run.NoMistakesBuildSHA != nil {
		commit = *run.NoMistakesBuildSHA
	}
	return struct {
		Schema     string      `json:"schema"`
		RunID      string      `json:"run_id"`
		Build      publicBuild `json:"build"`
		RoleConfig struct {
			CaptureStatus string `json:"capture_status"`
			Hash          string `json:"hash,omitempty"`
			Schema        string `json:"schema,omitempty"`
			Roles         struct {
				Reviewer    publicResolvedRoleEvidence `json:"reviewer"`
				Implementer publicResolvedRoleEvidence `json:"implementer"`
			} `json:"roles"`
		} `json:"role_config"`
		Attempts []publicAttemptEvidence `json:"attempts"`
	}{
		Schema: runEvidenceSchema, RunID: run.ID, Build: publicBuild{Version: version, Commit: commit},
		RoleConfig: struct {
			CaptureStatus string `json:"capture_status"`
			Hash          string `json:"hash,omitempty"`
			Schema        string `json:"schema,omitempty"`
			Roles         struct {
				Reviewer    publicResolvedRoleEvidence `json:"reviewer"`
				Implementer publicResolvedRoleEvidence `json:"implementer"`
			} `json:"roles"`
		}{
			CaptureStatus: captureStatus, Hash: valueOrEmpty(run.AgentRoleSnapshotHash), Schema: resolution.Schema,
			Roles: struct {
				Reviewer    publicResolvedRoleEvidence `json:"reviewer"`
				Implementer publicResolvedRoleEvidence `json:"implementer"`
			}{
				Reviewer:    publicResolvedRoleEvidence{Source: resolution.Reviewer.Source, Candidates: candidates("reviewer", resolution.Reviewer)},
				Implementer: publicResolvedRoleEvidence{Source: resolution.Implementer.Source, Candidates: candidates("implementer", resolution.Implementer)},
			},
		},
		Attempts: attempts,
	}, nil
}

func safePublicHarness(value string) string {
	if strings.HasPrefix(value, "acp:") {
		if target := safePublicValue(strings.TrimPrefix(value, "acp:")); target != "" {
			return "acp:" + target
		}
		return ""
	}
	return safePublicValue(value)
}

func safePublicValue(value string) string {
	safe, ok := agent.SafeObservedIdentity(value)
	if !ok {
		return ""
	}
	return safe
}

func safeHarnessPointer(value *string) *string {
	if value == nil {
		return nil
	}
	safe := safePublicHarness(*value)
	if safe == "" {
		return nil
	}
	return &safe
}

func safeObservedPointer(value *string) *string {
	if value == nil {
		return nil
	}
	safe, ok := agent.SafeObservedIdentity(*value)
	if !ok {
		return nil
	}
	return &safe
}

func safeRolePointer(value *string) *string {
	if value == nil || (*value != "reviewer" && *value != "implementer") {
		return nil
	}
	return value
}

func publicAttemptCandidateID(role *string, index *int, legacy string) string {
	if role != nil && index != nil {
		return fmt.Sprintf("%s:%d", *role, *index)
	}
	return safePublicValue(legacy)
}

func safePublicReason(value string) string {
	if value == "" || len(value) > 64 {
		return ""
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			continue
		}
		return ""
	}
	return value
}

func safePublicReasonPointer(value *string) *string {
	if value == nil {
		return nil
	}
	safe := safePublicReason(*value)
	if safe == "" {
		return nil
	}
	return &safe
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
