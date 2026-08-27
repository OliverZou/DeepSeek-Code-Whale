package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/usewhale/whale/internal/app"
	"github.com/usewhale/whale/internal/core"
	"github.com/usewhale/whale/internal/defaults"
	"github.com/usewhale/whale/internal/llm"
	"github.com/usewhale/whale/internal/llm/deepseek"
	"github.com/usewhale/whale/internal/store"
	"github.com/usewhale/whale/internal/tasks"
	"github.com/usewhale/whale/internal/team_engine"
	teampglog "github.com/usewhale/whale/internal/team_engine/log"
	"github.com/usewhale/whale/internal/tools"
	whaleworktree "github.com/usewhale/whale/internal/worktree"
)

func newTeamCmd() *cobra.Command {
	var (
		dbPath        string
		whiteboardDir string
		configPath    string
		workdir       string
		maxRetries    int
		role          string
		profile       string
		verifierFocus string
	)

	teamCmd := &cobra.Command{
		Use:   "team",
		Short: "Team Engine: multi-agent orchestration (Leader-Worker-Verifier)",
		Long: `Team Engine implements the MiniMax Agent Team Leader-Worker-Verifier
collaboration model using Whale subagents. It decomposes complex goals
into subtasks, executes them through specialized worker agents, and
validates outputs through adversarial verification.

Subcommands:
  create    Create a new task
  run       Run a task through produce→verify→done lifecycle
  plan      Decompose a goal and run all subtasks
  status    Show task status
  list      List all tasks
  cancel    Cancel a task
  feedback  Send human feedback to a task`,
	}

	// Global flags for the team engine.
	teamCmd.PersistentFlags().StringVar(&dbPath, "db", ".whale/team_engine.db", "Path to SQLite database")
	teamCmd.PersistentFlags().StringVar(&whiteboardDir, "whiteboard", ".whale/team_tasks", "Whiteboard directory for agent communication")
	teamCmd.PersistentFlags().StringVar(&configPath, "config", "", "Path to team_engine.yaml config")
	// workdir 必填：team 状态目录/交付目录的位置必须显式指定——默认回退到
	// 启动目录会让 .whale/team_tasks 落到 whale.exe 所在目录/任意 cwd。
	teamCmd.PersistentFlags().StringVar(&workdir, "workdir", "", "Working directory for agent execution (required)")
	// Resolve paths at the command root so every subcommand consumes the same
	// absolute values. newTeamEngine repeats this (idempotent), but paths that
	// bypass it — e.g. RunLeaderDriven's workdir, which becomes task.Workdir —
	// would otherwise keep the raw default "." and make the verify phase
	// resolve against the engine process cwd.
	teamCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		// 只读查询命令（status/list/analyze/history）按 master UUID 用全局运行
		// 索引解析工作区，允许不传 --workdir；其余命令（创建/执行/反馈）
		// 必须显式指定。注意顺序：workdir 为空时绝不能先绝对化成 cwd
		// （Join(cwd, "") = cwd 会吞掉 UUID 解析），路径解析只在 workdir
		// 非空时进行。
		queryOnly := map[string]bool{"status": true, "list": true, "analyze": true, "history": true}
		if !queryOnly[cmd.Name()] && strings.TrimSpace(workdir) == "" {
			return errors.New("--workdir is required: team state and deliverables need an explicit workspace (e.g. --workdir C:\\projects\\my-app)")
		}
		if strings.TrimSpace(workdir) != "" {
			if !filepath.IsAbs(workdir) {
				cwd, _ := os.Getwd()
				workdir = filepath.Join(cwd, workdir)
			}
			if !filepath.IsAbs(whiteboardDir) {
				whiteboardDir = filepath.Join(workdir, whiteboardDir)
			}
			// db 默认路径同样跟随 workdir（而非 cwd）——否则从 exe 目录启动时
			// team_engine.db 会落在程序目录。
			if !filepath.IsAbs(dbPath) {
				dbPath = filepath.Join(workdir, dbPath)
			}
		}
		return nil
	}

	// resolveQueryWorkdir: 只读查询按 master UUID 用全局运行索引解析工作区——
	// status/analyze/history 不必重复传 --workdir（master 是全局唯一键）。
	resolveQueryWorkdir := func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(workdir) != "" {
			return nil
		}
		if len(args) == 0 {
			return errors.New("--workdir is required for this command (no master id to resolve from)")
		}
		wd, ok := team_engine.ResolveRunIndex(args[0])
		if !ok {
			return fmt.Errorf("no workspace index for %q — pass --workdir once (the index is written on each new team run)", args[0])
		}
		workdir = wd
		whiteboardDir = filepath.Join(wd, ".whale", "team_tasks")
		return nil
	}

	// --- create subcommand ---
	createCmd := &cobra.Command{
		Use:   "create --title TITLE --description DESC [--role ROLE] [--profile PROFILE]",
		Short: "Create a new task",
		RunE: func(cmd *cobra.Command, args []string) error {
			title, _ := cmd.Flags().GetString("title")
			description, _ := cmd.Flags().GetString("description")

			if title == "" || description == "" {
				return fmt.Errorf("--title and --description are required")
			}

			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath, workdir)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			task, err := eng.CreateTask(
				title, description,
				team_engine.AgentRole(role),
				team_engine.ToolProfile(profile),
				nil, maxRetries, workdir, verifierFocus, "", "",
			)
			if err != nil {
				return fmt.Errorf("create task: %w", err)
			}

			fmt.Printf("✅ Task created: %s\n", task.ID)
			fmt.Printf("   Title: %s\n", task.Title)
			fmt.Printf("   Role:  %s\n", task.Role)
			fmt.Printf("   State: %s\n", task.State)
			return nil
		},
	}
	createCmd.Flags().String("title", "", "Task title")
	createCmd.Flags().String("description", "", "Task description/prompt")
	createCmd.Flags().StringVar(&role, "role", string(team_engine.RoleDeveloper), "Agent role (developer|tester|reviewer|researcher|writer|formatter|evaluator)")
	createCmd.Flags().StringVar(&profile, "profile", "", "Tool profile (default|read_only|research|content|test)")
	createCmd.Flags().StringVar(&verifierFocus, "verifier-focus", "", "Verification focus (correctness|security|completeness|sources|plausibility)")
	createCmd.Flags().IntVar(&maxRetries, "max-retries", 9, "Maximum verification retries")

	// --- run subcommand ---
	runCmd := &cobra.Command{
		Use:   "run <task-id>",
		Short: "Run a task through the full produce→verify→done lifecycle",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath, workdir)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			// Opt-in git worktree isolation: enabled only when --worktree is
			// passed and the working directory is inside a git repository.
			if useWorktree, _ := cmd.Flags().GetBool("worktree"); useWorktree {
				repoRoot, err := whaleworktree.CheckoutRoot(workdir)
				if err != nil {
					return fmt.Errorf("--worktree requires a git repository: %w", err)
				}
				eng.EnableWorktree(repoRoot)
			}

			taskID := args[0]
			fmt.Printf("🚀 Running task %s...\n", taskID)

			success, err := eng.RunTask(context.Background(), taskID)
			if err != nil {
				return fmt.Errorf("run task: %w", err)
			}

			task, _ := eng.GetTask(taskID)
			if task != nil {
				fmt.Printf("   State: %s\n", task.State)
				if output, _ := eng.Whiteboard.ReadOutput(taskID); output != "" {
					preview := output
					if len(preview) > 500 {
						preview = preview[:500] + "..."
					}
					fmt.Printf("   Output preview:\n%s\n", preview)
				}
			}

			if success {
				fmt.Println("✅ Task completed successfully")
			} else {
				fmt.Println("❌ Task failed after exhausting retries")
			}
			return nil
		},
	}
	runCmd.Flags().Bool("worktree", false, "Use git worktree isolation for coding tasks")

	// --- spec subcommand ---
	initCmd := &cobra.Command{
		Use:   "init --goal GOAL [--team TEAM]",
		Short: "Elaborate a goal into a detailed spec; no decomposition, no execution",
		RunE: func(cmd *cobra.Command, args []string) error {
			goal, _ := cmd.Flags().GetString("goal")
			if goal == "" {
				return fmt.Errorf("--goal is required")
			}
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath, workdir)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()
			// Use fast in-process LLM calls for elaboration (no subprocess).
			if model, _ := cmd.Flags().GetString("model"); model != "" {
				if lite := newLiteSpawner(model); lite != nil {
					eng.Runner.SetLiteSpawner(lite)
				}
			}
			if teamName, _ := cmd.Flags().GetString("team"); teamName != "" {
				roots := team_engine.DefaultTeamRoots(workdir)
				tc, err := team_engine.FindTeamInRoots(roots, teamName)
				if err != nil {
					return fmt.Errorf("load team %q: %w", teamName, err)
				}
				eng.SetTeam(tc)
			}
			leader := team_engine.NewLeader(eng.Runner).WithTeam(eng.Team())
			start := time.Now()
			elaborated, err := leader.Elaborate(goal, workdir, 120*time.Second)
			dur := time.Since(start)
			if err != nil {
				return fmt.Errorf("elaborate: %w", err)
			}
			specPath := filepath.Join(whiteboardDir, "spec.md")
			if elaborated == goal {
				fmt.Printf("✅ Goal already fully specified (%.1fs)\n", dur.Seconds())
			} else {
				os.WriteFile(specPath, []byte(elaborated), 0644)
				fmt.Printf("📋 Elaborated Spec → %s (%.1fs)\n", specPath, dur.Seconds())
			}
			return nil
		},
	}
	initCmd.Flags().String("goal", "", "The goal to elaborate")
	initCmd.Flags().String("team", "", "Team name for domain context")

	// --- plan subcommand ---
	executeCmd := &cobra.Command{
		Use:   "execute --goal GOAL [--team TEAM]",
		Short: "Decompose a goal and execute all subtasks in parallel",
		RunE: func(cmd *cobra.Command, args []string) error {
			goal, _ := cmd.Flags().GetString("goal")
			if goal == "" {
				return fmt.Errorf("--goal is required")
			}

			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath, workdir)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			// Opt-in git worktree isolation: enabled only when --worktree is
			// passed and the working directory is inside a git repository.
			if useWorktree, _ := cmd.Flags().GetBool("worktree"); useWorktree {
				repoRoot, err := whaleworktree.CheckoutRoot(workdir)
				if err != nil {
					return fmt.Errorf("--worktree requires a git repository: %w", err)
				}
				eng.EnableWorktree(repoRoot)
			}

			// Optional team configuration.
			if teamName, _ := cmd.Flags().GetString("team"); teamName != "" {
				roots := team_engine.DefaultTeamRoots(workdir)
				tc, err := team_engine.FindTeamInRoots(roots, teamName)
				if err != nil {
					return fmt.Errorf("load team %q: %w", teamName, err)
				}
				eng.SetTeam(tc)
				fmt.Printf("👥 Team: %s (%d roles)\n", tc.Label, len(tc.Roles))
			}

			fmt.Printf("📋 Planning goal: %s\n", goal)

			// Elaborate + decompose are pure one-shot LLM calls — run them
			// through the in-process lite spawner (plain text response, no
			// subagent loop). Workers/verifiers still use the native subagent
			// adapter wired in newTeamEngine, which is what restores AgentName.
			if lite := newLiteSpawner("deepseek-v4-flash"); lite != nil {
				eng.Runner.SetLiteSpawner(lite)
			}
			// 一次 team 任务全程一个 TeamEngine：把本会话的引擎注册为活动执行
			// 引擎，leader 的 team_run_plan 工具复用同一实例执行 batch，而不是
			// 每次重建（重建会丢 lite/team 配置、双实例各记各的 token）。
			// 生命周期仍归本会话（defer Close），工具路径不会关闭复用引擎。
			team_engine.SetDefaultRunEngine(eng)
			defer team_engine.ClearDefaultRunEngine()

			// --plan-file: reuse an existing decompose (verbatim plan; the
			// Leader is bootstrapped, no LLM decomposition of the goal).
			planFile, _ := cmd.Flags().GetString("plan-file")
			var planOptions []team_engine.LeaderDrivenOption
			if planFile != "" {
				complexity, planTasks, err := team_engine.LoadPlanFile(planFile)
				if err != nil {
					return fmt.Errorf("load plan file: %w", err)
				}
				fmt.Printf("📋 Using plan file: %s (%d tasks, complexity=%s)\n", planFile, len(planTasks), complexity)
				planOptions = append(planOptions, team_engine.WithPreDecomposedPlan(planTasks, complexity))
			}

			stopAt := strings.ToLower(strings.TrimSpace(cmd.Flag("stop-at").Value.String()))
			if stopAt == "spec" || stopAt == "decompose" {
				leader := team_engine.NewLeader(eng.Runner).WithTeam(eng.Team())
				start := time.Now()
				elaborated, err := leader.Elaborate(goal, workdir, 120*time.Second)
				if err != nil {
					return fmt.Errorf("elaborate: %w", err)
				}
				if stopAt == "spec" {
					specPath := filepath.Join(whiteboardDir, "spec.md")
					if elaborated == goal {
						fmt.Printf("✅ Goal already fully specified (%.1fs)\n", time.Since(start).Seconds())
					} else {
						os.WriteFile(specPath, []byte(elaborated), 0644)
						fmt.Printf("📋 Elaborated Spec → %s (%.1fs)\n", specPath, time.Since(start).Seconds())
					}
					return nil
				}
				planTasks, rawJSON, err := leader.DecomposeFull(elaborated, workdir, time.Duration(eng.Router.ResolveDecomposerTimeout())*time.Second)
				if err != nil {
					return fmt.Errorf("decompose: %w", err)
				}
				// Write plan.md and plan.json.
				specPath := filepath.Join(whiteboardDir, "spec.md")
				if elaborated != goal {
					os.WriteFile(specPath, []byte(elaborated), 0644)
				}
				planPath := filepath.Join(whiteboardDir, "plan.md")
				var md strings.Builder
				md.WriteString(fmt.Sprintf("# 项目计划\n\n## 目标\n\n%s\n\n## 任务列表 (%d)\n\n", goal, len(planTasks)))
				for i, pt := range planTasks {
					md.WriteString(fmt.Sprintf("%d. **%s** (%s)\n   %s\n\n", i+1, pt.Title, pt.Role, pt.Description))
				}
				os.WriteFile(planPath, []byte(md.String()), 0644)
				jsonPath := filepath.Join(whiteboardDir, "plan.json")
				planJSON, _ := json.MarshalIndent(planTasks, "", "  ")
				os.WriteFile(jsonPath, planJSON, 0644)
				fmt.Printf("📋 Decomposed %d tasks in %.1fs\n   spec → %s\n   plan → %s\n%s\n",
					len(planTasks), time.Since(start).Seconds(), specPath, planPath, rawJSON)
				return nil
			}

			masterTask, mtErr := eng.CreateMasterTask(goal, workdir, "")
			if mtErr != nil {
				return fmt.Errorf("create master task: %w", mtErr)
			}

			// Subscribe to engine events for real-time progress output.
			cancel := eng.OnEvent(func(event team_engine.TaskEvent) {
				switch event.Type {
				case team_engine.EventStateChanged:
					if event.TaskID != "" {
						icon := statusIcon(team_engine.TaskState(event.NewState))
						fmt.Fprintf(cmd.ErrOrStderr(), "  %s %s: %s -> %s\n",
							icon, event.TaskID[:8], event.OldState, event.NewState)
					}
				case team_engine.EventTaskDone:
					fmt.Fprintf(cmd.ErrOrStderr(), "  done %s\n", event.TaskID[:8])
				case team_engine.EventAgentLog:
					fmt.Fprint(cmd.ErrOrStderr(), ".")
				}
			})
			defer cancel()

			report, err := eng.RunLeaderDriven(cmd.Context(), goal, workdir, masterTask.ID, planOptions...)
			if err != nil {
				return fmt.Errorf("plan and run: %w", err)
			}

			// Token 汇总:raw 是重放体积,effective(= miss + completion + hit/31,
			// DeepSeek 峰时价)才是真实账单量级;命中率说明重放占比。
			if hit, miss, completion := eng.UsageSplit(); hit+miss+completion > 0 {
				eff := eng.EffectiveTokens()
				hitRate := 0.0
				if hit+miss > 0 {
					hitRate = 100 * float64(hit) / float64(hit+miss)
				}
				fmt.Printf("\n⚡ Token: raw=%d (hit=%d miss=%d completion=%d, 缓存命中 %.0f%%) · effective≈%d\n",
					eng.TokenTotal(), hit, miss, completion, hitRate, eff)
			}

			fmt.Printf("\n📊 Final report:\n%s\n", report)
			return nil
		},
	}
	executeCmd.Flags().String("goal", "", "The goal to decompose into subtasks")
	executeCmd.Flags().String("team", "", "Team name to use for decomposition")
	executeCmd.Flags().String("model", "", "Model override for the Leader decomposition step")
	executeCmd.Flags().Bool("worktree", false, "Use git worktree isolation for coding tasks")
	executeCmd.Flags().String("stop-at", "", "Stop early: 'spec' (elaborate only) or 'decompose' (plan only)")
	executeCmd.Flags().String("plan-file", "", "Reuse an existing plan.json verbatim (Leader is bootstrapped without LLM decomposition; --goal still required as the run title)")

	// --- analyze subcommand ---
	analyzeCmd := &cobra.Command{
		Use:   "analyze MASTER_ID",
		Short: "Analyze a finished run (run_report + leader review trail)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := resolveQueryWorkdir(cmd, args); err != nil {
				return err
			}
			return analyzeRun(whiteboardDir, args[0])
		},
	}

	// --- status subcommand ---
	statusCmd := &cobra.Command{
		Use:   "status [task-id]",
		Short: "Show task status (all tasks, or one task by ID)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath, workdir)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			jsonOutput, _ := cmd.Flags().GetBool("json")

			if err := resolveQueryWorkdir(cmd, args); err != nil {
				return err
			}

			if len(args) == 1 {
				task, err := eng.GetTask(args[0])
				if err != nil {
					return fmt.Errorf("get task: %w", err)
				}
				if task == nil {
					return fmt.Errorf("task %q not found", args[0])
				}
				printTask(task, jsonOutput)
			} else {
				tasks, err := eng.ListTasks()
				if err != nil {
					return fmt.Errorf("list tasks: %w", err)
				}
				if jsonOutput {
					enc := json.NewEncoder(os.Stdout)
					enc.SetIndent("", "  ")
					enc.Encode(tasks)
				} else {
					fmt.Printf("📊 Tasks (%d total):\n\n", len(tasks))
					for _, t := range tasks {
						pct := team_engine.GetProgress(t.State)
						icon := statusIcon(t.State)
						fmt.Printf("  %s %s [%s] %3d%% %s\n", icon, t.ID[:8], t.State, pct, t.Title)
					}
				}
			}
			return nil
		},
	}
	statusCmd.Flags().Bool("json", false, "Output as JSON")

	// --- list subcommand (alias for status without args) ---
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List all tasks",
		RunE: func(cmd *cobra.Command, args []string) error {
			return statusCmd.RunE(cmd, nil)
		},
	}

	// --- cancel subcommand ---
	cancelCmd := &cobra.Command{
		Use:   "cancel <task-id>",
		Short: "Cancel a running task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath, workdir)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			if err := eng.CancelTask(args[0]); err != nil {
				return fmt.Errorf("cancel task: %w", err)
			}
			fmt.Printf("✅ Task %s cancelled\n", args[0])
			return nil
		},
	}

	// --- feedback subcommand ---
	feedbackCmd := &cobra.Command{
		Use:   "feedback <task-id> <message>",
		Short: "Send human feedback to a task",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath, workdir)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			if err := eng.SendFeedback(args[0], args[1]); err != nil {
				return fmt.Errorf("send feedback: %w", err)
			}
			fmt.Printf("✅ Feedback sent to task %s\n", args[0])
			return nil
		},
	}

	// --- resolve subcommand — resolve a pending escalation ---
	resolveCmd := &cobra.Command{
		Use:   "resolve <batch-id> <decision>",
		Short: "Resolve a pending escalation (continue|retry|abort|modify)",
		Long:  `Resolve a pending escalation for a batch. Valid decisions: continue, retry, abort, modify.`,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath, workdir)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			decision := team_engine.EscalationDecision(args[1])
			switch decision {
			case team_engine.EscalationContinue, team_engine.EscalationRetry,
				team_engine.EscalationAbort, team_engine.EscalationModify:
				// valid
			default:
				return fmt.Errorf("invalid decision %q: use continue|retry|abort|modify", args[1])
			}

			if err := eng.ResolveEscalation(args[0], decision); err != nil {
				return fmt.Errorf("resolve escalation: %w", err)
			}
			fmt.Printf("✅ Escalation %s resolved with decision: %s\n", args[0], decision)
			return nil
		},
	}

	// --- escalation subcommand — show pending escalations ---
	escalationCmd := &cobra.Command{
		Use:   "escalation",
		Short: "List pending escalations",
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath, workdir)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			pending := eng.Escalation.ListPending()
			if len(pending) == 0 {
				fmt.Println("No pending escalations.")
				return nil
			}
			fmt.Printf("🚨 Pending escalations (%d):\n", len(pending))
			for _, batchID := range pending {
				fmt.Printf("  - Batch %s\n", batchID)
				fmt.Printf("    Resolve: whale team resolve %s continue|retry|abort|modify\n", batchID)
			}
			return nil
		},
	}

	// --- prompt subcommand — append a turn to a member session (prompt primitive) ---
	promptCmd := &cobra.Command{
		Use:   "prompt <task-id> <message>",
		Short: "Send a message to a team member's session and wait for its reply",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath, workdir)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			reply, err := eng.Prompt(cmd.Context(), team_engine.PromptRequest{
				ToTaskID: args[0],
				From:     "human",
				Content:  args[1],
				Sync:     true,
			})
			if err != nil {
				return fmt.Errorf("prompt: %w", err)
			}
			if reply == nil {
				fmt.Println("Message delivered (no reply).")
				return nil
			}
			fmt.Println(reply.Content)
			return nil
		},
	}

	// --- spawn subcommand — create a new member task (spawn primitive) ---
	spawnCmd := &cobra.Command{
		Use:   "spawn --title TITLE --description DESC [--role ROLE] [--workdir DIR]",
		Short: "Spawn a new team member task",
		RunE: func(cmd *cobra.Command, args []string) error {
			spawnTitle, _ := cmd.Flags().GetString("title")
			spawnDesc, _ := cmd.Flags().GetString("description")
			spawnRole, _ := cmd.Flags().GetString("role")
			spawnWorkdir, _ := cmd.Flags().GetString("workdir")
			spawnMaxRetries, _ := cmd.Flags().GetInt("max-retries")
			if strings.TrimSpace(spawnTitle) == "" || strings.TrimSpace(spawnDesc) == "" {
				return fmt.Errorf("--title and --description are required")
			}
			if spawnRole == "" {
				spawnRole = "worker"
			}
			if spawnMaxRetries <= 0 {
				spawnMaxRetries = 3
			}

			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath, workdir)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			task, err := eng.Spawn(cmd.Context(), team_engine.SpawnRequest{
				Title:       spawnTitle,
				Description: spawnDesc,
				Role:        team_engine.AgentRole(spawnRole),
				MaxRetries:  spawnMaxRetries,
				Workdir:     spawnWorkdir,
				From:        "human",
			})
			if err != nil {
				return fmt.Errorf("spawn: %w", err)
			}
			fmt.Printf("✅ Spawned task %s: %s [%s]\n", task.ID, task.Title, task.State)
			return nil
		},
	}
	spawnCmd.Flags().String("title", "", "Task title")
	spawnCmd.Flags().String("description", "", "Task prompt for the member")
	spawnCmd.Flags().String("role", "worker", "Member role")
	spawnCmd.Flags().String("workdir", "", "Working directory")
	spawnCmd.Flags().Int("max-retries", 3, "Max retries")

	// --- abort subcommand — gracefully stop a member (abort primitive) ---
	abortCmd := &cobra.Command{
		Use:   "abort <task-id>",
		Short: "Gracefully stop a team member task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath, workdir)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			if err := eng.Abort(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("abort: %w", err)
			}
			fmt.Printf("✅ Task %s aborted\n", args[0])
			return nil
		},
	}

	// --- kill subcommand — forcefully terminate a member (kill primitive) ---
	killCmd := &cobra.Command{
		Use:   "kill <task-id>",
		Short: "Forcefully terminate a team member task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath, workdir)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			if err := eng.Kill(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("kill: %w", err)
			}
			fmt.Printf("✅ Task %s killed\n", args[0])
			return nil
		},
	}

	// --- summarize subcommand — read a member's report (summarize primitive) ---
	summarizeCmd := &cobra.Command{
		Use:   "summarize <task-id|session-id>",
		Short: "Print a team member's last report/summary",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath, workdir)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			sessionID := eng.Store.SessionID(args[0])
			if sessionID == "" {
				sessionID = args[0] // allow a raw session ID
			}
			summary, err := eng.Summarize(cmd.Context(), sessionID)
			if err != nil {
				return fmt.Errorf("summarize: %w", err)
			}
			if summary == "" {
				fmt.Printf("(no report yet for session %s)\n", sessionID)
				return nil
			}
			fmt.Println(summary)
			return nil
		},
	}

	// --- fork subcommand — clone a member session (fork primitive) ---
	forkCmd := &cobra.Command{
		Use:   "fork <task-id|session-id>",
		Short: "Clone a team member's session and print the new session ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath, workdir)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			sessionID := eng.Store.SessionID(args[0])
			if sessionID == "" {
				sessionID = args[0] // allow a raw session ID
			}
			newID, err := eng.Fork(cmd.Context(), sessionID)
			if err != nil {
				return fmt.Errorf("fork: %w", err)
			}
			fmt.Println(newID)
			return nil
		},
	}

	teamCmd.AddCommand(promptCmd)
	teamCmd.AddCommand(spawnCmd)
	teamCmd.AddCommand(abortCmd)
	teamCmd.AddCommand(killCmd)
	teamCmd.AddCommand(summarizeCmd)
	teamCmd.AddCommand(forkCmd)

	teamCmd.AddCommand(createCmd)
	teamCmd.AddCommand(runCmd)
	teamCmd.AddCommand(initCmd)
	teamCmd.AddCommand(executeCmd)
	teamCmd.AddCommand(analyzeCmd)
	teamCmd.AddCommand(statusCmd)
	teamCmd.AddCommand(listCmd)
	teamCmd.AddCommand(cancelCmd)
	teamCmd.AddCommand(feedbackCmd)
	// --- history subcommand — show task state history ---
	historyCmd := &cobra.Command{
		Use:   "history <task-id>",
		Short: "Show state transition history for a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := resolveQueryWorkdir(cmd, args); err != nil {
				return err
			}
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath, workdir)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			entries, err := eng.Store.GetTaskHistory(args[0])
			if err != nil {
				return fmt.Errorf("get history: %w", err)
			}
			if len(entries) == 0 {
				fmt.Printf("No history for task %s\n", args[0])
				return nil
			}
			fmt.Printf("📜 State history for %s:\n\n", args[0])
			for _, e := range entries {
				arrow := "→"
				if e.OldState == "" {
					arrow = "🆕"
				}
				fmt.Printf("  %s %s %s %s\n",
					e.ChangedAt[:19], e.OldState, arrow, e.NewState)
				if e.ErrorMsg != "" {
					fmt.Printf("     ⚠️ %s\n", e.ErrorMsg)
				}
			}
			return nil
		},
	}

	// --- export subcommand — export full task session log (场景1) ---
	exportCmd := &cobra.Command{
		Use:   "export <task-id>",
		Short: "Export full task session log as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath, workdir)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			jsonStr, err := eng.ExportTaskLogJSON(args[0])
			if err != nil {
				return fmt.Errorf("export: %w", err)
			}
			output, _ := cmd.Flags().GetString("output")
			if output != "" {
				if err := os.WriteFile(output, []byte(jsonStr), 0644); err != nil {
					return fmt.Errorf("write file: %w", err)
				}
				fmt.Printf("Session log exported to %s\n", output)
			} else {
				fmt.Println(jsonStr)
			}
			return nil
		},
	}
	exportCmd.Flags().StringP("output", "o", "", "Write to file instead of stdout")

	teamCmd.AddCommand(resolveCmd)
	teamCmd.AddCommand(escalationCmd)
	teamCmd.AddCommand(historyCmd)
	teamCmd.AddCommand(exportCmd)

	// --- trace subcommand — human-readable task execution trace ---
	traceCmd := &cobra.Command{
		Use:   "trace <task-id>",
		Short: "Show human-readable task execution trace",
		Long: `Show a detailed execution trace for a task including
timeline, output, verifier result, artifacts, and inbox messages.

Use --json for machine-readable output.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath, workdir)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			jsonFlag, _ := cmd.Flags().GetBool("json")
			if jsonFlag {
				jsonStr, err := eng.ExportTaskLogJSON(args[0])
				if err != nil {
					return fmt.Errorf("export: %w", err)
				}
				fmt.Println(jsonStr)
				return nil
			}

			log, err := eng.ExportTaskLog(args[0])
			if err != nil {
				return fmt.Errorf("trace: %w", err)
			}

			sep := strings.Repeat("=", 60)
			sub := strings.Repeat("-", 60)

			// Section 1: Task Info.
			fmt.Println(sep)
			fmt.Printf("  Task Trace: %s\n", log["task_id"])
			fmt.Println(sep)
			fmt.Printf("  Title:      %s\n", log["title"])
			fmt.Printf("  Role:       %s\n", log["role"])
			fmt.Printf("  State:      %s\n", log["state"])
			fmt.Printf("  Retries:    %s\n", log["retries"])
			fmt.Printf("  Created:    %s\n", log["created_at"])
			fmt.Printf("  Updated:    %s\n", log["updated_at"])
			if sid, ok := log["session_id"].(string); ok && sid != "" {
				fmt.Printf("  Session:    %s\n", sid)
			}

			// Section 2: State History Timeline.
			if history, ok := log["state_history"].([]map[string]string); ok && len(history) > 0 {
				fmt.Println(sub)
				fmt.Println("  Timeline:")
				for _, h := range history {
					ts := h["changed_at"]
					if len(ts) > 19 {
						ts = ts[:19]
					}
					oldState := h["old_state"]
					newState := h["new_state"]
					arrow := " → "
					if oldState == "" {
						arrow = "🆕 "
					}
					fmt.Printf("    %s  %-12s%s%s\n", ts, oldState, arrow, newState)
					if h["error_msg"] != "" {
						fmt.Printf("       ⚠️  %s\n", h["error_msg"])
					}
				}
			}

			// Section 3: Output Preview.
			if output, ok := log["output"].(string); ok && output != "" {
				fmt.Println(sub)
				fmt.Println("  Output Preview:")
				preview := output
				if len(preview) > 500 {
					preview = preview[:500] + "..."
				}
				for _, line := range strings.Split(preview, "\n") {
					fmt.Printf("    | %s\n", line)
				}
			}

			// Section 4: Verifier Result.
			if verifier, ok := log["verifier_result"].(string); ok && verifier != "" {
				fmt.Println(sub)
				fmt.Println("  Verifier Result:")
				for _, line := range strings.Split(verifier, "\n") {
					fmt.Printf("    | %s\n", line)
				}
			}

			// Section 5: Artifacts.
			if artifacts, ok := log["artifacts"].([]string); ok && len(artifacts) > 0 {
				fmt.Println(sub)
				fmt.Printf("  Artifacts (%d):\n", len(artifacts))
				for _, a := range artifacts {
					fmt.Printf("    - %s\n", a)
				}
			}

			// Section 6: Inbox Messages.
			if msgs, ok := log["inbox_messages"].([]interface{}); ok && len(msgs) > 0 {
				fmt.Println(sub)
				fmt.Printf("  Inbox Messages (%d):\n", len(msgs))
				for _, m := range msgs {
					if mm, ok := m.(map[string]interface{}); ok {
						from := fmt.Sprint(mm["from"])
						content := fmt.Sprint(mm["content"])
						fmt.Printf("    From: %s\n", from)
						fmt.Printf("      %s\n", content)
					}
				}
			}

			fmt.Println(sep)
			return nil
		},
	}
	traceCmd.Flags().Bool("json", false, "Output as JSON (machine-readable)")
	teamCmd.AddCommand(traceCmd)

	return teamCmd
}

// newLiteSpawner creates a fast FuncSpawner that calls the LLM API directly,
// without forking a subprocess.  Returns nil if no API key is configured.
// Only suitable for pure prompt→response calls (no tools, one-shot).
func newLiteSpawner(fallbackModel string) team_engine.SubagentSpawner {
	// Load API key the same way the rest of Whale does:
	// env var first, then ~/.whale/credentials.json.
	apiKey := loadDeepSeekAPIKey()
	if apiKey == "" {
		return nil
	}

	return team_engine.NewFuncSpawner(func(ctx context.Context, req team_engine.SubagentRequest) (team_engine.SubagentResponse, error) {
		mdl := req.Model
		if mdl == "" {
			mdl = fallbackModel
		}
		maxTok := req.MaxTokens
		if maxTok <= 0 {
			maxTok = 4096
		}
		client, err := deepseek.New(
			deepseek.WithAPIKey(apiKey),
			deepseek.WithModel(mdl),
			deepseek.WithMaxTokens(maxTok),
			deepseek.WithThinking(false),
			deepseek.WithTemperature(0),
		)
		if err != nil {
			return team_engine.SubagentResponse{
				Success:    false,
				Diagnostic: err.Error(),
			}, nil
		}
		messages := []core.Message{
			{Role: core.RoleUser, Text: req.Task},
		}
		events := client.StreamResponse(ctx, messages, nil)
		var fullText string
		for ev := range events {
			switch ev.Type {
			case llm.EventContentDelta:
				fullText += ev.Content
			case llm.EventError:
				return team_engine.SubagentResponse{
					Output:     fullText,
					Success:    false,
					Diagnostic: ev.Err.Error(),
				}, nil
			case llm.EventComplete:
			}
		}
		return team_engine.SubagentResponse{
			Output:  fullText,
			Success: true,
		}, nil
	})
}

// loadDeepSeekAPIKey reads the API key from the same sources as deepseek.New
// plus ~/.whale/credentials.json (which is where whale setup saves it).
func loadDeepSeekAPIKey() string {
	if v := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	credsPath := filepath.Join(home, ".whale", "credentials.json")
	data, err := os.ReadFile(credsPath)
	if err != nil {
		return ""
	}
	var creds struct {
		DeepSeekAPIKey string `json:"deepseek_api_key"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return ""
	}
	return strings.TrimSpace(creds.DeepSeekAPIKey)
}

