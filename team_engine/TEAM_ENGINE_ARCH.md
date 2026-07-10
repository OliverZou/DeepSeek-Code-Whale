# Team Engine — Whale Integration Architecture

## Overview

Team Engine implements the MiniMax Agent Team Leader-Worker-Verifier
collaboration model as a state-machine-driven multi-agent orchestration
runtime, integrated into Whale.

Unlike the original [team-engine-go](https://github.com/myai/team-engine-go)
which calls external agent CLIs (Claude Code, OpenCode, Codex), this
integration uses Whale's own subagent spawning mechanism for all three
roles, giving full control over agent behavior, tool permissions, and
execution context.

## Architecture

```
┌──────────────────────────────────────────────────────────┐
│                    Whale CLI (whale team)                  │
│  internal/ui/cli/cmd/team_cmd.go                          │
│  ┌───────────┐ ┌──────────┐ ┌─────────┐ ┌────────────┐  │
│  │ create    │ │ run      │ │ plan    │ │ status     │  │
│  │ cancel    │ │ feedback │ │ list    │ │            │  │
│  └─────┬─────┘ └────┬─────┘ └────┬────┘ └──────┬─────┘  │
│        └──────────────┴───────────┴─────────────┘        │
│                          │                                │
│                 newTeamEngine()                           │
└──────────────────────────┬───────────────────────────────┘
                           │
┌──────────────────────────▼───────────────────────────────┐
│                   TeamEngine (internal/team_engine/)      │
│                                                           │
│  ┌────────────────────────────────────────────────────┐  │
│  │              team_engine.go (Engine)                │  │
│  │  ┌─────────┐  ┌──────────┐  ┌──────────────────┐  │  │
│  │  │CreateTk│  │ RunTask  │  │ PlanAndRun       │  │  │
│  │  │Cancel  │  │ Feedback │  │ RunPipeline      │  │  │
│  │  └────┬────┘  └────┬─────┘  └────────┬─────────┘  │  │
│  │       └────────────┴─────────────────┘             │  │
│  └────────────────────────────────────────────────────┘  │
│                                                           │
│  ┌──────────┐ ┌────────┐ ┌────────┐ ┌─────────────────┐  │
│  │ models   │ │ config │ │ db     │ │ whiteboard      │  │
│  │ (task,   │ │ (yaml) │ │(sqlite)│ │ (file system)   │  │
│  │  state,  │ │        │ │        │ │ input/output/   │  │
│  │  role)   │ │        │ │        │ │ artifacts       │  │
│  └──────────┘ └────────┘ └────────┘ └─────────────────┘  │
│                                                           │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐                 │
│  │ router   │ │ leader   │ │ verifier │                 │
│  │(profile) │ │(AI plan) │ │(advers.) │                 │
│  └────┬─────┘ └────┬─────┘ └────┬─────┘                 │
│       └────────────┴────────────┴───┐                    │
│                                      │                    │
│  ┌───────────────────────────────────▼────────────────┐  │
│  │              runner.go (AgentRunner)                │  │
│  │  SubagentSpawner interface                          │  │
│  └───────────────────────┬────────────────────────────┘  │
└──────────────────────────┼───────────────────────────────┘
                           │
┌──────────────────────────▼───────────────────────────────┐
│              SubagentSpawner Implementation               │
│                                                           │
│  ┌─────────────────────┐  ┌──────────────────────────┐   │
│  │ ShellSubagentSpawner│  │ WhaleNativeSpawner       │   │
│  │ (spawner.go)        │  │ (future: tasks.Runner)   │   │
│  │ calls "whale exec"  │  │ direct Go API            │   │
│  └─────────────────────┘  └──────────────────────────┘   │
└───────────────────────────────────────────────────────────┘
```

## State Machine

```
                    ┌──────────┐
                    │  pending │  ← 用户创建任务
                    └────┬─────┘
                         │ engine.assign()
                    ┌────▼─────┐
                    │ assigned │  ← 已分配 Worker 角色
                    └────┬─────┘
                         │ runner.spawn()
                    ┌────▼──────┐
                    │ producing │  ← Whale subagent 执行中
                    └────┬──────┘
                         │ Worker 完成，产出写入白板
                    ┌────▼──────┐
                    │ produced  │  ← 等待 Verifier 检查
                    └────┬──────┘
                         │ verifier.check()
                    ┌────▼───────┐
                    │ verifying  │  ← Verifier subagent 运行中
                    └────┬───────┘
                         │
              ┌──────────┴──────────┐
              │ PASS                │ FAIL (retry < max)
         ┌────▼────┐         ┌─────▼─────┐
         │verified │         │ producing │ ← 回到 Worker 修复
         └────┬────┘         └───────────┘
              │
              │ FAIL (retry >= max) → failed
              │
         ┌────▼────┐
         │  done   │  ← 最终状态
         └─────────┘
```

## Key Design Decisions

### 1. Tool Profiles Instead of External CLIs

The original team-engine-go selected an external CLI backend
(cc/opencode/codex) for each task.  This integration replaces that with
**Tool Profiles** that map to Whale tool permission sets:

| Profile     | Allowed Tools                                                | Use Case          |
|-------------|--------------------------------------------------------------|-------------------|
| `default`   | read, write, edit, apply_patch, shell_run, shell_wait        | Developer/Tester  |
| `read_only` | read, grep, search, web_search, fetch                        | Reviewer          |
| `research`  | read, web_search, fetch                                      | Researcher        |
| `content`   | read, write, edit, apply_patch                               | Writer/Formatter  |
| `test`      | read, write, edit, shell_run                                 | Tester            |

### 2. SubagentSpawner Interface

All agent execution goes through the `SubagentSpawner` interface:

```go
type SubagentSpawner interface {
    SpawnSubagent(ctx context.Context, req SubagentRequest) (SubagentResponse, error)
}
```

Two implementations provided:
- **`ShellSubagentSpawner`** — Calls `whale exec` as a subprocess
- **`WhaleSpawnerAdapter`** — Adapter for direct `tasks.Runner` integration

### 3. File-Based Whiteboard

Agents communicate via file system (whiteboard):

```
team_tasks/<task_id>/
├── input.md        # Task description (written by engine)
├── output.md       # Worker output (written by worker)
├── verifier.md     # Verifier result (written by verifier)
├── status.json     # Current state metadata
└── artifacts/      # Worker-produced files
```

### 4. SQLite Persistence

Task state is persisted in SQLite (via modernc.org/sqlite, pure Go, no CGO).
Use `:memory:` for testing, file path for production.

## File Map

| File | Purpose | Origin |
|------|---------|--------|
| `internal/team_engine/models.go` | Task, TaskState, AgentRole data models | Ported from team-engine-go |
| `internal/team_engine/config.go` | YAML config loading with defaults | Ported (simplified) |
| `internal/team_engine/db.go` | SQLite persistence layer | Ported from team-engine-go |
| `internal/team_engine/whiteboard.go` | File-based inter-agent communication | Ported from team-engine-go |
| `internal/team_engine/runner.go` | AgentRunner + SubagentSpawner interface | **Rewritten for Whale** |
| `internal/team_engine/router.go` | Tool profile routing | **Simplified for Whale** |
| `internal/team_engine/leader.go` | AI-powered task decomposition | Ported (uses Whale subagent) |
| `internal/team_engine/verifier.go` | Adversarial output verification | Ported (uses Whale subagent) |
| `internal/team_engine/team_engine.go` | Core state machine orchestrator | Ported from team-engine-go |
| `internal/team_engine/spawner.go` | SubagentSpawner implementations | **New for Whale** |
| `internal/team_engine/team_engine.yaml` | Default configuration | **New for Whale** |
| `internal/ui/cli/cmd/team_cmd.go` | `whale team` CLI subcommand | **New for Whale** |

## CLI Usage

```bash
# Create a task
whale team create --title "Build API" --description "Create a REST API" --role developer

# Run a task
whale team run <task-id>

# List all tasks
whale team list

# Check task status
whale team status <task-id>

# Decompose a goal and run subtasks
whale team plan --goal "Build a complete web application"

# Send feedback
whale team feedback <task-id> "Please add input validation"
```

## Future Integration Points

1. **Deep Integration** — Replace `ShellSubagentSpawner` with a direct
   `tasks.Runner.SpawnSubagentWithProgress` call for better performance
   and real-time streaming

2. **Tool Registration** — Register Team Engine tools in
   `internal/tools/catalog.go` so they appear in Whale's TUI tool list

3. **Workflow Integration** — Create a Whale workflow script that uses
   Team Engine for multi-agent orchestration

4. **TUI Dashboard** — Add a Team Engine tab in Whale's TUI showing
   task status, progress, and agent outputs
