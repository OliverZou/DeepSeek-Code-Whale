package app

import (
	"context"
	"fmt"

	"github.com/usewhale/whale/internal/tasks"
	"github.com/usewhale/whale/internal/team_engine"
)

// teamEngineSpawnAdapter wraps a tasks.Runner into a team_engine.SpawnFunc,
// replacing the ShellSubagentSpawner (whale exec subprocess) with Whale's
// native subagent spawning mechanism. This avoids cold-starting a new Whale
// process for every team task, and enables proper context isolation, tool
// permissions, and audit logging.
func teamEngineSpawnAdapter(runner *tasks.Runner) team_engine.SpawnFunc {
	return func(ctx context.Context, req team_engine.SubagentRequest) (team_engine.SubagentResponse, error) {
		tasksReq := tasks.SpawnSubagentRequest{
			Task:         req.Task,
			Role:         req.Role,
			Model:        req.Model,
			MaxToolIters: req.MaxIters,
			MaxToolCalls: req.MaxCalls,
		}
		if len(req.Tools) > 0 {
			tasksReq.Tools = req.Tools
		}
		if req.Workdir != "" {
			tasksReq.Task = fmt.Sprintf("Working directory: %s\n\n%s", req.Workdir, req.Task)
		}

		resp, err := runner.SpawnSubagent(ctx, tasksReq)
		if err != nil {
			return team_engine.SubagentResponse{
				Output:   "",
				ExitCode: -1,
				Success:  false,
			}, fmt.Errorf("spawn subagent: %w", err)
		}

		success := resp.Status == "completed" || resp.Status == "done" || resp.Error == ""
		exitCode := 0
		if !success {
			exitCode = 1
		}

		return team_engine.SubagentResponse{
			Output:   resp.Summary,
			ExitCode: exitCode,
			Success:  success,
		}, nil
	}
}
