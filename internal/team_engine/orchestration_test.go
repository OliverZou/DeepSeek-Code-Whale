package team_engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeLeaderDefinition writes a leader .md definition file (with a YAML
// frontmatter header) into <teamDir>/agents/<role>.md, mimicking the on-disk
// layout that leaderOrchestrationText() reads. The frontmatter is stripped
// before injection; the body's orchestration prose is what the LLM must follow.
func writeLeaderDefinition(t *testing.T, teamDir, role, frontmatter, body string) {
	t.Helper()
	agentsDir := filepath.Join(teamDir, "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir agents dir: %v", err)
	}
	md := frontmatter + "\n" + body
	if err := os.WriteFile(filepath.Join(agentsDir, role+".md"), []byte(md), 0o644); err != nil {
		t.Fatalf("write leader definition: %v", err)
	}
}

// dagLevels computes the layer (level) of every batch in a task plan,
// returning batchID → level (0 for roots without dependencies, increasing as
// dependency depth grows). It also validates DAG integrity: every depends_on
// reference must point at an existing batch, and the graph must be acyclic.
func dagLevels(tasks []PlanTask) (map[string]int, error) {
	batchSet := make(map[string]bool, len(tasks))
	batchDeps := make(map[string][]string)
	for _, t := range tasks {
		if t.BatchID == "" {
			return nil, fmt.Errorf("task %q missing batch_id", t.Title)
		}
		batchSet[t.BatchID] = true
	}
	for _, t := range tasks {
		seen := make(map[string]bool)
		var deps []string
		for _, dep := range t.DependsOnBatch {
			if !batchSet[dep] {
				return nil, fmt.Errorf("batch %q depends on missing batch %q", t.BatchID, dep)
			}
			if !seen[dep] {
				seen[dep] = true
				deps = append(deps, dep)
			}
		}
		batchDeps[t.BatchID] = append(batchDeps[t.BatchID], deps...)
	}

	state := make(map[string]int) // 1=visiting, 2=done
	levels := make(map[string]int)
	var visit func(b string) (int, error)
	visit = func(b string) (int, error) {
		switch state[b] {
		case 1:
			return 0, fmt.Errorf("cycle detected at batch %q", b)
		case 2:
			return levels[b], nil
		}
		state[b] = 1
		maxDep := -1
		for _, dep := range batchDeps[b] {
			l, err := visit(dep)
			if err != nil {
				return 0, err
			}
			if l > maxDep {
				maxDep = l
			}
		}
		state[b] = 2
		levels[b] = maxDep + 1
		return levels[b], nil
	}
	for _, t := range tasks {
		if _, err := visit(t.BatchID); err != nil {
			return nil, err
		}
	}
	return levels, nil
}

// TestBuildLeaderPrompt_InjectsLeaderOrchestration verifies that the team's
// leader .md orchestration prose (标准SOP / 工作流路由 …) is injected into the
// decompose prompt, with the YAML frontmatter stripped so only the routing
// rules the LLM must understand reach the model.
// TestBuildLeaderPrompt_InjectsOutputOwnership sees that the base decompose
// prompt carries the physical-file ownership rule as an explicit key rule, so
// the LLM is told up front that a single deliverable file must not be split
// across two tasks (e.g. game.js assigned to both a logic task and a DOM task).
func TestBuildLeaderPrompt_InjectsOutputOwnership(t *testing.T) {
	prompt := DecomposePrompt("build a website")
	if !strings.Contains(prompt, "## 物理文件所有权唯一（关键）") {
		t.Error("base decompose prompt must carry the physical-file ownership rule (## 物理文件所有权唯一)")
	}
	if !strings.Contains(prompt, "每个交付物物理文件在整份计划中必须恰好被一个任务拥有") {
		t.Error("ownership rule must state that exactly one task owns each deliverable file")
	}
	if !strings.Contains(prompt, "output 里的每个文件都只出现一次") {
		t.Error("ownership rule must ask the LLM to self-check unique output paths")
	}
}

