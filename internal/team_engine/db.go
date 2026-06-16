package team_engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// TaskDB is a SQLite-backed task store with state-transition validation,
// history tracking, and agent memory.
type TaskDB struct {
	db *sql.DB
}

// NewDB opens (or creates) the SQLite database at dbPath and runs
// schema migration.  Use ":memory:" for an in-memory database.
func NewDB(dbPath string) (*TaskDB, error) {
	// Ensure the parent directory exists (skip for :memory: and similar special names).
	if dbPath != ":memory:" && !strings.Contains(dbPath, "?") {
		dir := filepath.Dir(dbPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create db parent dir %s: %w", dir, err)
		}
	}
	d, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := d.Exec("PRAGMA journal_mode=WAL"); err != nil {
		d.Close()
		return nil, fmt.Errorf("pragma journal_mode: %w", err)
	}
	if _, err := d.Exec("PRAGMA foreign_keys=ON"); err != nil {
		d.Close()
		return nil, fmt.Errorf("pragma foreign_keys: %w", err)
	}

	tdb := &TaskDB{db: d}
	if err := tdb.migrate(); err != nil {
		d.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return tdb, nil
}

func (tdb *TaskDB) Close() error { return tdb.db.Close() }

// Checkpoint forces a WAL checkpoint so that all readers see the latest writes.
// Call after deletions or bulk state changes that must be immediately visible.
func (tdb *TaskDB) Checkpoint() error {
	_, err := tdb.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

// checkpointAfterWrite is called after every write operation to ensure
// cross-process readers (Dashboard) can see the latest data immediately.
// Errors are silently ignored — the write itself already succeeded.
func (tdb *TaskDB) checkpointAfterWrite() {
	_, _ = tdb.db.Exec("PRAGMA wal_checkpoint(PASSIVE)")
}

// ---------------------------------------------------------------------------
// Schema migration
// ---------------------------------------------------------------------------

func (tdb *TaskDB) migrate() error {
	// Core tables.
	schema := `
	CREATE TABLE IF NOT EXISTS tasks (
		id               TEXT PRIMARY KEY,
		title            TEXT NOT NULL,
		description      TEXT NOT NULL DEFAULT '',
		role             TEXT NOT NULL DEFAULT 'developer',
		profile          TEXT NOT NULL DEFAULT 'default',
		state            TEXT NOT NULL DEFAULT 'pending',
		max_retries      INTEGER NOT NULL DEFAULT 3,
		retry_count      INTEGER NOT NULL DEFAULT 0,
		workdir          TEXT NOT NULL DEFAULT '.',
		parent_ids       TEXT NOT NULL DEFAULT '[]',
		artifact_path    TEXT NOT NULL DEFAULT '',
		verifier_feedback TEXT NOT NULL DEFAULT '',
		verifier_focus   TEXT NOT NULL DEFAULT '',
		batch_id         TEXT NOT NULL DEFAULT '',
		created_at       TEXT NOT NULL,
		updated_at       TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS state_history (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id     TEXT NOT NULL,
		old_state   TEXT,
		new_state   TEXT NOT NULL,
		error_msg   TEXT NOT NULL DEFAULT '',
		changed_at  TEXT NOT NULL,
		FOREIGN KEY (task_id) REFERENCES tasks(id)
	);

	CREATE TABLE IF NOT EXISTS agent_memory (
		id          TEXT PRIMARY KEY,
		agent_role  TEXT NOT NULL,
		mem_key     TEXT NOT NULL,
		content     TEXT NOT NULL,
		source_task TEXT NOT NULL DEFAULT '',
		created_at  TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS master_tasks (
		id               TEXT PRIMARY KEY,
		goal             TEXT NOT NULL,
		workspace_path   TEXT NOT NULL DEFAULT '',
		status           TEXT NOT NULL DEFAULT 'pending',
		batch_progress   TEXT NOT NULL DEFAULT '',
		created_at       TEXT NOT NULL,
		updated_at       TEXT NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_tasks_state   ON tasks(state);
	CREATE INDEX IF NOT EXISTS idx_tasks_created ON tasks(created_at);
	CREATE INDEX IF NOT EXISTS idx_history_task  ON state_history(task_id);
	CREATE INDEX IF NOT EXISTS idx_memory_role   ON agent_memory(agent_role);
	CREATE INDEX IF NOT EXISTS idx_memory_key    ON agent_memory(agent_role, mem_key);
	CREATE INDEX IF NOT EXISTS idx_master_created ON master_tasks(created_at);
	`
	if _, err := tdb.db.Exec(schema); err != nil {
		return fmt.Errorf("create tables: %w", err)
	}

	// Auto-migration: add columns that may not exist in older databases.
	autoCols := []string{
		"batch_id	TEXT NOT NULL DEFAULT ''",
		"profile	TEXT NOT NULL DEFAULT 'default'",
		"master_task_id	TEXT NOT NULL DEFAULT ''",
		"batch_progress	TEXT NOT NULL DEFAULT ''",
	}
	for _, col := range autoCols {
		parts := strings.Fields(col)
		if len(parts) > 0 {
			tdb.db.Exec("ALTER TABLE tasks ADD COLUMN " + col)
		}
	}

	// Auto-migration for master_tasks.
	masterAutoCols := []string{
		"batch_progress	TEXT NOT NULL DEFAULT ''",
	}
	for _, col := range masterAutoCols {
		parts := strings.Fields(col)
		if len(parts) > 0 {
			tdb.db.Exec("ALTER TABLE master_tasks ADD COLUMN " + col)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// State history
// ---------------------------------------------------------------------------

// RecordStateHistory records a state transition in the history table.
func (tdb *TaskDB) RecordStateHistory(taskID, oldState, newState, errorMsg string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	old := sql.NullString{String: oldState, Valid: oldState != ""}
	_, err := tdb.db.Exec(
		`INSERT INTO state_history (task_id, old_state, new_state, error_msg, changed_at)
		 VALUES (?, ?, ?, ?, ?)`,
		taskID, old, newState, errorMsg, now,
	)
	tdb.checkpointAfterWrite()
	return err
}

// GetTaskHistory returns all state transitions for a task, ordered by time.
type StateHistoryEntry struct {
	ID        int
	TaskID    string
	OldState  string
	NewState  string
	ErrorMsg  string
	ChangedAt string
}

func (tdb *TaskDB) GetTaskHistory(taskID string) ([]StateHistoryEntry, error) {
	rows, err := tdb.db.Query(
		`SELECT id, task_id, old_state, new_state, error_msg, changed_at
		 FROM state_history WHERE task_id = ? ORDER BY id`, taskID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []StateHistoryEntry
	for rows.Next() {
		var old sql.NullString
		var e StateHistoryEntry
		if err := rows.Scan(&e.ID, &e.TaskID, &old, &e.NewState, &e.ErrorMsg, &e.ChangedAt); err != nil {
			return nil, err
		}
		if old.Valid {
			e.OldState = old.String
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ---------------------------------------------------------------------------
// MasterTask — records a user's top-level goal (总任务)
// ---------------------------------------------------------------------------

// MasterTask represents a user's top-level goal (总任务).
type MasterTask struct {
	ID            string `json:"id"`
	Goal          string `json:"goal"`
	WorkspacePath string `json:"workspace_path"`
	Status        string `json:"status"`
	BatchProgress string `json:"batch_progress"` // JSON checkpoint: {"completed_batches":[...], "batch_cycles":{...}}
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// InsertMasterTask creates a new master task record.
func (tdb *TaskDB) InsertMasterTask(mt *MasterTask) error {
	now := time.Now().UTC().Format(time.RFC3339)
	mt.CreatedAt = now
	mt.UpdatedAt = now
	_, err := tdb.db.Exec(
		`INSERT INTO master_tasks (id, goal, workspace_path, status, batch_progress, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		mt.ID, mt.Goal, mt.WorkspacePath, mt.Status, mt.BatchProgress, mt.CreatedAt, mt.UpdatedAt,
	)
	tdb.checkpointAfterWrite()
	return err
}

// ListMasterTasks returns all master tasks ordered by creation time (newest first).
func (tdb *TaskDB) ListMasterTasks() ([]*MasterTask, error) {
	rows, err := tdb.db.Query(
		`SELECT id, goal, workspace_path, status, batch_progress, created_at, updated_at
		 FROM master_tasks ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list master tasks: %w", err)
	}
	defer rows.Close()

	var tasks []*MasterTask
	for rows.Next() {
		mt := &MasterTask{}
		if err := rows.Scan(&mt.ID, &mt.Goal, &mt.WorkspacePath, &mt.Status, &mt.BatchProgress, &mt.CreatedAt, &mt.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan master task: %w", err)
		}
		tasks = append(tasks, mt)
	}
	return tasks, rows.Err()
}

// SaveMasterTaskProgress persists a JSON checkpoint for a master task.
func (tdb *TaskDB) SaveMasterTaskProgress(masterTaskID, progressJSON string) error {
	_, err := tdb.db.Exec(
		`UPDATE master_tasks SET batch_progress = ?, updated_at = ? WHERE id = ?`,
		progressJSON, time.Now().UTC().Format(time.RFC3339), masterTaskID,
	)
	tdb.checkpointAfterWrite()
	return err
}

// GetMasterTaskProgress returns the JSON checkpoint for a master task.
func (tdb *TaskDB) GetMasterTaskProgress(masterTaskID string) (string, error) {
	var progress string
	err := tdb.db.QueryRow(
		`SELECT COALESCE(batch_progress, '') FROM master_tasks WHERE id = ?`, masterTaskID,
	).Scan(&progress)
	if err != nil {
		return "", err
	}
	return progress, nil
}

// UpdateTaskMasterTaskID sets the master_task_id for a task.
func (tdb *TaskDB) UpdateTaskMasterTaskID(taskID, masterTaskID string) error {
	_, err := tdb.db.Exec(
		`UPDATE tasks SET master_task_id = ? WHERE id = ?`,
		masterTaskID, taskID,
	)
	tdb.checkpointAfterWrite()
	return err
}

// ListTasksByMasterTask returns all tasks belonging to a master task.
func (tdb *TaskDB) ListTasksByMasterTask(masterTaskID string) ([]*Task, error) {
	rows, err := tdb.db.Query(
		`SELECT id, title, description, role, profile, state, max_retries,
		 retry_count, workdir, parent_ids, artifact_path, verifier_feedback,
		 verifier_focus, batch_id, master_task_id, created_at, updated_at
		FROM tasks WHERE master_task_id = ? ORDER BY created_at ASC`, masterTaskID,
	)
	if err != nil {
		return nil, fmt.Errorf("list tasks by master: %w", err)
	}
	defer rows.Close()

	var tasks []*Task
	for rows.Next() {
		task, err := tdb.scanTaskFromRows(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// GetMasterTask retrieves a master task by ID.
func (tdb *TaskDB) GetMasterTask(id string) (*MasterTask, error) {
	row := tdb.db.QueryRow(
		`SELECT id, goal, workspace_path, status, created_at, updated_at
		 FROM master_tasks WHERE id = ?`, id,
	)
	mt := &MasterTask{}
	if err := row.Scan(&mt.ID, &mt.Goal, &mt.WorkspacePath, &mt.Status, &mt.CreatedAt, &mt.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan master task: %w", err)
	}
	return mt, nil
}

// UpdateMasterTaskStatus updates the status and updated_at.
func (tdb *TaskDB) UpdateMasterTaskStatus(id, status string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := tdb.db.Exec(
		`UPDATE master_tasks SET status = ?, updated_at = ? WHERE id = ?`,
		status, now, id,
	)
	tdb.checkpointAfterWrite()
	return err
}

// ---------------------------------------------------------------------------
// Task CRUD
// ---------------------------------------------------------------------------

func (tdb *TaskDB) InsertTask(task *Task) error {
	parentJSON, err := json.Marshal(task.ParentIDs)
	if err != nil {
		return fmt.Errorf("marshal parent_ids: %w", err)
	}

	_, err = tdb.db.Exec(
		`INSERT INTO tasks
		(id, title, description, role, profile, state, max_retries, retry_count,
		 workdir, parent_ids, artifact_path, verifier_feedback, verifier_focus,
		 batch_id, master_task_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		task.ID, task.Title, task.Description, string(task.Role), string(task.Profile),
		string(task.State), task.MaxRetries, task.RetryCount,
		task.Workdir, string(parentJSON), task.ArtifactPath, task.VerifierFeedback,
		task.VerifierFocus, task.BatchID, task.MasterTaskID, task.CreatedAt, task.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	// Record initial state.
	_ = tdb.RecordStateHistory(task.ID, "", string(task.State), "")
	tdb.checkpointAfterWrite()
	return nil
}

func (tdb *TaskDB) GetTask(id string) (*Task, error) {
	row := tdb.db.QueryRow(
		`SELECT id, title, description, role, profile, state, max_retries,
		 retry_count, workdir, parent_ids, artifact_path, verifier_feedback,
		 verifier_focus, batch_id, master_task_id, created_at, updated_at
		FROM tasks WHERE id = ?`, id,
	)
	return tdb.scanTask(row)
}

func (tdb *TaskDB) ListTasks() ([]*Task, error) {
	rows, err := tdb.db.Query(
		`SELECT id, title, description, role, profile, state, max_retries,
		 retry_count, workdir, parent_ids, artifact_path, verifier_feedback,
		 verifier_focus, batch_id, master_task_id, created_at, updated_at
		FROM tasks ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	var tasks []*Task
	for rows.Next() {
		task, err := tdb.scanTaskFromRows(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func (tdb *TaskDB) ListTasksByState(state TaskState) ([]*Task, error) {
	rows, err := tdb.db.Query(
		`SELECT id, title, description, role, profile, state, max_retries,
		 retry_count, workdir, parent_ids, artifact_path, verifier_feedback,
		 verifier_focus, batch_id, master_task_id, created_at, updated_at
		FROM tasks WHERE state = ? ORDER BY created_at DESC`, string(state),
	)
	if err != nil {
		return nil, fmt.Errorf("list tasks by state: %w", err)
	}
	defer rows.Close()

	var tasks []*Task
	for rows.Next() {
		task, err := tdb.scanTaskFromRows(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func (tdb *TaskDB) UpdateTask(id string, fields map[string]interface{}) error {
	if len(fields) == 0 {
		return nil
	}
	var setClauses []string
	var args []interface{}
	for k, v := range fields {
		setClauses = append(setClauses, k+" = ?")
		args = append(args, v)
	}
	setClauses = append(setClauses, "updated_at = ?")
	args = append(args, time.Now().UTC().Format(time.RFC3339))
	args = append(args, id)

	query := fmt.Sprintf("UPDATE tasks SET %s WHERE id = ?", strings.Join(setClauses, ", "))
	_, err := tdb.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("update task %s: %w", id, err)
	}
	tdb.checkpointAfterWrite()
	return nil
}

func (tdb *TaskDB) TransitionState(id string, newState TaskState, failureReason, verifierFeedback string) error {
	task, err := tdb.GetTask(id)
	if err != nil {
		return fmt.Errorf("get task for transition: %w", err)
	}
	if task == nil {
		return fmt.Errorf("task %q not found", id)
	}
	if !CanTransition(task.State, newState) {
		return fmt.Errorf("invalid transition %s -> %s for task %s", task.State, newState, id)
	}

	fields := map[string]interface{}{
		"state": string(newState),
	}
	if failureReason != "" {
		fields["artifact_path"] = failureReason
	}
	if verifierFeedback != "" {
		fields["verifier_feedback"] = verifierFeedback
	}
	if err := tdb.UpdateTask(id, fields); err != nil {
		return err
	}

	// Record in state history.
	_ = tdb.RecordStateHistory(id, string(task.State), string(newState), failureReason)
	return nil
}

// ForceTransitionState is like TransitionState but skips the CanTransition
// check.  It is used for resume operations where the normal state machine
// does not allow the transition (e.g. failed → pending).
func (tdb *TaskDB) ForceTransitionState(id string, newState TaskState, reason string) error {
	task, err := tdb.GetTask(id)
	if err != nil {
		return fmt.Errorf("get task for force transition: %w", err)
	}
	if task == nil {
		return fmt.Errorf("task %q not found", id)
	}

	fields := map[string]interface{}{
		"state": string(newState),
	}
	if err := tdb.UpdateTask(id, fields); err != nil {
		return err
	}

	_ = tdb.RecordStateHistory(id, string(task.State), string(newState), reason)
	return nil
}

func (tdb *TaskDB) DeleteTask(id string) error {
	tdb.db.Exec("DELETE FROM state_history WHERE task_id = ?", id)
	_, err := tdb.db.Exec("DELETE FROM tasks WHERE id = ?", id)
	tdb.checkpointAfterWrite()
	return err
}

// DeleteMasterTask deletes a master task and all its subtasks (with history).
func (tdb *TaskDB) DeleteMasterTask(masterTaskID string) error {
	// Delete state history for all subtasks.
	tdb.db.Exec("DELETE FROM state_history WHERE task_id IN (SELECT id FROM tasks WHERE master_task_id = ?)", masterTaskID)
	// Delete all subtasks.
	tdb.db.Exec("DELETE FROM tasks WHERE master_task_id = ?", masterTaskID)
	// Delete the master task itself.
	_, err := tdb.db.Exec("DELETE FROM master_tasks WHERE id = ?", masterTaskID)
	tdb.checkpointAfterWrite()
	return err
}

// ---------------------------------------------------------------------------
// Scan helpers
// ---------------------------------------------------------------------------

func (tdb *TaskDB) scanTask(scanner interface {
	Scan(dest ...interface{}) error
}) (*Task, error) {
	var (
		id, title, desc, role, profile, state, workdir, batchID string
		masterTaskID                                             string
		parentJSON, artifactPath, verifierFeedback              string
		verifierFocus, createdAt, updatedAt                     string
		maxRetries, retryCount                                  int
	)
	err := scanner.Scan(
		&id, &title, &desc, &role, &profile, &state,
		&maxRetries, &retryCount, &workdir, &parentJSON,
		&artifactPath, &verifierFeedback, &verifierFocus,
		&batchID, &masterTaskID, &createdAt, &updatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan task: %w", err)
	}

	var parentIDs []string
	if err := json.Unmarshal([]byte(parentJSON), &parentIDs); err != nil {
		parentIDs = []string{}
	}

	return &Task{
		ID:               id,
		Title:            title,
		Description:      desc,
		Role:             AgentRole(role),
		Profile:          ToolProfile(profile),
		State:            TaskState(state),
		MaxRetries:       maxRetries,
		RetryCount:       retryCount,
		Workdir:          workdir,
		ParentIDs:        parentIDs,
		ArtifactPath:     artifactPath,
		VerifierFeedback: verifierFeedback,
		VerifierFocus:    verifierFocus,
		BatchID:          batchID,
		MasterTaskID:     masterTaskID,
		CreatedAt:        createdAt,
		UpdatedAt:        updatedAt,
	}, nil
}

func (tdb *TaskDB) scanTaskFromRows(rows *sql.Rows) (*Task, error) {
	return tdb.scanTask(rows)
}

// ---------------------------------------------------------------------------
// Agent Memory
// ---------------------------------------------------------------------------

// MemoryEntry is a persisted agent experience/learning.
type MemoryEntry struct {
	ID         string `json:"id"`
	AgentRole  string `json:"agent_role"`
	Key        string `json:"key"`
	Content    string `json:"content"`
	SourceTask string `json:"source_task"`
	CreatedAt  string `json:"created_at"`
}

// SaveMemory persists an agent memory entry.
func (tdb *TaskDB) SaveMemory(memory *MemoryEntry) error {
	if memory.ID == "" {
		memory.ID = fmt.Sprintf("mem-%d", time.Now().UnixNano())
	}
	if memory.CreatedAt == "" {
		memory.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	_, err := tdb.db.Exec(
		`INSERT OR REPLACE INTO agent_memory (id, agent_role, mem_key, content, source_task, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		memory.ID, memory.AgentRole, memory.Key, memory.Content, memory.SourceTask, memory.CreatedAt,
	)
	tdb.checkpointAfterWrite()
	return err
}

// GetMemories retrieves memory entries for a role, optionally filtered by key.
func (tdb *TaskDB) GetMemories(role AgentRole, key string) ([]MemoryEntry, error) {
	var rows *sql.Rows
	var err error
	if key != "" {
		rows, err = tdb.db.Query(
			`SELECT id, agent_role, mem_key, content, source_task, created_at
			 FROM agent_memory WHERE agent_role = ? AND mem_key = ? ORDER BY created_at DESC
			 LIMIT 10`, string(role), key,
		)
	} else {
		rows, err = tdb.db.Query(
			`SELECT id, agent_role, mem_key, content, source_task, created_at
			 FROM agent_memory WHERE agent_role = ? ORDER BY created_at DESC
			 LIMIT 50`, string(role),
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []MemoryEntry
	for rows.Next() {
		var m MemoryEntry
		if err := rows.Scan(&m.ID, &m.AgentRole, &m.Key, &m.Content, &m.SourceTask, &m.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, m)
	}
	return entries, rows.Err()
}
