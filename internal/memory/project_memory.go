package memory

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/usewhale/whale/internal/defaults"
)

type ProjectMemory struct {
	Path          string
	Content       string
	OriginalChars int
	Truncated     bool
}

func ReadProjectMemory(workspaceRoot string, fileOrder []string, maxChars int) (ProjectMemory, bool) {
	root := strings.TrimSpace(workspaceRoot)
	if root == "" {
		return ProjectMemory{}, false
	}
	if maxChars <= 0 {
		maxChars = defaults.DefaultMemoryMaxChars
	}
	for _, name := range fileOrder {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		path := filepath.Join(root, name)
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		raw := strings.TrimSpace(string(b))
		if raw == "" {
			return ProjectMemory{}, false
		}
		pm := ProjectMemory{
			Path:          path,
			Content:       raw,
			OriginalChars: len(raw),
		}
		if len(raw) > maxChars {
			pm.Truncated = true
			pm.Content = raw[:maxChars] + "\n... (truncated)"
		}
		return pm, true
	}
	return ProjectMemory{}, false
}

// ReadSavedMemory aggregates facts previously saved via the save_project_memory
// tool under <root>/.whale/memory/*.md and returns them as a single markdown
// blob for injection into the system prompt. Returns ok=false when no saved
// facts exist. Each file becomes a "## <key>" section.
func ReadSavedMemory(workspaceRoot string, maxChars int) (ProjectMemory, bool) {
	root := strings.TrimSpace(workspaceRoot)
	if root == "" {
		return ProjectMemory{}, false
	}
	if maxChars <= 0 {
		maxChars = defaults.DefaultMemoryMaxChars
	}
	dir := filepath.Join(root, ".whale", "memory")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ProjectMemory{}, false
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return ProjectMemory{}, false
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		content := strings.TrimSpace(string(raw))
		if content == "" {
			continue
		}
		b.WriteString("## ")
		b.WriteString(strings.TrimSuffix(name, ".md"))
		b.WriteString("\n")
		b.WriteString(content)
		b.WriteString("\n\n")
	}
	blob := strings.TrimSpace(b.String())
	if blob == "" {
		return ProjectMemory{}, false
	}
	pm := ProjectMemory{
		Path:          dir,
		Content:       blob,
		OriginalChars: len(blob),
	}
	if len(blob) > maxChars {
		pm.Truncated = true
		pm.Content = blob[:maxChars] + "\n... (truncated)"
	}
	return pm, true
}