// ensureTeamEngineSpawnFunc wires the native subagent adapter via
// SetDefaultSpawnFunc for the standalone `whale team` CLI. The app runtime
// normally wires this adapter itself; the CLI has no app runtime, so without
// this call workers/verifiers would fall back to the shell spawner and drop
// their AgentName (agent definition persona/tools/permission mode). It is a
// no-op when no API key is configured, leaving the shell spawner as fallback.
func ensureTeamEngineSpawnFunc(workdir string) {
	apiKey := loadDeepSeekAPIKey()
	if apiKey == "" {
		return
	}
	providerFactory := func(model string, maxTokens int) (llm.Provider, error) {
		if strings.TrimSpace(model) == "" {
			model = defaults.DefaultModel
		}
		opts := []deepseek.Option{
			deepseek.WithAPIKey(apiKey),
			deepseek.WithModel(model),
			// The CLI has no app runtime to set per-model thinking; the default
			// (DefaultThinkingEnabled=true) burns the completion budget on
			// chain-of-thought for reasoning models — the leader's native-adapter
			// decompose drops from ~284s to ~26s once thinking is disabled. Keep
			// the CLI team path on the same fast thinking-off route as the lite
			// spawner and the live-test spawner (realTeamLeaderSpawner), so worker/
			// verifier/leader all share one provider configuration.
			deepseek.WithThinking(false),
		}
		if maxTokens > 0 {
			opts = append(opts, deepseek.WithMaxTokens(maxTokens))
		}
		return deepseek.New(opts...)
	}
	library := tasks.NewAgentDefinitionLibrary(workdir)
	// Native subagents select tools from the parent registry by capability.
	// A minimal CLI runner has no parent tools, so build the workspace
	// toolset here — otherwise workers get an empty tool set and cannot
	// write artifacts (they'd emit only the <analysis> preamble).
	var parentTools *core.ToolRegistry
	if toolset, err := tools.NewToolset(workdir); err == nil {
		// The team tools go into the subagent registry (leaders select
		// team_run/team_status/… by name); the CLI runner's members are the
		// only consumers of this registry besides the parent controls in
		// app_tools_init, where the same set is assembled.
		toolsForRegistry := append(toolset.Tools(), toolset.TeamEngineTools()...)
		parentTools, _ = core.NewToolRegistryChecked(toolsForRegistry)
	}
	// Member subagent sessions must land on a real JSONLStore + sessions dir so
	// they are persistent, observable, and forkable — a runner without these
	// falls back to an InMemoryStore and members become unforkable.
	sessionsDir := store.DefaultSessionsDir(store.DefaultDataDir())
	msgStore, _ := store.NewJSONLStore(sessionsDir)
	// Team workers/verifiers run in this dedicated runner: enable low-threshold
	// auto-compact so long coding loops (TDD 迭代 + 浏览器验证螺旋) don't grow
	// unbounded histories. v4 的 1M 上下文窗口下 0.02 ≈ 20K 估计 token 触发；
	// 摘要必须保留任务契约/验收标准，否则压缩会丢上下文（v11 实验的教训）。
	// 仅 team 会话生效——主 app 的 runner 不受影响。
	runner := tasks.NewRunner(tasks.RunnerConfig{
		ProviderFactory:  providerFactory,
		AgentDefinitions: library,
		WorkspaceRoot:    workdir,
		DefaultModel:     defaults.DefaultModel,
		ParentTools:      parentTools,
		MessageStore:     msgStore,
		SessionsDir:      sessionsDir,
		AutoCompact:      true,
		AutoCompactThreshold: 0.02,
		CompactSummaryContext: "你是团队子代理（team worker）。压缩摘要必须逐条保留：任务契约与验收标准、产出文件路径与当前状态、" +
			"未通过的测试与报错原文、已验证通过的检查点、尚未完成的下一步。丢失契约会导致重试与返工。",
	})
	teamRuntime := app.NewTeamRuntime(runner, library, sessionsDir, msgStore)
	team_engine.SetDefaultSpawnFunc(teamRuntime.SpawnFunc())
	team_engine.SetDefaultSessionOps(teamRuntime)
}

