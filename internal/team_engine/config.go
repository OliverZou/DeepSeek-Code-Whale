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

// RoleEntry describes a role's tool profile and timeout.
type RoleEntry struct {
	Profile string `yaml:"profile"`
	Timeout int    `yaml:"timeout"` // seconds; 0 = default (600s)
}

// BatchConfig defines default batch execution parameters.
type BatchConfig struct {
	DefaultConcurrency int `yaml:"default_concurrency"`  // 0 = unlimited
	DefaultMaxCycles   int `yaml:"default_max_cycles"`    // 0 = unlimited
}

// RoutingConfig defines how tasks are routed to tool profiles.
type RoutingConfig struct {
	RoleMap        map[string]RoleEntry `yaml:"role_map"`
	KeywordRules   []KeywordRule        `yaml:"keyword_rules"`
	VerifierTools  string               `yaml:"verifier_tools"`
	DefaultProfile string               `yaml:"default_profile"`
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
			"developer":  {Profile: "default", Timeout: 600},
			"tester":     {Profile: "test", Timeout: 600},
			"reviewer":   {Profile: "read_only", Timeout: 600},
			"researcher": {Profile: "research", Timeout: 900},
			"writer":     {Profile: "content", Timeout: 600},
			"formatter":  {Profile: "content", Timeout: 600},
			"evaluator":    {Profile: "read_only", Timeout: 600},
			"synthesizer":  {Profile: "read_only", Timeout: 300},
		},
		VerifierTools: "verify",
	},
	Batch: BatchConfig{
		DefaultConcurrency: 0,  // unlimited
		DefaultMaxCycles:   0,  // unlimited
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
		// On parse error, fall back to defaults.
		cfg := Defaults
		return &cfg, nil
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
	if override.Routing.RoleMap != nil {
		for k, v := range override.Routing.RoleMap {
			if result.Routing.RoleMap == nil {
				result.Routing.RoleMap = make(map[string]RoleEntry)
			}
			result.Routing.RoleMap[k] = v
		}
	}
	if override.Routing.KeywordRules != nil {
		result.Routing.KeywordRules = override.Routing.KeywordRules
	}

	// Batch config merge.
	if override.Batch.DefaultConcurrency > 0 {
		result.Batch.DefaultConcurrency = override.Batch.DefaultConcurrency
	}
	if override.Batch.DefaultMaxCycles > 0 {
		result.Batch.DefaultMaxCycles = override.Batch.DefaultMaxCycles
	}

	return result
}