func TestBuildLeaderPrompt_InjectsLeaderOrchestration(t *testing.T) {
	teamDir := t.TempDir()
	writeLeaderDefinition(t, teamDir, "software-team-lead",
		"---\nname: software-team-lead\ndescription: 软件开发团队 leader\n---",
		"## 标准SOP工作流\n\n用户需求 → 产品经理(PRD) → 架构师(系统设计) → 工程师(代码实现) → QA工程师(测试验证)。")

	tc := &TeamConfig{
		TeamDir: teamDir,
		Leader:  TeamLeaderConfig{Role: "software-team-lead"},
	}

	prompt := tc.BuildLeaderPrompt(DecomposePrompt("build a website"))

	if !strings.Contains(prompt, "## 团队编排与工作流") {
		t.Error("leader orchestration section header not injected")
	}
	if !strings.Contains(prompt, "标准SOP工作流") {
		t.Error("leader orchestration body (标准SOP) not injected")
	}
	if !strings.Contains(prompt, "产品经理") {
		t.Error("leader orchestration body (产品经理 step) not injected")
	}
	// The frontmatter carries only name/description — it must be stripped so
	// the injected prose is the orchestration rules, not metadata.
	if strings.Contains(prompt, "name: software-team-lead") {
		t.Error("yaml frontmatter was not stripped from orchestration text")
	}
}