// newTeamEngine creates a TeamEngine with a default shell-based spawner.
// The spawner calls the Whale CLI (via `whale exec`) for subagent execution.
func newTeamEngine(dbPath, whiteboardDir, configPath, workdir string) (*team_engine.TeamEngine, error) {
	// Resolve paths.
	if strings.TrimSpace(workdir) == "" {
		return nil, errors.New("--workdir is required (team state/deliverables location)")
	}
	if !filepath.IsAbs(workdir) {
		cwd, _ := os.Getwd()
		workdir = filepath.Join(cwd, workdir)
	}
	if !filepath.IsAbs(dbPath) {
		dbPath = filepath.Join(workdir, dbPath)
	}
	// workdir 是项目交付目录，也是 write 工具的 workspace 边界。让 whiteboardDir
	// （team 状态目录）和日志都跟随 workdir 而非 cwd，使 verify/ 等状态目录落在
	// workspace 内——否则 verifier 的 write 工具写 verify/ 会被 "escapes workspace"
	// 权限拦下，只能退化成静态分析。
	if !filepath.IsAbs(workdir) {
		cwd, _ := os.Getwd()
		workdir = filepath.Join(cwd, workdir)
	}
	if !filepath.IsAbs(whiteboardDir) {
		whiteboardDir = filepath.Join(workdir, whiteboardDir)
	}
	if configPath != "" && !filepath.IsAbs(configPath) {
		cwd, _ := os.Getwd()
		configPath = filepath.Join(cwd, configPath)
	}

	// The `whale team` CLI is a standalone entry point: no app runtime has
	// called SetDefaultSpawnFunc, so wire the native subagent adapter here.
	// This restores AgentName resolution (agent definition persona/tools/
	// permission) for workers and verifiers instead of degrading to the shell
	// spawner's bare "[Role: ...]" text.
	if team_engine.DefaultSpawnFunc() == nil {
		ensureTeamEngineSpawnFunc(workdir)
	}

	// Wire the team engine logger so team_engine.log lands on disk — the CLI
	// `team execute` path otherwise leaves it empty. SetLogger closes any
	// previous logger's file handle first, so repeated engine construction
	// (as in tests) does not leak the open file. Must run BEFORE LogSpawnerType
	// so the spawner-type record is not dropped (defaultTeamLog is nil until
	// SetLogger runs).
	team_engine.SetLogger(teampglog.NewTeamLog(workdir))

	// Prefer the native subagent adapter; fall back to the shell spawner when
	// no adapter could be wired (e.g. no API key configured).
	var spawner team_engine.SubagentSpawner
	if fn := team_engine.DefaultSpawnFunc(); fn != nil {
		spawner = team_engine.NewFuncSpawner(fn)
		team_engine.LogSpawnerType("default", "adapter", "", 0)
	} else {
		spawner = team_engine.NewShellSubagentSpawner()
		team_engine.LogSpawnerType("default", "shell", "", 0)
	}

	eng, err := team_engine.New(dbPath, whiteboardDir, configPath, spawner)
	if err != nil {
		return nil, err
	}
	// Inject the SessionOps wired by ensureTeamEngineSpawnFunc so the six
	// primitives (prompt/spawn/abort/kill/summarize/fork) can address member
	// sessions from the CLI.
	if ops := team_engine.DefaultSessionOps(); ops != nil {
		eng.SetSessionOps(ops)
	}
	return eng, nil
}

