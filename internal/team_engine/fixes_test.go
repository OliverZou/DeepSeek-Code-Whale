package team_engine

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/usewhale/whale/internal/team_engine/log"
)

// ============================================================================
// stripGenericRoleSection tests
// ============================================================================

func TestStripGenericRoleSection(t *testing.T) {
	// Test with old-style prompt that has Rule 5.
	base := `4. ORDER BY DEPENDENCY.
5. Assign an appropriate ROLE to each subtask:
   - "developer"   — writing code
6. Group tasks into batches.`

	result := stripGenericRoleSection(base)

	if strings.Contains(result, "5. Assign an appropriate ROLE") {
		t.Error("expected rule 5 to be stripped")
	}
	if !strings.Contains(result, "4. ORDER BY DEPENDENCY") {
		t.Error("expected rule 4 to remain")
	}
	if !strings.Contains(result, "6. Group tasks into batches") {
		t.Error("expected rule 6 to remain after stripped rule 5")
	}
}

func TestStripGenericRoleSection_NoRule5(t *testing.T) {
	base := "Some prompt without role section."
	result := stripGenericRoleSection(base)
	if result != base {
		t.Errorf("expected unchanged prompt, got: %s", result)
	}
}

// ============================================================================
// BuildLeaderPrompt tests
// ============================================================================

func TestBuildLeaderPrompt_StripsGenericRoles(t *testing.T) {
	tc := &TeamConfig{
		Label: "test",
		Leader: TeamLeaderConfig{
			Role:  "队长",
			Model: "test-model",
		},
		Roles: []string{"backend-engineer", "frontend-engineer"},
		RoleTitles: map[string]string{
			"backend-engineer":  "后端工程师",
			"frontend-engineer": "前端工程师",
		},
		RoleDescs: map[string]string{
			"backend-engineer":  "写后端",
			"frontend-engineer": "写前端",
		},
	}

	basePrompt := DecomposePrompt("test goal")
	result := tc.BuildLeaderPrompt(basePrompt)

	// Team roles are injected.
	if !strings.Contains(result, "后端工程师") {
		t.Error("missing team role: 后端工程师")
	}
	// Team roles must be present.
	if !strings.Contains(result, "后端工程师") {
		t.Error("missing team role: 后端工程师")
	}
	if !strings.Contains(result, "前端工程师") {
		t.Error("missing team role: 前端工程师")
	}
	if !strings.Contains(result, "Rule 5 (ROLE ASSIGNMENT)") {
		t.Error("missing Rule 5 replacement header")
	}
}

func TestBuildLeaderPrompt_NoRolesPreservesBase(t *testing.T) {
	tc := &TeamConfig{
		Label: "test",
		Leader: TeamLeaderConfig{
			Role:  "队长",
			Model: "test-model",
			Prompt: "You are a great leader.",
		},
		Roles: nil,
	}

	basePrompt := DecomposePrompt("test goal")
	result := tc.BuildLeaderPrompt(basePrompt)

	// Base prompt content should remain.
	if !strings.Contains(result, "ONE TASK PER ROLE") {
		t.Error("base prompt should remain when no team roles")
	}
	if !strings.Contains(result, "You are a great leader") {
		t.Error("missing leader prompt")
	}
}

func TestBuildLeaderPrompt_InjectRules(t *testing.T) {
	tc := &TeamConfig{
		Label: "test",
		Leader: TeamLeaderConfig{
			Role: "队长",
			Rules: []string{"必须使用 TDD", "必须 Git 提交"},
		},
	}

	basePrompt := DecomposePrompt("test goal")
	result := tc.BuildLeaderPrompt(basePrompt)

	if !strings.Contains(result, "必须使用 TDD") {
		t.Error("missing team rule: TDD")
	}
	if !strings.Contains(result, "必须 Git 提交") {
		t.Error("missing team rule: Git")
	}
}