// TestDecompose_ParallelDagPreserved feeds the planner a DAG with two parallel
// root batches and one converged batch, then asserts the parsed plan keeps the
// diamond structure (roots have no depends_on, the converged node depends on
// both roots) — i.e. the LLM's DAG-structured编排 is faithfully preserved.
func TestDecompose_ParallelDagPreserved(t *testing.T) {
	dagJSON := `[
      {"title":"设计接口","description":"定义接口契约","output":"docs/contract.md","role":"software-architect","verify_mode":"semantic","acceptance_criteria":["契约完整"],"batch_id":"1","batch_label":"设计","depends_on_batch":[],"depends_on_index":-1,"verifier_focus":"architecture","max_cycles":1},
      {"title":"调研竞品","description":"调研市场现状","output":"docs/research.md","role":"software-product-manager","verify_mode":"semantic","acceptance_criteria":["来源可靠"],"batch_id":"2","batch_label":"调研","depends_on_batch":[],"depends_on_index":-1,"verifier_focus":"correctness","max_cycles":1},
      {"title":"集成验证","description":"端到端验收","output":"docs/report.md","role":"software-qa-engineer","verify_mode":"semantic","acceptance_criteria":["端到端可工作"],"batch_id":"3","batch_label":"集成","depends_on_batch":["1","2"],"depends_on_index":-1,"verifier_focus":"correctness","max_cycles":1}
    ]`

	teamDir := t.TempDir()
	team := &TeamConfig{TeamDir: teamDir, Leader: TeamLeaderConfig{Role: "software-team-lead"}}
	capture := &capturingSpawner{resp: SubagentResponse{SessionID: "sess-1", Output: dagJSON, Success: true, ExitCode: 0}}
	runner := NewRunner(capture).WithTeam(team)
	p := NewPlanner(runner).WithTeam(team)

	tasks, _, err := p.DecomposeFull("build site", teamDir, 5*time.Second)
	if err != nil {
		t.Fatalf("decompose failed: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("got %d tasks, want 3", len(tasks))
	}

	// Batches 1 and 2 are parallel roots: no depends_on_batch.
	for _, b := range []string{"1", "2"} {
		for _, task := range tasks {
			if task.BatchID == b && len(task.DependsOnBatch) != 0 {
				t.Errorf("batch %s should be a parallel root, got deps %v", b, task.DependsOnBatch)
			}
		}
	}

	// Batch 3 converges on both roots, forming a diamond rather than a chain.
	var b3 *PlanTask
	for i := range tasks {
		if tasks[i].BatchID == "3" {
			b3 = &tasks[i]
		}
	}
	if b3 == nil {
		t.Fatal("missing converged batch 3")
	}
	if len(b3.DependsOnBatch) != 2 {
		t.Errorf("batch 3 deps = %v, want [1 2] (diamond DAG)", b3.DependsOnBatch)
	}

	// The whole plan must be a valid acyclic DAG with batch 3 at the deepest layer.
	levels, err := dagLevels(tasks)
	if err != nil {
		t.Fatalf("invalid DAG: %v", err)
	}
	if levels["1"] != 0 || levels["2"] != 0 {
		t.Errorf("root levels = %v, want batch 1 and 2 at level 0", levels)
	}
	if levels["3"] != 1 {
		t.Errorf("converged level = %d, want 1", levels["3"])
	}
}

// TestDecompose_FollowsLeaderStandardSOP wires the leader definition (标准SOP:
// 用户需求 → 产品经理 → 架构师 → 工程师 → QA) into the runner, feeds a plan
// whose batches follow that very order and chain, then asserts:
//   1. the decompose prompt actually carried the leader's orchestration prose;
//   2. the parsed plan is a strict serial chain matching the leader's SOP
//      member schedule (each batch depends on the previous, roles in order).
func TestDecompose_FollowsLeaderStandardSOP(t *testing.T) {
	teamDir := t.TempDir()
	writeLeaderDefinition(t, teamDir, "software-team-lead",
		"---\nname: software-team-lead\ndescription: 软件开发团队 leader\n---",
		"## 标准SOP工作流\n\n用户需求 → 产品经理(PRD) → 架构师(系统设计+任务分解) → 工程师(代码实现) → QA工程师(测试验证)。")

	sopJSON := `[
      {"title":"PRD","description":"输出产品需求文档","role":"software-product-manager","verify_mode":"semantic","acceptance_criteria":["覆盖需求"],"batch_id":"1","batch_label":"需求","depends_on_batch":[],"depends_on_index":-1,"verifier_focus":"correctness","max_cycles":1},
      {"title":"系统设计","description":"输出系统设计","role":"software-architect","verify_mode":"semantic","acceptance_criteria":["接口对齐"],"batch_id":"2","batch_label":"设计","depends_on_batch":["1"],"depends_on_index":-1,"verifier_focus":"architecture","max_cycles":1},
      {"title":"代码实现","description":"实现功能","role":"software-engineer","verify_mode":"mechanical","acceptance_criteria":["编译通过"],"batch_id":"3","batch_label":"实现","depends_on_batch":["2"],"depends_on_index":-1,"max_cycles":1},
      {"title":"测试验证","description":"端到端验证","role":"software-qa-engineer","verify_mode":"semantic","acceptance_criteria":["测试通过"],"batch_id":"4","batch_label":"验证","depends_on_batch":["3"],"depends_on_index":-1,"verifier_focus":"correctness","max_cycles":1}
    ]`

	team := &TeamConfig{TeamDir: teamDir, Leader: TeamLeaderConfig{Role: "software-team-lead"}}
	capture := &capturingSpawner{resp: SubagentResponse{SessionID: "sess-1", Output: sopJSON, Success: true, ExitCode: 0}}
	runner := NewRunner(capture).WithTeam(team)
	p := NewPlanner(runner).WithTeam(team)

	tasks, _, err := p.DecomposeFull("开发一个网站", teamDir, 5*time.Second)
	if err != nil {
		t.Fatalf("decompose failed: %v", err)
	}

	// The prompt handed to the LLM must carry the leader's orchestration info.
	if !strings.Contains(capture.req.Task, "团队编排与工作流") {
		t.Error("decompose prompt did not inject leader orchestration section")
	}
	if !strings.Contains(capture.req.Task, "标准SOP工作流") {
		t.Error("decompose prompt did not include the leader 标准SOP definition")
	}

	if len(tasks) != 4 {
		t.Fatalf("got %d tasks, want 4", len(tasks))
	}

	// The plan must be a valid acyclic DAG forming a strict serial chain
	// (需求 → 设计 → 实现 → 验证), i.e. each batch sits one layer deeper.
	levels, err := dagLevels(tasks)
	if err != nil {
		t.Fatalf("invalid DAG: %v", err)
	}
	wantBatch := []string{"1", "2", "3", "4"}
	wantLevel := []int{0, 1, 2, 3}
	for i, b := range wantBatch {
		if levels[b] != wantLevel[i] {
			t.Errorf("batch %s level = %d, want %d (SOP serial chain)", b, levels[b], wantLevel[i])
		}
	}

	// Each batch's role matches the leader's member schedule order.
	wantRoles := []string{
		"software-product-manager",
		"software-architect",
		"software-engineer",
		"software-qa-engineer",
	}
	roleByBatch := make(map[string]string, len(tasks))
	for _, task := range tasks {
		roleByBatch[task.BatchID] = task.Role
	}
	for i, b := range wantBatch {
		if got := roleByBatch[b]; got != wantRoles[i] {
			t.Errorf("batch %s role = %q, want %q (leader SOP member order)", b, got, wantRoles[i])
		}
	}
}
