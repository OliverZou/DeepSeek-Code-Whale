package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/usewhale/whale/internal/team_engine"
	"github.com/usewhale/whale/internal/team_engine/server"
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
	teamCmd.PersistentFlags().StringVar(&workdir, "workdir", ".", "Working directory for agent execution")

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

			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			task, err := eng.CreateTask(
				title, description,
				team_engine.AgentRole(role),
				team_engine.ToolProfile(profile),
				nil, maxRetries, workdir, verifierFocus,
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
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			taskID := args[0]
			fmt.Printf("🚀 Running task %s...\n", taskID)

			success, err := eng.RunTask(taskID)
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

	// --- plan subcommand ---
	planCmd := &cobra.Command{
		Use:   "plan --goal GOAL",
		Short: "Decompose a goal and run all subtasks",
		RunE: func(cmd *cobra.Command, args []string) error {
			goal, _ := cmd.Flags().GetString("goal")
			if goal == "" {
				return fmt.Errorf("--goal is required")
			}

			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			fmt.Printf("📋 Planning goal: %s\n", goal)
			masterTask, mtErr := eng.CreateMasterTask(goal, workdir)
			if mtErr != nil {
				return fmt.Errorf("create master task: %w", mtErr)
			}
			batches, err := eng.PlanAndRun(cmd.Context(), goal, workdir, masterTask.ID)
			if err != nil {
				return fmt.Errorf("plan and run: %w", err)
			}

			fmt.Printf("\n📊 Results:\n")
			for _, batch := range batches {
				icon := "✅"
				if batch.Status == team_engine.BatchStatusFailed {
					icon = "❌"
				}
				fmt.Printf("  %s Batch %s [%s]\n", icon, batch.LabelOrID(), batch.Status)
				for _, t := range batch.Tasks {
					taskIcon := "  ✅"
					if t.State == team_engine.TaskStateFailed {
						taskIcon = "  ❌"
					}
					fmt.Printf("    %s %s [%s] %s\n", taskIcon, t.ID[:8], t.State, t.Title)
				}
			}
			return nil
		},
	}
	planCmd.Flags().String("goal", "", "The goal to decompose into subtasks")

	// --- status subcommand ---
	statusCmd := &cobra.Command{
		Use:   "status [task-id]",
		Short: "Show task status (all tasks, or one task by ID)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			jsonOutput, _ := cmd.Flags().GetBool("json")

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
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath)
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
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath)
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
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath)
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
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath)
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

	teamCmd.AddCommand(createCmd)
	teamCmd.AddCommand(runCmd)
	teamCmd.AddCommand(planCmd)
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
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			entries, err := eng.DB.GetTaskHistory(args[0])
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
			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath)
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

	// --- dashboard subcommand ---
	dashboardCmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Start real-time web dashboard",
		Long: `Start a web server with SSE-based live task monitoring.

Dashboard shows:
  - Real-time task status (auto-refreshes every 2s via SSE)
  - Aggregate statistics (total, success rate)
  - Progress bars for producing/verifying stages
  - Agent output preview
  - Dark theme, modern UI

Open http://localhost:8080 after starting.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			addr, _ := cmd.Flags().GetString("addr")

			eng, err := newTeamEngine(dbPath, whiteboardDir, configPath)
			if err != nil {
				return fmt.Errorf("init engine: %w", err)
			}
			defer eng.Close()

			srv := server.NewDashboardServer(eng, addr)
			return srv.Start()
		},
	}
	dashboardCmd.Flags().String("addr", "localhost:8080", "Listen address (host:port)")
	teamCmd.AddCommand(dashboardCmd)

	return teamCmd
}

// newTeamEngine creates a TeamEngine with a default shell-based spawner.
// The spawner calls the Whale CLI (via `whale exec`) for subagent execution.
func newTeamEngine(dbPath, whiteboardDir, configPath string) (*team_engine.TeamEngine, error) {
	// Resolve paths.
	if !filepath.IsAbs(dbPath) {
		cwd, _ := os.Getwd()
		dbPath = filepath.Join(cwd, dbPath)
	}
	if !filepath.IsAbs(whiteboardDir) {
		cwd, _ := os.Getwd()
		whiteboardDir = filepath.Join(cwd, whiteboardDir)
	}
	if configPath != "" && !filepath.IsAbs(configPath) {
		cwd, _ := os.Getwd()
		configPath = filepath.Join(cwd, configPath)
	}

	// Create a SubagentSpawner that calls the Whale CLI.
	spawner := team_engine.NewShellSubagentSpawner()

	return team_engine.New(dbPath, whiteboardDir, configPath, spawner)
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
	default:
		return "❓"
	}
}