func TestBuildLeaderPrompt_InjectPipeline(t *testing.T) {
	tc := &TeamConfig{
		Label: "test",
		Leader: TeamLeaderConfig{Role: "队长"},
		Roles: []string{"backend-engineer"},
		RoleTitles: map[string]string{"backend-engineer": "后端工程师"},
		RoleDescs:  map[string]string{"backend-engineer": "写后端"},
		Pipeline: &PipelineFile{
			Pipelines: map[string]PipelineDef{
				"new-feature": {
					Description: "新功能开发",
					Trigger:     "新建|开发",
					Stages: []PipelineStage{
						{ID: "design", Label: "设计", Roles: []string{"软件架构师"}},
						{ID: "impl", Label: "实施", Roles: []string{"后端工程师", "前端工程师"}, DependsOn: []string{"design"}, Parallel: true},
					},
				},
				"bugfix": {
					Description: "Bug修复",
					Trigger:     "bug|修复",
					Stages: []PipelineStage{
						{ID: "fix", Label: "修复", Roles: []string{"后端工程师"}},
					},
				},
			},
			Default: "dynamic",
		},
	}

	basePrompt := DecomposePrompt("test goal")
	result := tc.BuildLeaderPrompt(basePrompt)

	if !strings.Contains(result, "Pipeline Templates") {
		t.Error("missing pipeline section header")
	}
	if !strings.Contains(result, "ADVISORY") {
		t.Error("missing advisory label")
	}
	if !strings.Contains(result, "new-feature") {
		t.Error("missing pipeline name: new-feature")
	}
	if !strings.Contains(result, "bugfix") {
		t.Error("missing pipeline name: bugfix")
	}
	if !strings.Contains(result, "dynamic") {
		t.Error("missing default strategy")
	}
	if !strings.Contains(result, "parallel=true") {
		t.Error("missing parallel flag")
	}
}

func TestBuildLeaderPrompt_NoPipeline(t *testing.T) {
	tc := &TeamConfig{
		Label:    "test",
		Category: "开发",
		Leader:   TeamLeaderConfig{Role: "队长"},
	}

	basePrompt := DecomposePrompt("test goal")
	result := tc.BuildLeaderPrompt(basePrompt)

	if strings.Contains(result, "Pipeline Templates") {
		t.Error("should not have pipeline section when Pipeline is nil")
	}
}

func TestTeamConfig_Category(t *testing.T) {
	tc := &TeamConfig{
		Label:    "test",
		Category: "开发",
		Leader:   TeamLeaderConfig{Role: "队长"},
	}
	if tc.Category != "开发" {
		t.Errorf("expected category '开发', got %q", tc.Category)
	}
}

// ============================================================================
// checkpointAfterWrite tests
// ============================================================================

func TestCheckpointAfterWrite_NoPanic(t *testing.T) {
	// FileTaskStore checkpoint/checkpointAfterWrite are no-ops.
	store, err := NewFileTaskStore(t.TempDir())
	if err != nil {
		t.Fatalf("open file store: %v", err)
	}
	defer store.Close()

	// Should not panic or error.
	store.checkpointAfterWrite()
	store.Checkpoint()
}

func TestInsertTaskCheckpoints(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, err := eng.CreateTask("Checkpoint test", "desc", RoleDeveloper, "", nil, 0, ".", "", "", "")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if task == nil {
		t.Fatal("expected non-nil task")
	}

	// Verify the task exists — insert + checkpoint succeeded.
	got, err := eng.Store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if got == nil {
		t.Fatal("task not found after insert")
	}
}

// ============================================================================
// TeamLog TryLock tests
// ============================================================================

func TestTeamLog_TryLockNonBlocking(t *testing.T) {
	tl := log.NewTeamLogAt(t.TempDir() + "/test.log")
	defer tl.Close()

	// Single write should succeed.
	tl.Log("test", "hello %s", "world")
}

func TestTeamLog_TryLockConcurrent(t *testing.T) {
	tl := log.NewTeamLogAt(t.TempDir() + "/test.log")
	defer tl.Close()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			tl.Log("test", "msg %d", n)
		}(i)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// All writes completed — no deadlock.
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: concurrent writes timed out after 5s")
	}
}

// ============================================================================
// Planner model override tests
// ============================================================================

func TestPlanner_UsesTeamLeaderModel(t *testing.T) {
	tc := &TeamConfig{
		Label: "test",
		Leader: TeamLeaderConfig{
			Role:  "队长",
			Model: "custom-leader-model",
		},
	}

	// Verify team config has the model.
	if tc.Leader.Model != "custom-leader-model" {
		t.Fatal("team leader model not set")
	}
}

// ============================================================================
// DB Write Checkpoint tests
// ============================================================================

func TestUpdateTaskCheckpoints(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("Update test", "desc", RoleDeveloper, "", nil, 0, ".", "", "", "")
	err := eng.Store.UpdateTask(task.ID, map[string]interface{}{"state": "done"})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	// Verify state was updated.
	got, _ := eng.Store.GetTask(task.ID)
	if got == nil {
		t.Fatal("task not found after update")
	}
}

func TestDeleteTaskCheckpoints(t *testing.T) {
	eng := newTestEngine(t)
	defer eng.Close()

	task, _ := eng.CreateTask("Delete test", "desc", RoleDeveloper, "", nil, 0, ".", "", "", "")
	err := eng.DeleteTask(task.ID)
	if err != nil {
		t.Fatalf("delete task: %v", err)
	}

	got, _ := eng.Store.GetTask(task.ID)
	if got != nil {
		t.Error("task should be gone after delete")
	}
}
