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

// ResolveTimeout returns the timeout duration for a task based on config rules.
func (r *Router) ResolveTimeout(role AgentRole, isVerifier bool) int {
	// Verifier — always 120s (lightweight check).
	if isVerifier {
		return 120
	}

	rt := r.cfg.Routing
	if re, ok := rt.RoleMap[string(role)]; ok && re.Timeout > 0 {
		return re.Timeout
	}
	return 600
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
		// Tool-grounded verification: can run test/lint/build via shell
		// but cannot modify files. Decision must be based on command output.
		return []string{
			"read_file", "list_dir", "grep", "search_files",
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
	default:
		return ProfileDefault
	}
}
