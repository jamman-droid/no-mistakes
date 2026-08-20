package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	AgentRoleResolutionSchema = "no-mistakes-agent-role-resolution/v1"

	AgentCandidateConfigured  = "configured"
	AgentCandidateAvailable   = "available"
	AgentCandidateUnavailable = "unavailable"
	AgentCandidateSkipped     = "skipped"

	AgentCandidateReasonExecutableNotFound = "executable_not_found"
	AgentCandidateReasonDuplicate          = "duplicate"

	AgentRoleSourceRepositoryRole    = "repository.agent_roles"
	AgentRoleSourceRepositoryDefault = "repository.agent"
	AgentRoleSourceGlobalRole        = "global.agent_roles"
	AgentRoleSourceGlobalDefault     = "global.agent"
)

// AgentRoleSources identifies the precedence layer that supplied each role.
type AgentRoleSources struct {
	Reviewer    string `json:"reviewer"`
	Implementer string `json:"implementer"`
}

// AgentCandidateResolution is a redacted host-observed disposition for one
// configured candidate. Label fingerprints arguments but never contains them.
type AgentCandidateResolution struct {
	Index      int             `json:"index"`
	Harness    types.AgentName `json:"harness"`
	Provider   string          `json:"provider,omitempty"`
	Model      string          `json:"model,omitempty"`
	Label      string          `json:"label"`
	ArgsSHA256 string          `json:"args_sha256,omitempty"`
	Status     string          `json:"status"`
	Reason     string          `json:"reason,omitempty"`
	// RuntimeLabel binds adapter callbacks to this candidate in memory. It is
	// deliberately excluded from durable/public evidence because legacy or
	// externally supplied labels may contain unsafe identity text.
	RuntimeLabel string `json:"-"`
}

// AgentRoleResolution preserves every configured candidate in declared order.
type AgentRoleResolution struct {
	Schema      string         `json:"schema"`
	Reviewer    RoleResolution `json:"reviewer"`
	Implementer RoleResolution `json:"implementer"`
}

type RoleResolution struct {
	Source     string                     `json:"source"`
	Candidates []AgentCandidateResolution `json:"candidates"`
}

// EffectiveAgentRolePolicy returns the trusted/global precedence result without
// probing executables or constructing an agent.
func (c *Config) EffectiveAgentRolePolicy() AgentRoleResolution {
	return AgentRoleResolution{
		Schema: AgentRoleResolutionSchema,
		Reviewer: RoleResolution{
			Source:     c.AgentRoleSources.Reviewer,
			Candidates: c.configuredCandidateResolutions(c.AgentRoles.Reviewer),
		},
		Implementer: RoleResolution{
			Source:     c.AgentRoleSources.Implementer,
			Candidates: c.configuredCandidateResolutions(c.AgentRoles.Implementer),
		},
	}
}

func (c *Config) configuredCandidateResolutions(selection RoleSelection) []AgentCandidateResolution {
	out := make([]AgentCandidateResolution, 0, len(selection))
	for i, candidate := range selection {
		out = append(out, c.candidateResolution(i, candidate, AgentCandidateConfigured, ""))
	}
	return out
}

// ResolveAgentWithReport preserves ResolveAgent's compatibility behavior while
// also returning every configured candidate, including unavailable and exact
// duplicate candidates that are excluded from the runnable selection.
func (c *Config) ResolveAgentWithReport(ctx context.Context, lookPath func(string) (string, error)) (AgentRoleResolution, error) {
	configuredDefault := c.configuredAgents()
	configuredRoles := copyAgentRoles(c.AgentRoles)
	report := AgentRoleResolution{
		Schema:      AgentRoleResolutionSchema,
		Reviewer:    RoleResolution{Source: c.AgentRoleSources.Reviewer, Candidates: c.configuredCandidateResolutions(configuredRoles.Reviewer)},
		Implementer: RoleResolution{Source: c.AgentRoleSources.Implementer, Candidates: c.configuredCandidateResolutions(configuredRoles.Implementer)},
	}
	memo := agentProbeMemo{}
	if err := c.resolveDefaultAgent(ctx, lookPath); err != nil {
		// Preserve host-observed unavailability even when the inherited default
		// leaves the run with no runnable agent. Resolution still fails exactly
		// as before; only the redacted evidence becomes complete.
		for _, role := range []struct {
			selection RoleSelection
			report    *RoleResolution
		}{
			{selection: configuredRoles.Reviewer, report: &report.Reviewer},
			{selection: configuredRoles.Implementer, report: &report.Implementer},
		} {
			candidates := copyRoleSelection(role.selection)
			if len(candidates) == 0 || roleSelectionMatchesAgents(candidates, configuredDefault) {
				candidates = roleSelectionFromAgents(configuredDefault)
			}
			_, candidateReport, _ := c.resolveRoleSelectionWithReport(ctx, memo, candidates, lookPath)
			role.report.Candidates = candidateReport
		}
		c.AgentRoleResolution = &report
		return report, err
	}
	for _, name := range c.Agents {
		memo[name] = agentProbeResult{name: name, available: true}
	}

	for _, role := range []struct {
		name      string
		selection RoleSelection
		resolved  *RoleSelection
		report    *RoleResolution
	}{
		{name: "reviewer", selection: configuredRoles.Reviewer, resolved: &c.AgentRoles.Reviewer, report: &report.Reviewer},
		{name: "implementer", selection: configuredRoles.Implementer, resolved: &c.AgentRoles.Implementer, report: &report.Implementer},
	} {
		candidates := copyRoleSelection(role.selection)
		if len(candidates) == 0 {
			candidates = roleSelectionFromAgents(c.Agents)
		}
		if roleSelectionMatchesAgents(candidates, configuredDefault) && len(configuredDefault) == 1 && configuredDefault[0] == types.AgentAuto {
			candidates = roleSelectionFromAgents(c.Agents)
		}
		resolved, candidateReport, err := c.resolveRoleSelectionWithReport(ctx, memo, candidates, lookPath)
		role.report.Candidates = candidateReport
		if err != nil {
			c.AgentRoleResolution = &report
			return report, fmt.Errorf("resolve %s agent role: %w", role.name, err)
		}
		*role.resolved = resolved
	}
	c.AgentRoleResolution = &report
	return report, nil
}

