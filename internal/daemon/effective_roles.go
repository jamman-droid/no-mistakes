package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
)

// EffectiveAgentRoles is the precedence-resolved role policy for one exact
// repository checkout plus the freshly pinned trusted default-branch config.
// It intentionally performs no executable probes and constructs no agents.
type EffectiveAgentRoles struct {
	TargetSHA         string
	TrustedConfigSHA  string
	AllowRepoCommands bool
	Roles             config.AgentRoleResolution
}

// ResolveEffectiveAgentRoles applies the same trusted/global boundary as run
// startup without launching or probing an agent. Fetch/read failure fails
// closed instead of falling back to a stale default-branch ref.
func ResolveEffectiveAgentRoles(ctx context.Context, p *paths.Paths, repo *db.Repo, workDir string) (*EffectiveAgentRoles, error) {
	globalCfg, err := config.LoadGlobal(p.ConfigFile())
	if err != nil {
		return nil, fmt.Errorf("load global config: %w", err)
	}
	targetSHA, err := git.ResolveRef(ctx, workDir, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("resolve target HEAD: %w", err)
	}
	repoCfg, err := loadRepoConfigAtSHA(ctx, workDir, targetSHA)
	if err != nil {
		return nil, fmt.Errorf("load target repository config: %w", err)
	}

	var trustedSHA string
	var pinReason error
	if repo.DefaultBranch != "" {
		fetchCtx, cancel := context.WithTimeout(ctx, recoveredConfigFetchTimeout)
		defer cancel()
		if err := fetchRecoveredRemoteBranch(fetchCtx, workDir, "origin", repo.DefaultBranch); err != nil {
			slog.Warn("failed to fetch default branch for effective role query", "branch", repo.DefaultBranch, "error", err)
			pinReason = fmt.Errorf("fetch: %s", safeurl.RedactText(err.Error()))
		} else if sha, err := git.ResolveRef(ctx, workDir, "refs/remotes/origin/"+repo.DefaultBranch); err != nil {
			slog.Warn("failed to resolve default branch for effective role query", "branch", repo.DefaultBranch, "error", err)
			pinReason = fmt.Errorf("resolve: %s", safeurl.RedactText(err.Error()))
		} else {
			trustedSHA = sha
		}
	}
	if err := assertGateTrustedConfigReadable(ctx, workDir, trustedConfigSubjectRoles, repo.DefaultBranch, trustedSHA); err != nil {
		if pinReason != nil {
			return nil, fmt.Errorf("%w (%w)", err, pinReason)
		}
		return nil, err
	}
	trustedRepoCfg := loadTrustedRepoConfig(ctx, workDir, trustedSHA, "effective-role-query")
	cfg, _, allowRepoCommands := mergeEffectiveAgentConfig(globalCfg, repoCfg, trustedRepoCfg)
	cfg.TrustedConfigSHA = trustedSHA

	return &EffectiveAgentRoles{
		TargetSHA: targetSHA, TrustedConfigSHA: trustedSHA,
		AllowRepoCommands: allowRepoCommands, Roles: cfg.EffectiveAgentRolePolicy(),
	}, nil
}

// mergeEffectiveAgentConfig is the shared trusted/global precedence seam used
// by startup, recovery, and the public read-only resolver.
func mergeEffectiveAgentConfig(globalCfg *config.GlobalConfig, pushed, trusted *config.RepoConfig) (*config.Config, *config.RepoConfig, bool) {
	allowRepoCommands := trusted != nil && trusted.AllowRepoCommands
	effective := config.EffectiveRepoConfig(pushed, trusted, allowRepoCommands)
	return config.Merge(globalCfg, effective), effective, allowRepoCommands
}

func loadRepoConfigAtSHA(ctx context.Context, workDir, sha string) (*config.RepoConfig, error) {
	entry, err := git.Run(ctx, workDir, "ls-tree", sha, "--", ".no-mistakes.yaml")
	if err != nil {
		return nil, fmt.Errorf("inspect tree %s: %w", sha, err)
	}
	if entry == "" {
		return &config.RepoConfig{}, nil
	}
	content, err := git.ShowFile(ctx, workDir, sha, ".no-mistakes.yaml")
	if err != nil {
		return nil, fmt.Errorf("read .no-mistakes.yaml at %s: %w", sha, err)
	}
	cfg, err := config.LoadRepoFromBytes([]byte(content))
	if err != nil {
		return nil, fmt.Errorf("parse .no-mistakes.yaml at %s: %w", sha, err)
	}
	return cfg, nil
}
