package team_engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPersonaSummary_LeaderHeadlineAndMemberNames(t *testing.T) {
	dir := t.TempDir()
	agents := filepath.Join(dir, "agents")
	if err := os.MkdirAll(agents, 0755); err != nil {
		t.Fatal(err)
	}
	lead := "---\nname: software-team-lead\ndescription: Lead who orchestrates\n---\n\n# 软件开发团队 - 主理人\n## 齐活林（Qi） · 交付总监（Delivery Director）\n\n## 团队成员\n\n| 成员 | 姓名 | 文件 | 职责 |\n|------|------|------|------|\n| 工程师 | 寇豆码（Kou） | software-engineer.md | 批量实现代码 |\n| QA | 严过关（Yan） | software-qa-engineer.md | 测试验证 |\n"
	if err := os.WriteFile(filepath.Join(agents, "software-team-lead.md"), []byte(lead), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agents, "software-engineer.md"), []byte("# Engineer - Alex\n\n## Core Identity\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tc := &TeamConfig{TeamDir: dir, Leader: TeamLeaderConfig{Role: "software-team-lead"}, Roles: []string{"software-engineer", "software-qa-engineer"}}

	got := tc.PersonaSummary()
	if !strings.Contains(got, "齐活林（Qi） · 交付总监（Delivery Director）") {
		t.Errorf("leader headline missing: %q", got)
	}
	if !strings.Contains(got, "寇豆码（Kou）") {
		t.Errorf("member name from leader table missing: %q", got)
	}
	if !strings.Contains(got, "software-engineer") {
		t.Errorf("member agent name missing: %q", got)
	}
	if strings.Contains(got, "Core Identity") || strings.Contains(got, "Coding Process") {
		t.Errorf("member definition leaked into persona summary: %q", got)
	}
}

func TestPersonaSummary_NoTeamDir(t *testing.T) {
	tc := &TeamConfig{TeamDir: ""}
	if got := tc.PersonaSummary(); got != "" {
		t.Errorf("expected empty persona for dir-less team, got %q", got)
	}
}