// agentProbeMemo caches one resolution pass's executable probes so the default
// seat and both role seats never spawn the same probe subprocess twice. Seeding
// it with an already-resolved harness is sound because resolveDefaultAgent only
// returns names whose probe already succeeded.
type agentProbeMemo map[types.AgentName]agentProbeResult

type agentProbeResult struct {
	name      types.AgentName
	available bool
	probe     string
	err       error
}

func (c *Config) probeConfiguredAgent(ctx context.Context, memo agentProbeMemo, declared types.AgentName, lookPath func(string) (string, error)) (types.AgentName, bool, string, error) {
	if cached, ok := memo[declared]; ok {
		return cached.name, cached.available, cached.probe, cached.err
	}
	name, available, probe, err := c.resolveConfiguredAgent(ctx, declared, lookPath)
	if memo != nil {
		memo[declared] = agentProbeResult{name: name, available: available, probe: probe, err: err}
	}
	return name, available, probe, err
}

func (c *Config) resolveRoleSelectionWithReport(ctx context.Context, memo agentProbeMemo, candidates RoleSelection, lookPath func(string) (string, error)) (RoleSelection, []AgentCandidateResolution, error) {
	resolved := make(RoleSelection, 0, len(candidates))
	report := make([]AgentCandidateResolution, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	probed := make([]string, 0, len(candidates))
	configured := make([]types.AgentName, 0, len(candidates))
	for index, declared := range candidates {
		configured = append(configured, declared.Harness)
		candidate := declared
		name, ok, probe, err := c.probeConfiguredAgent(ctx, memo, candidate.Harness, lookPath)
		if probe != "" {
			probed = append(probed, probe)
		}
		if err != nil {
			return nil, report, err
		}
		if !ok {
			report = append(report, c.candidateResolution(index, candidate, AgentCandidateUnavailable, AgentCandidateReasonExecutableNotFound))
			continue
		}
		candidate.Harness = name
		identity := resolvedAgentIdentity(name) + "\x00" + candidate.Provider + "\x00" + candidate.Model + "\x00" + candidateArgsFingerprint(candidate.Args)
		if seen[identity] {
			report = append(report, c.candidateResolution(index, candidate, AgentCandidateSkipped, AgentCandidateReasonDuplicate))
			continue
		}
		seen[identity] = true
		resolved = append(resolved, candidate)
		report = append(report, c.candidateResolution(index, candidate, AgentCandidateAvailable, ""))
	}
	if len(resolved) == 0 {
		return nil, report, noRunnableAgentError(configured, probed)
	}
	return resolved, report, nil
}

func (c *Config) candidateResolution(index int, candidate AgentCandidate, status, reason string) AgentCandidateResolution {
	argsHash := ""
	if args := c.AgentArgsForCandidate(candidate); len(args) > 0 {
		argsHash = candidateArgsFingerprint(args)
	}
	return AgentCandidateResolution{
		Index: index, Harness: safeEvidenceHarness(candidate.Harness),
		Provider: safeEvidenceIdentity(candidate.Provider), Model: safeEvidenceIdentity(candidate.Model),
		Label: fmt.Sprintf("candidate-%d", index), ArgsSHA256: argsHash,
		Status: status, Reason: reason, RuntimeLabel: candidate.Label(),
	}
}

func safeEvidenceHarness(harness types.AgentName) types.AgentName {
	value := string(harness)
	if safeEvidenceIdentityToken(value) {
		return harness
	}
	if target, ok := types.ACPTargetFor(harness); ok && safeEvidenceIdentityToken(target) {
		return types.AgentName("acp:" + target)
	}
	return types.AgentName("redacted-" + evidenceIdentityHash(value))
}

// safeEvidenceIdentity is the evidence boundary, not the parse boundary: it is
// deliberately narrower than safeAgentIdentityToken so a configured identity can
// never carry ':' or '@' into recorded evidence. A refused value collapses to a
// stable per-value fingerprint rather than an empty string, so evidence still
// reports that an identity was configured and keeps distinct candidates
// distinguishable.
func safeEvidenceIdentity(value string) string {
	if value == "" {
		return ""
	}
	if !safeEvidenceIdentityToken(value) {
		return "redacted-" + evidenceIdentityHash(value)
	}
	return value
}

func safeEvidenceIdentityToken(value string) bool {
	return safeAgentIdentityToken(value) && !strings.ContainsAny(value, ":@")
}

func evidenceIdentityHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}
