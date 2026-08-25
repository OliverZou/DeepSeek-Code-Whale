//go:build live
// +build live

package team_engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/usewhale/whale/internal/core"
	"github.com/usewhale/whale/internal/llm"
	"github.com/usewhale/whale/internal/llm/deepseek"
)

// loadDeepSeekAPIKeyReal mirrors the CLI's loadDeepSeekAPIKey so this test
// reads the key from the same sources whale setup writes: env var first, then
// ~/.whale/credentials.json's deepseek_api_key field. This lets a live team
// run use the user's saved key without a subprocess or shell env.
func loadDeepSeekAPIKeyReal() string {
	if v := os.Getenv("DEEPSEEK_API_KEY"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".whale", "credentials.json"))
	if err != nil {
		return ""
	}
	var creds struct {
		DeepSeekAPIKey string `json:"deepseek_api_key"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return ""
	}
	return creds.DeepSeekAPIKey
}

// realTeamLeaderSpawner builds a FuncSpawner that calls the DeepSeek API
// directly (mirroring newLiteSpawner), so a live leader decomposition runs
// against the real model instead of a mock. Callers pass a non-empty apiKey.
func realTeamLeaderSpawner(apiKey string) SubagentSpawner {
	return NewFuncSpawner(func(ctx context.Context, req SubagentRequest) (SubagentResponse, error) {
		mdl := req.Model
		if mdl == "" {
			mdl = "deepseek-v4-pro"
		}
		maxTok := req.MaxTokens
		if maxTok <= 0 {
			maxTok = 16384
		}
		client, err := deepseek.New(
			deepseek.WithAPIKey(apiKey),
			deepseek.WithModel(mdl),
			deepseek.WithMaxTokens(maxTok),
			deepseek.WithThinking(false),
			deepseek.WithTemperature(0),
		)
		if err != nil {
			return SubagentResponse{Success: false, Diagnostic: err.Error()}, nil
		}
		messages := []core.Message{{Role: core.RoleUser, Text: req.Task}}
		events := client.StreamResponse(ctx, messages, nil)
		var fullText string
		for ev := range events {
			switch ev.Type {
			case llm.EventContentDelta:
				fullText += ev.Content
			case llm.EventError:
				return SubagentResponse{Output: fullText, Success: false, Diagnostic: ev.Err.Error()}, nil
			case llm.EventComplete:
			}
		}
		return SubagentResponse{Output: fullText, Success: true}, nil
	})
}

// TestRealLLM_LeaderDecomposition runs a REAL leader decomposition against the
// REAL team definition (software-team-lead.md via FindTeam, which sets TeamDir
// so the leader orchestration prose is injected) and the REAL DeepSeek key, then
// verifies the parsed plan is a valid acyclic DAG whose roles come from the
// team vocabulary. It is gated so the offline `go test ./...` suite stays green:
//
//   - `-short` or missing key => t.Skip (not fail)
//   - present key           => live DeepSeek call
//
// This file is gated behind the "live" build tag so the default `go test ./...`
// suite never compiles or runs it (no real key, no real network, no expense).
// Run it explicitly when you WANT a real LLM call, e.g.:
//
//	go test -tags live ./internal/team_engine/ -run TestRealLLM_LeaderDecomposition -v -timeout 15m
func TestRealLLM_LeaderDecomposition(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real LLM test in short mode")
	}
	apiKey := loadDeepSeekAPIKeyReal()
	if apiKey == "" {
		t.Skip("no DEEPSEEK_API_KEY / ~/.whale/credentials.json found; skipping real LLM call")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	teamsDir := filepath.Join(home, ".whale", "teams")
	const teamName = "软件开发团队"

	// FindTeam (directory layout) sets TeamDir, which is REQUIRED for the real
	// leader definition to be injected by leaderOrchestrationText(). Plain
	// LoadTeamConfig would leave TeamDir empty and inject nothing.
	tc, err := FindTeam(teamsDir, teamName)
	if err != nil {
		t.Fatalf("FindTeam(%q): %v", teamName, err)
	}
	if tc == nil {
		t.Fatalf("team %q not found under %s", teamName, teamsDir)
	}
	if tc.TeamDir == "" {
		t.Fatal("TeamDir empty: real leader .md won't be injected; use FindTeam (directory layout)")
	}
	if tc.Leader.Role == "" {
		t.Fatal("leader role empty")
	}

	// The team's member roles come from team.yaml's roles[]; leaving them
	// unresolved falls back to the ref itself (the English agent id), which is
	// exactly the vocabulary we assert below.
	prompt := tc.BuildLeaderPrompt(DecomposePrompt("开发一个用户登录功能"))
	if prompt == "" {
		t.Fatal("leader prompt empty")
	}
	t.Logf("leader orchestration prose injected: %v", containsAny(prompt,
		"团队编排与工作流", "标准SOP", "产品经理", "架构师", "工程师", "QA",
		"用户需求", "software-team-lead"))
	t.Logf("Available team roles present: %v", containsAny(prompt,
		"software-product-manager", "software-architect", "software-engineer", "software-qa-engineer"))

	// Leader model from config.yaml (model.leader), defaulting to v4-pro.
	mdl := "deepseek-v4-pro"
	if tc.Config != nil && tc.Config.Model.Leader != "" {
		mdl = tc.Config.Model.Leader
	}
	t.Logf("using leader model: %s", mdl)

	runner := NewRunner(realTeamLeaderSpawner(apiKey)).WithTeam(tc)
	p := NewPlanner(runner).WithTeam(tc)

	goal := "使用 HTML5、CSS3 和原生 JavaScript 实现一个可在浏览器直接打开的 2048 数字合并小游戏。核心需求：4x4 网格棋盘；支持方向键和触摸滑动两种方式移动方块；相邻相同数字方块合并为双倍；每次有效移动后随机生成一个 2 或 4 的新方块；实时显示当前分数和最高分，最高分用 localStorage 持久化；游戏结束判定即无可移动方块时结束，胜利判定即达成 2048 时胜利；响应式布局适配桌面和移动端。产出 index.html、style.css、game.js 三个文件，并编写游戏核心逻辑移动、合并、胜负判定的单元测试。"
	workdir := t.TempDir()
	timeout := 12 * time.Minute

	start := time.Now()
	tasks, raw, err := p.DecomposeFull(goal, workdir, timeout, mdl)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("real decompose failed: %v\n--- raw output (first 2500 chars) ---\n%s", err, truncate(raw, 2500))
	}
	t.Logf("real decompose succeeded: %d tasks in %s", len(tasks), elapsed.Round(time.Millisecond))
	t.Logf("--- raw output (first 3500 chars) ---\n%s", truncate(raw, 3500))

	if len(tasks) == 0 {
		t.Fatal("real decompose returned zero tasks")
	}

	// 1. The plan must be a valid acyclic DAG: every depends_on_batch points to
	//    an existing batch and there is no cycle.
	levels, err := dagLevels(tasks)
	if err != nil {
		t.Fatalf("real plan is not a valid DAG: %v", err)
	}
	t.Logf("DAG valid: %d distinct batches, levels=%v", len(levels), levels)

	// Printed live so the report is readable at a glance: one line per batch
	// with its role, title and the batches it depends on.
	for _, task := range tasks {
		deps := "-"
		if len(task.DependsOnBatch) > 0 {
			deps = strings.Join(task.DependsOnBatch, ",")
		}
		t.Logf("batch=%s level=%d role=%s deps=[%s] title=%s",
			task.BatchID, levels[task.BatchID], task.Role, deps, task.Title)
	}

	// 2. Roles must come from the team vocabulary (the leader's member schedule
	//    injected from team.yaml); the leader must NOT fall back to generic names
	//    like "developer" / "tester" / "researcher".
	allowedRoles := map[string]bool{
		"software-team-lead":       true,
		"software-product-manager": true,
		"software-architect":       true,
		"software-engineer":        true,
		"software-qa-engineer":     true,
	}
	for _, task := range tasks {
		if !allowedRoles[task.Role] {
			t.Errorf("task %q has role %q not in the team vocabulary", task.Title, task.Role)
		}
		if strings.TrimSpace(task.BatchID) == "" {
			t.Errorf("task %q missing batch_id", task.Title)
		}
	}
}

// --- small helpers (kept local to avoid extra dependencies) ---

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated]"
}
