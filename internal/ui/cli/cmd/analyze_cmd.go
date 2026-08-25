package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// analyzeRun prints a one-page post-run analysis from the local trail:
// run_report.json + review_actions.jsonl under <whiteboardDir>/<masterID>.
func analyzeRun(whiteboardDir, masterID string) error {
	dir := filepath.Join(whiteboardDir, masterID)
	data, err := os.ReadFile(filepath.Join(dir, "run_report.json"))
	if err != nil {
		return fmt.Errorf("read run_report.json: %w", err)
	}
	var r runReportLite
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("parse run_report.json: %w", err)
	}

	fmt.Printf("=== Run %s [%s] ===\n", shortID(r.MasterTaskID), r.Status)
	fmt.Printf("wall=%.1fs tokens=%d leader_tokens=%d engine=%s\n", r.WallSeconds, r.TotalTokens, r.LeaderTokens, r.EngineCommit)
	fmt.Printf("leader_session=%s  started=%s finished=%s\n", r.LeaderSessionID, r.StartedAt, r.FinishedAt)
	fmt.Printf("summary: %s\n", r.Summary)

	verdicts := map[string]int{}
	rows := []runRow{}
	for _, b := range r.Batches {
		for _, t := range b.Tasks {
			verdicts[t.Verdict]++
			rows = append(rows, runRow{
				ID: t.ID[:8], Role: t.Role, State: t.State, Verdict: t.Verdict, Top: t.TopTools, Sess: t.SessionID,
				WS: t.WorkerDuration, VS: t.VerifierDuration, Wait: t.ToolWaitSeconds,
				WT: t.WorkerTokens, VT: t.VerifierTokens, Calls: t.ToolCalls, Retry: t.RetryCount, Cap: t.VerifierCapCount,
			})
		}
	}
	fmt.Println("--- verdicts ---")
	for k, v := range verdicts {
		fmt.Printf("  %-14s %d\n", k, v)
	}
	fmt.Println("--- tasks ---")
	for _, t := range rows {
		fmt.Printf("  %s %-16s %-9s %-14s worker=%6.1fs/%5dk calls=%3d wait=%6.1fs verify=%6.1fs/%5dk cap=%d retry=%d\n",
			t.ID, t.Role, t.State, t.Verdict, t.WS, t.WT/1000, t.Calls, t.Wait, t.VS, t.VT/1000, t.Cap, t.Retry)
		if t.Top != "" {
			fmt.Printf("      top_tools=[%s]  session=%s\n", t.Top, t.Sess)
		}
	}

	if raw, err := os.ReadFile(filepath.Join(dir, "review_actions.jsonl")); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
		fmt.Println("--- leader review actions ---")
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			var a reviewAction
			if json.Unmarshal([]byte(line), &a) == nil {
				fmt.Printf("  %s %-14s %s [%s] note=%s\n", a.Ts, a.Action, shortID(a.TaskID), a.Result, a.Note)
			}
		}
	} else {
		fmt.Println("(no review_actions.jsonl - leader did not re-dispatch)")
	}
	fmt.Printf("\nusage trail: ~/.whale/usage/<session_id>.jsonl (per-request tokens)\n")
	return nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

type runRow struct {
	ID, Role, State, Verdict, Top, Sess string
	WS, VS, Wait                        float64
	WT, VT, Calls, Retry, Cap           int
}

type runReportLite struct {
	MasterTaskID    string  `json:"master_task_id"`
	Status          string  `json:"status"`
	Summary         string  `json:"summary"`
	StartedAt       string  `json:"started_at"`
	FinishedAt      string  `json:"finished_at"`
	WallSeconds     float64 `json:"wall_seconds"`
	TotalTokens     int     `json:"total_tokens"`
	LeaderSessionID string  `json:"leader_session_id"`
	EngineCommit    string  `json:"engine_commit"`
	LeaderTokens    int     `json:"leader_tokens"`
	Batches         []struct {
		ID     string `json:"id"`
		Label  string `json:"label"`
		Status string `json:"status"`
		Tasks  []struct {
			ID               string  `json:"id"`
			Role             string  `json:"role"`
			State            string  `json:"state"`
			RetryCount       int     `json:"retry_count"`
			WorkerDuration   float64 `json:"worker_duration_seconds"`
			WorkerTokens     int     `json:"worker_tokens"`
			VerifierDuration float64 `json:"verifier_duration_seconds"`
			VerifierTokens   int     `json:"verifier_tokens"`
			ToolCalls        int     `json:"tool_calls"`
			ToolWaitSeconds  float64 `json:"tool_wait_seconds"`
			TopTools         string  `json:"top_tools"`
			SessionID        string  `json:"session_id"`
			Verdict          string  `json:"verdict"`
			VerifierCapCount int     `json:"verifier_cap_count"`
		} `json:"tasks"`
	} `json:"batches"`
}

type reviewAction struct {
	Ts     string `json:"ts"`
	Action string `json:"action"`
	TaskID string `json:"task_id"`
	Result string `json:"result"`
	Note   string `json:"note"`
}
