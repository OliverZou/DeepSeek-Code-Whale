package team_engine

import (
	"regexp"
)

// Router resolves a ToolProfile for a given task based on config rules.
//
// Priority (highest first):
//  1. Verifier-dedicated profile — when isVerifier=true
//  2. Keyword matching — scan description against regex patterns
//  3. Role-default — look up the role in role_map
//  4. Global default
type Router struct {
	cfg *Config
}

// NewRouter creates a Router from a Config.
func NewRouter(cfg *Config) *Router {
	return &Router{cfg: cfg}
}

// ResolveProfile determines which tool profile to use for a task.
func (r *Router) ResolveProfile(role AgentRole, description string, isVerifier bool) ToolProfile {
	rt := r.cfg.Routing

	// 1. Verifier-dedicated profile.
	if isVerifier {
		if rt.VerifierTools != "" {
			return toProfile(rt.VerifierTools)
		}
		return ProfileReadOnly
	}

	// 2. Keyword matching.
	if description != "" {
		for _, rule := range rt.KeywordRules {
			if rule.Pattern != "" {
				if matched, _ := regexp.MatchString("(?i)"+rule.Pattern, description); matched {
					return toProfile(rule.Profile)
				}
			}
		}
	}

	// 3. Role-default.
	if re, ok := rt.RoleMap[string(role)]; ok && re.Profile != "" {
		return toProfile(re.Profile)
	}

	// 4. Global default.
	return toProfile(rt.DefaultProfile)
}

// ResolveTimeout returns the timeout duration (seconds) for a task based on
// config rules.
//
// When isVerifier=true, it returns config's verifier_timeout_sec (default 300).
// Otherwise it uses the per-role timeout from role_map, falling back to 1800
// for unknown/team-specific roles (research/deep-analysis need ample time).
func (r *Router) ResolveTimeout(role AgentRole, isVerifier bool) int {
	if isVerifier {
		if r.cfg.Routing.VerifierTimeoutSec > 0 {
			return r.cfg.Routing.VerifierTimeoutSec
		}
		return 300
	}

	rt := r.cfg.Routing
	if re, ok := rt.RoleMap[string(role)]; ok && re.Timeout > 0 {
		return re.Timeout
	}
	// Default for unknown/team-specific roles (e.g. 宏观策略师) —
	// research/deep-analysis tasks need ample time.
	return 1800
}

// ResolveModel returns the LLM model name for a given role.
// Falls back to empty string (Whale default) when not configured.
func (r *Router) ResolveModel(role AgentRole) string {
	if re, ok := r.cfg.Routing.RoleMap[string(role)]; ok && re.Model != "" {
		return re.Model
	}
	return ""
}

// ResolveDecomposerTimeout returns the decomposer (Leader) timeout in seconds.
func (r *Router) ResolveDecomposerTimeout() int {
	if r.cfg.Routing.DecomposerTimeoutSec > 0 {
		return r.cfg.Routing.DecomposerTimeoutSec
	}
	return 180
}

// ProfileToToolNames maps a ToolProfile to the list of Whale tool names
// that the subagent should be allowed to use.
func ProfileToToolNames(profile ToolProfile) []string {
	switch profile {
	case ProfileDefault:
		return []string{
			"read_file", "list_dir", "grep", "search_files",
			"edit", "write", "apply_patch",
			"shell_run", "shell_wait", "shell_cancel", "write_stdin",
		}
	case ProfileReadOnly:
		return []string{
			"read_file", "list_dir", "grep", "search_files",
			"web_search", "web_fetch", "fetch",
		}
	case ProfileResearch:
		return []string{
			"read_file", "list_dir", "grep", "search_files",
			"web_search", "web_fetch", "fetch",
		}
	case ProfileContent:
		return []string{
			"read_file", "list_dir", "grep", "search_files",
			"edit", "write", "apply_patch",
		}
	case ProfileTest:
		return []string{
			"read_file", "list_dir", "grep", "search_files",
			"edit", "write", "apply_patch",
			"shell_run", "shell_wait", "shell_cancel",
		}
	case ProfileVerify:
		// Tool-grounded verification: can run test/lint/build via shell and
		// write its own acceptance-level test artifacts to the verify/ dir.
		// The write tool replaces shell-based file drops (mkdir/cat heredoc),
		// which break under cmd.exe/pwsh on Windows and leave stray "-p" dirs
		// or tmp files. Decision must still be backed by command output.
		return []string{
			"read_file", "list_dir", "grep", "search_files",
			"write",
			"shell_run", "shell_wait", "shell_cancel",
			"web_search", "web_fetch", "fetch",
		}
	default:
		return ProfileToToolNames(ProfileDefault)
	}
}

func toProfile(name string) ToolProfile {
	switch name {
	case "default":
		return ProfileDefault
	case "read_only":
		return ProfileReadOnly
	case "research":
		return ProfileResearch
	case "content":
		return ProfileContent
	case "test":
		return ProfileTest
	case "verify":
		return ProfileVerify
	default:
		return ProfileDefault
	}
}
