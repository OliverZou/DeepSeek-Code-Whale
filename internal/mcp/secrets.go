package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const secretsFile = "mcp_secrets.json"

// secretsStore holds user-provided values for MCP env vars (tokens, keys, etc.).
// Values are keyed by server name, then env var name.
// Persisted to ~/.whale/mcp_secrets.json.
type secretsStore struct {
	mu   sync.RWMutex
	path string
	data map[string]map[string]string // server → env_name → value
}

func loadSecrets(dataDir string) *secretsStore {
	s := &secretsStore{
		path: filepath.Join(dataDir, secretsFile),
		data: map[string]map[string]string{},
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return s
	}
	json.Unmarshal(b, &s.data)
	return s
}

// Lookup returns the secret value for (server, envName), or empty if not found.
func (s *secretsStore) Lookup(server, envName string) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if sv, ok := s.data[server]; ok {
		if v, ok2 := sv[envName]; ok2 {
			return v, true
		}
	}
	return "", false
}

// Set stores a secret value and persists.
func (s *secretsStore) Set(server, envName, value string) error {
	if s == nil {
		return fmt.Errorf("secrets store not initialized")
	}
	s.mu.Lock()
	if s.data[server] == nil {
		s.data[server] = map[string]string{}
	}
	s.data[server][envName] = value
	s.mu.Unlock()
	return s.save()
}

func (s *secretsStore) save() error {
	s.mu.RLock()
	b, err := json.MarshalIndent(s.data, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0600)
}
