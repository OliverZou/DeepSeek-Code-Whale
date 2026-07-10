//go:build !teamlog

// Package log provides structured logging for the team engine lifecycle.
// Build with -tags teamlog to enable verbose diagnostic logging.
package log

// TeamLog is a no-op when the teamlog build tag is absent.
type TeamLog struct{}

// NewTeamLog returns a no-op logger.
func NewTeamLog(workspaceRoot string) *TeamLog { return &TeamLog{} }

// NewTeamLogAt returns a no-op logger at an explicit path.
func NewTeamLogAt(path string) *TeamLog { return &TeamLog{} }

// AddLog is a no-op.
func (t *TeamLog) AddLog(path string) {}

// Log is a no-op.
func (t *TeamLog) Log(cat, format string, args ...interface{}) {}

// Close is a no-op.
func (t *TeamLog) Close() error { return nil }

// --- No-op methods for each lifecycle node ---

func (t *TeamLog) DashboardRegister(path, wsID string, err error)          {}
func (t *TeamLog) DashboardWSConnect(wsID string, ok bool, err error)     {}
func (t *TeamLog) DashboardWSDisconnect(wsID string)                       {}
func (t *TeamLog) DashboardQueueResume(wsID, taskID, method string)        {}
func (t *TeamLog) DashboardStateTransition(taskID, from, to, reason string) {}
func (t *TeamLog) DashboardResumeMaster(wsID, taskID string, err error)    {}

func (t *TeamLog) SpawnerType(role, kind, model string, maxTokens int)      {}
func (t *TeamLog) LeaderDecompose(goal string, model string, attempt, maxTokens, promptTok, compTok int, dur float64, outputLen int, truncated, success bool) {}
func (t *TeamLog) LeaderPlan(planTasks int, err error)                      {}
func (t *TeamLog) LeaderRetry(attempt int, reason string)                   {}

func (t *TeamLog) WorkerStart(taskID, role, model string, attempt, maxRetries int) {}
func (t *TeamLog) WorkerDone(taskID string, dur float64, exitCode int, outputLen int, success bool) {}
func (t *TeamLog) WorkerRetry(taskID string, attempt int, feedback string)          {}

func (t *TeamLog) VerifierStart(taskID string)                             {}
func (t *TeamLog) VerifierDone(taskID string, passed bool, dur float64)    {}

func (t *TeamLog) BatchStart(batchID, label string, taskCount, cycle, maxCycles int) {}
func (t *TeamLog) BatchDone(batchID string, status string, dur float64)     {}
func (t *TeamLog) BatchCycleReport(batchID string, cycle int, decision string) {}

func (t *TeamLog) EngineResume(masterTaskID, goal string, batchCount int, err error) {}
func (t *TeamLog) EngineAutoResume(masterTaskID string, err error)          {}
func (t *TeamLog) EngineResumeTask(taskID, newState string)                 {}

func (t *TeamLog) CLIHeartbeat(wsID string, registered bool)               {}
func (t *TeamLog) CLIWSConnect(wsID string, err error)                     {}
func (t *TeamLog) CLIWSDisconnect(wsID string)                              {}
func (t *TeamLog) CLIReceiveResume(masterTaskID string)                     {}