func printTask(task *team_engine.Task, jsonOutput bool) {
	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(task)
		return
	}

	fmt.Printf("📋 Task: %s\n", task.ID)
	fmt.Printf("   Title:    %s\n", task.Title)
	fmt.Printf("   Role:     %s\n", task.Role)
	fmt.Printf("   Profile:  %s\n", task.Profile)
	fmt.Printf("   State:    %s (%d%%)\n", task.State, team_engine.GetProgress(task.State))
	fmt.Printf("   Retries:  %d/%d\n", task.RetryCount, task.MaxRetries)
	fmt.Printf("   Created:  %s\n", task.CreatedAt)
	fmt.Printf("   Updated:  %s\n", task.UpdatedAt)
	if task.VerifierFocus != "" {
		fmt.Printf("   Focus:    %s\n", task.VerifierFocus)
	}
	if task.VerifierFeedback != "" {
		feedback := task.VerifierFeedback
		if len(feedback) > 200 {
			feedback = feedback[:200] + "..."
		}
		fmt.Printf("   Feedback: %s\n", feedback)
	}
}

func statusIcon(state team_engine.TaskState) string {
	switch state {
	case team_engine.TaskStatePending:
		return "⏳"
	case team_engine.TaskStateAssigned:
		return "📝"
	case team_engine.TaskStateProducing:
		return "🔧"
	case team_engine.TaskStateProduced:
		return "📦"
	case team_engine.TaskStateVerifying:
		return "🔍"
	case team_engine.TaskStateVerified:
		return "✅"
	case team_engine.TaskStateDone:
		return "✔️"
	case team_engine.TaskStateFailed:
		return "❌"
	case team_engine.TaskStateSuspended:
		return "⚠️"
	default:
		return "❓"
	}
}
