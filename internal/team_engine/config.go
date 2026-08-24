package team_engine

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config holds Team Engine configuration loaded from a YAML file.
type Config struct {
	Routing RoutingConfig `yaml:"routing"`
	Batch   BatchConfig   `yaml:"batch"`
}

// RoleEntry describes a role's tool profile, timeout, and LLM model.
type RoleEntry struct {
	Profile string `yaml:"profile"`
	Timeout int    `yaml:"timeout"` // seconds; 0 = default (600s)
	Model   string `yaml:"model"`   // LLM model name; "" = use Whale default
}

// BatchConfig defines default batch execution parameters.
type BatchConfig struct {
	MaxAgents        int `yaml:"max_agents"`         // max simultaneous agents; 0 = unlimited
	DefaultMaxCycles int `yaml:"default_max_cycles"` // 0 = unlimited
}

// RoutingConfig defines how tasks are routed to tool profiles.
type RoutingConfig struct {
	RoleMap              map[string]RoleEntry `yaml:"role_map"`
	KeywordRules         []KeywordRule        `yaml:"keyword_rules"`
	VerifierTools        string               `yaml:"verifier_tools"`
	DefaultProfile       string               `yaml:"default_profile"`
	VerifierTimeoutSec   int                  `yaml:"verifier_timeout_sec"`   // 0 = default 900
	DecomposerTimeoutSec int                  `yaml:"decomposer_timeout_sec"` // 0 = default 180
}

// KeywordRule maps a regex pattern to a tool profile.
type KeywordRule struct {
	Pattern string `yaml:"pattern"`
	Profile string `yaml:"profile"`
}

// Defaults is the hardcoded configuration used when no YAML file is
// present.
var Defaults = Config{
	Routing: RoutingConfig{
		DefaultProfile: "default",
		RoleMap: map[string]RoleEntry{
			"planner":     {Profile: "read_only", Timeout: 180, Model: "deepseek-v4-pro"},   // 3 min — plan decomposition
			"developer":   {Profile: "default", Timeout: 1800, Model: "deepseek-v4-pro"},    // 30 min — coding
			"tester":      {Profile: "test", Timeout: 1200, Model: "deepseek-v4-flash"},     // 20 min
			"reviewer":    {Profile: "read_only", Timeout: 900, Model: "deepseek-v4-flash"}, // 15 min
			"researcher":  {Profile: "research", Timeout: 1800, Model: "deepseek-v4-flash"}, // 30 min — web search + deep analysis
			"writer":      {Profile: "content", Timeout: 1800, Model: "deepseek-v4-flash"},  // 30 min — long-form writing
			"formatter":   {Profile: "content", Timeout: 900, Model: "deepseek-v4-flash"},
			"evaluator":   {Profile: "read_only", Timeout: 900, Model: "deepseek-v4-flash"},  // 15 min
			"synthesizer": {Profile: "read_only", Timeout: 1200, Model: "deepseek-v4-flash"}, // 20 min — merging results
		},
		VerifierTools:        "verify",
		VerifierTimeoutSec:   900, // 15 min — verifier runs worker tests + semantic review
		DecomposerTimeoutSec: 300, // 5 min — plan decomposition (v4-pro needs ~90s, v4-flash ~30s)
	},
	Batch: BatchConfig{
		MaxAgents:        9,  // max simultaneous Worker+Verifier pairs
		DefaultMaxCycles: 10, // safety cap — real exit is loop-until-dry
	},
}

// Load reads and parses a team_engine.yaml file, merging it with
// hardcoded defaults.  If path is empty or the file cannot be read,
// the defaults are returned as-is.
func Load(path string) (*Config, error) {
	if path == "" {
		cfg := Defaults
		return &cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := Defaults
			return &cfg, nil
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var fileCfg Config
	if err := yaml.Unmarshal(data, &fileCfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	merged := mergeConfig(Defaults, fileCfg)
	return &merged, nil
}

// mergeConfig performs a shallow merge of routing fields: file values
// take precedence over defaults.
func mergeConfig(base, override Config) Config {
	result := base

	if override.Routing.DefaultProfile != "" {
		result.Routing.DefaultProfile = override.Routing.DefaultProfile
	}
	if override.Routing.VerifierTools != "" {
		result.Routing.VerifierTools = override.Routing.VerifierTools
	}
	// Use len(v) > 0 instead of v != nil so that an explicit empty map in YAML
	// (role_map: {}) does NOT overwrite defaults.
	if len(override.Routing.RoleMap) > 0 {
		if result.Routing.RoleMap == nil {
			result.Routing.RoleMap = make(map[string]RoleEntry)
		}
		for k, v := range override.Routing.RoleMap {
			result.Routing.RoleMap[k] = v
		}
	}
	if len(override.Routing.KeywordRules) > 0 {
		result.Routing.KeywordRules = override.Routing.KeywordRules
	}

	// Timeout config merge.
	if override.Routing.VerifierTimeoutSec > 0 {
		result.Routing.VerifierTimeoutSec = override.Routing.VerifierTimeoutSec
	}
	if override.Routing.DecomposerTimeoutSec > 0 {
		result.Routing.DecomposerTimeoutSec = override.Routing.DecomposerTimeoutSec
	}

	// Batch config merge.
	if override.Batch.MaxAgents > 0 {
		result.Batch.MaxAgents = override.Batch.MaxAgents
	}
	if override.Batch.DefaultMaxCycles > 0 {
		result.Batch.DefaultMaxCycles = override.Batch.DefaultMaxCycles
	}

	return result
}
