package team_engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type ExpertEntry struct {
	Name        string   `yaml:"name"`
	NameEn      string   `yaml:"name_en"`
	Agent       string   `yaml:"agent"`
	Icon        string   `yaml:"icon,omitempty"`
	Description string   `yaml:"description"`
	Domains     []string `yaml:"domains,omitempty"`
	Skills      []string `yaml:"skills,omitempty"`
}

// UnmarshalYAML handles skills that may be plain strings (e.g. "UI设计")
// or locale-keyed maps (e.g. {zh: "文化适配", en: "Cultural Adaptation"}).
func (e *ExpertEntry) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		Name        string      `yaml:"name"`
		NameEn      string      `yaml:"name_en"`
		Agent       string      `yaml:"agent"`
		Icon        string      `yaml:"icon,omitempty"`
		Description string      `yaml:"description"`
		Domains     []string    `yaml:"domains,omitempty"`
		Skills      []yaml.Node `yaml:"skills,omitempty"`
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}
	e.Name = raw.Name
	e.NameEn = raw.NameEn
	e.Agent = raw.Agent
	e.Icon = raw.Icon
	e.Description = raw.Description
	e.Domains = raw.Domains
	for _, node := range raw.Skills {
		switch node.Kind {
		case yaml.ScalarNode:
			e.Skills = append(e.Skills, node.Value)
		case yaml.MappingNode:
			var zh string
			for i := 0; i+1 < len(node.Content); i += 2 {
				k := node.Content[i].Value
				v := node.Content[i+1].Value
				if k == "zh" || zh == "" {
					zh = v
				}
			}
			if zh != "" {
				e.Skills = append(e.Skills, zh)
			}
		}
	}
	return nil
}

type ExpertFile struct {
	Domain   string        `yaml:"domain"`
	DomainEn string        `yaml:"domain_en"`
	Icon     string        `yaml:"icon,omitempty"`
	Experts  []ExpertEntry `yaml:"experts"`
}

type ExpertRef struct {
	Domain string
	Name   string
}

type ExpertRegistry struct {
	files   []ExpertFile
	byRef   map[ExpertRef]*ExpertEntry
	byAgent map[string]*ExpertEntry
}

func ParseExpertRef(s string) ExpertRef {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) == 2 {
		return ExpertRef{Domain: parts[0], Name: parts[1]}
	}
	return ExpertRef{Domain: "", Name: parts[0]}
}

func (r ExpertRef) String() string {
	if r.Domain != "" {
		return r.Domain + "/" + r.Name
	}
	return r.Name
}

func LoadExpertFile(path string) (*ExpertFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read expert file %s: %w", path, err)
	}
	var ef ExpertFile
	if err := yaml.Unmarshal(data, &ef); err != nil {
		return nil, fmt.Errorf("parse expert file %s: %w", path, err)
	}
	if ef.Domain == "" {
		ef.Domain = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	for i := range ef.Experts {
		if ef.Experts[i].NameEn == "" {
			ef.Experts[i].NameEn = ef.Experts[i].Agent
		}
	}
	return &ef, nil
}

func LoadAllExperts(dir string) (*ExpertRegistry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return &ExpertRegistry{byRef: make(map[ExpertRef]*ExpertEntry), byAgent: make(map[string]*ExpertEntry)}, nil
		}
		return nil, fmt.Errorf("read experts dir: %w", err)
	}
	reg := &ExpertRegistry{
		byRef:   make(map[ExpertRef]*ExpertEntry),
		byAgent: make(map[string]*ExpertEntry),
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		ef, err := LoadExpertFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		reg.files = append(reg.files, *ef)
		for i := range ef.Experts {
			exp := &ef.Experts[i]
			ref := ExpertRef{Domain: ef.Domain, Name: exp.Name}
			reg.byRef[ref] = exp
			for _, d := range exp.Domains {
				if d != ef.Domain {
					crossRef := ExpertRef{Domain: d, Name: exp.Name}
					if _, exists := reg.byRef[crossRef]; !exists {
						reg.byRef[crossRef] = exp
					}
				}
			}
			if exp.Agent != "" {
				reg.byAgent[exp.Agent] = exp
			}
		}
	}
	return reg, nil
}

func (r *ExpertRegistry) Resolve(ref string) *ExpertEntry {
	parsed := ParseExpertRef(ref)
	if parsed.Domain != "" {
		if exp, ok := r.byRef[parsed]; ok {
			return exp
		}
	}
	for domain, exp := range r.byRef {
		if exp.Name == parsed.Name {
			_ = domain
			return exp
		}
	}
	if exp, ok := r.byAgent[ref]; ok {
		return exp
	}
	return nil
}

func (r *ExpertRegistry) ResolveAgentName(ref string) string {
	exp := r.Resolve(ref)
	if exp != nil && exp.Agent != "" {
		return exp.Agent
	}
	return ref
}

func (r *ExpertRegistry) DisplayName(ref string) string {
	exp := r.Resolve(ref)
	if exp != nil {
		return exp.Name
	}
	return ref
}

func (r *ExpertRegistry) AllExperts() []ExpertEntry {
	var out []ExpertEntry
	for _, ef := range r.files {
		out = append(out, ef.Experts...)
	}
	return out
}

func (r *ExpertRegistry) Files() []ExpertFile {
	return r.files
}
