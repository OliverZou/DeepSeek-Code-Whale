package team_engine

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// runIndexFile is the global registry mapping master UUIDs to their
// workspace — a master is globally unique, so status/analyze should resolve
// the workspace by UUID instead of making the user repeat --workdir.
var (
	runIndexOnce sync.Once
	runIndexPath string
	runIndexMu   sync.Mutex
)

func runIndexFile() string {
	runIndexOnce.Do(func() {
		home, err := os.UserHomeDir()
		if err != nil {
			runIndexPath = ""
			return
		}
		runIndexPath = filepath.Join(home, ".whale", "team_runs.jsonl")
	})
	return runIndexPath
}

// RecordRunIndex appends one line: master UUID -> workspace. Called after a
// master is created (CreateMasterTask), covering both the /team app path and
// the CLI execute path.
func RecordRunIndex(masterID, workdir, goal string) {
	path := runIndexFile()
	if path == "" {
		return
	}
	entry := map[string]string{
		"ts":        time.Now().UTC().Format(time.RFC3339),
		"master_id": masterID,
		"workdir":   workdir,
		"goal":      goal,
	}
	data, _ := json.Marshal(entry)
	runIndexMu.Lock()
	defer runIndexMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

// ResolveRunIndex returns the workspace for a master UUID (latest registration
// wins). ok=false when the index is empty/missing or the UUID is unknown.
func ResolveRunIndex(masterID string) (workdir string, ok bool) {
	path := runIndexFile()
	if path == "" {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 256*1024)
	var last string
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		var e map[string]string
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if e["master_id"] != masterID {
			continue
		}
		last = e["workdir"]
	}
	if last == "" {
		return "", false
	}
	return last, true
}
