package tasks

import (
	"strings"
	"testing"
)

func TestSystemVerifierDefinition(t *testing.T) {
	def, ok, err := SystemAgentDefinition("verifier")
	if err != nil {
		t.Fatalf("SystemAgentDefinition: %v", err)
	}
	if !ok {
		t.Fatal("system verifier definition not found")
	}
	if def.Name != "verifier" {
		t.Errorf("Name = %q, want verifier", def.Name)
	}
	has := func(list []string, want string) bool {
		for _, s := range list {
			if s == want {
				return true
			}
		}
		return false
	}
	if !has(def.Tools, CapabilityWorkspaceRead) || !has(def.Tools, CapabilityShellRun) {
		t.Errorf("system verifier tools = %v, want read + shell", def.Tools)
	}
	if !has(def.DisallowedTools, CapabilityWorkspaceWrite) {
		t.Errorf("system verifier disallowedTools = %v, want workspace.write", def.DisallowedTools)
	}
	if def.PermissionMode != AgentPermissionAuto {
		t.Errorf("permissionMode = %q, want auto", def.PermissionMode)
	}
	if !strings.Contains(def.Prompt, "不修改交付物") {
		t.Errorf("verifier persona must forbid modifying deliverables")
	}
}
