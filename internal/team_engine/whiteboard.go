package team_engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Whiteboard manages file-system based inter-agent communication.
//
// Each task gets a directory under <base_dir>/<task_id>/:
//
//	tasks/<task_id>/
//	├── input.md        # Leader 写入的任务描述 + 上下文
//	├── output.md       # Worker 写入的产出
//	├── verifier.md     # Verifier 写入的检查结果
//	├── status.json     # 当前状态元数据
//	├── inbox/          # Agent 间通讯：发给本 Agent 的消息（由他人写入）
//	│   ├── 001_from_human.json
//	│   └── 002_from_agent-B.json
//	├── outbox/         # Agent 间通讯：本 Agent 发出的消息
//	│   └── 001_to_agent-C.json
//	└── artifacts/      # Worker 产出的具体文件（代码等）
//
// Agent 间通讯原则（与人类同权）：
//   - 任何渠道（人类、Agent、Engine）都可以通过统一的 prompt/spawn/abort/kill
//     接口操作 Agent
//   - Agent 之间可以像人类一样进行多轮交互，包括主动推送和按需查询
//   - 消息存储在 inbox/outbox 中，Agent 启动时自动读取未读消息
//   - Agent 通过标准工具读写消息，与人类的交互方式完全一致
// InboxParams carries all inputs needed to write a task's inbox.md.
type InboxParams struct {
	Title           string
	Role            string
	Description     string
	Output          string
	UpstreamOutputs []UpstreamRef
	Template        string
	Memory          string
	AllowSelfSplit  bool
	RetryFeedback   string
}

// UpstreamRef is a named file reference to an upstream output.
type UpstreamRef struct {
	Name string
	Path string
}

type Whiteboard struct {
	baseDir string
}

// NewWhiteboard creates a Whiteboard rooted at baseDir.
func NewWhiteboard(baseDir string) (*Whiteboard, error) {
	abs, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("resolve base dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0755); err != nil {
		return nil, fmt.Errorf("create whiteboard dir %s: %w", abs, err)
	}
	return &Whiteboard{baseDir: abs}, nil
}

// BaseDir returns the absolute path to the whiteboard root.
func (wb *Whiteboard) BaseDir() string {
	return wb.baseDir
}

// TaskDir returns the directory for a given task.
func (wb *Whiteboard) TaskDir(taskID string) string {
	return filepath.Join(wb.baseDir, taskID)
}

// InitTask creates the task directory and writes input.md.
// NOTE: does NOT overwrite status.json — the caller is responsible for
// updating task state via WriteStatus when the state machine transitions.
func (wb *Whiteboard) InitTask(taskID, input string) error {
	taskDir := wb.TaskDir(taskID)
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		return fmt.Errorf("create task dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(taskDir, "artifacts"), 0755); err != nil {
		return fmt.Errorf("create artifacts dir: %w", err)
	}
	if err := wb.writeFile(filepath.Join(taskDir, "input.md"), input); err != nil {
		return fmt.Errorf("write input.md: %w", err)
	}
	// Only write initial status.json if it doesn't already exist (first init).
	statusPath := filepath.Join(taskDir, "status.json")
	if _, err := os.Stat(statusPath); os.IsNotExist(err) {
		return wb.WriteStatus(taskID, "initialised")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Board — 全局进度白板 (board.md) 和交付物汇总 (deliverable.md)
// ---------------------------------------------------------------------------

// BoardPath returns the path to the global board.md file.
func (wb *Whiteboard) BoardPath() string {
	return filepath.Join(wb.baseDir, "board.md")
}

// DeliverablePath returns the path to the global deliverable.md file.
func (wb *Whiteboard) DeliverablePath() string {
	return filepath.Join(wb.baseDir, "deliverable.md")
}

// WriteBoard writes/updates the global progress board.
func (wb *Whiteboard) WriteBoard(content string) error {
	return wb.writeFile(wb.BoardPath(), content)
}

// ReadBoard reads the current board content.
func (wb *Whiteboard) ReadBoard() (string, error) {
	return wb.readFile(wb.BoardPath())
}

// WriteDeliverable writes the final deliverable summary.
func (wb *Whiteboard) WriteDeliverable(content string) error {
	return wb.writeFile(wb.DeliverablePath(), content)
}

// ReadDeliverable reads the deliverable content.
func (wb *Whiteboard) ReadDeliverable() (string, error) {
	return wb.readFile(wb.DeliverablePath())
}

// BuildBoardContent generates a board.md summary from batches and tasks.
func (wb *Whiteboard) BuildBoardContent(batches []*Batch) string {
	var b strings.Builder
	b.WriteString("# Team Board\n\n")
	b.WriteString("## Batch Status\n\n")
	b.WriteString("| Batch | Status | Tasks | Progress |\n")
	b.WriteString("|-------|--------|-------|----------|\n")
	for _, batch := range batches {
		done := 0
		for _, t := range batch.Tasks {
			if t.State == TaskStateDone {
				done++
			}
		}
		pct := 0
		if len(batch.Tasks) > 0 {
			pct = done * 100 / len(batch.Tasks)
		}
		b.WriteString(fmt.Sprintf("| %s | %s | %d | %d%% (%d/%d) |\n",
			batch.LabelOrID(), batch.Status, len(batch.Tasks), pct, done, len(batch.Tasks)))
	}

	b.WriteString("\n## Task Details\n\n")
	for _, batch := range batches {
		b.WriteString(fmt.Sprintf("### %s\n\n", batch.LabelOrID()))
		b.WriteString("| Task | Role | State | Retries |\n")
		b.WriteString("|------|------|-------|---------|\n")
		for _, t := range batch.Tasks {
			b.WriteString(fmt.Sprintf("| %s | %s | %s | %d/%d |\n",
				shortID(t.ID), t.Role, t.State, t.RetryCount, t.MaxRetries))
		}
		b.WriteString("\n")
	}

	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("_Last updated: %s_\n", time.Now().UTC().Format(time.RFC3339)))
	return b.String()
}

// BuildDeliverableContent generates a deliverable.md from completed tasks.
func (wb *Whiteboard) BuildDeliverableContent(batches []*Batch) string {
	var b strings.Builder
	b.WriteString("# Deliverables\n\n")

	for _, batch := range batches {
		hasDone := false
		for _, t := range batch.Tasks {
			if t.State == TaskStateDone {
				hasDone = true
				break
			}
		}
		if !hasDone {
			continue
		}
		b.WriteString(fmt.Sprintf("## %s\n\n", batch.LabelOrID()))
		for _, t := range batch.Tasks {
			if t.State != TaskStateDone {
				continue
			}
			output, _ := wb.ReadOutput(t.ID)
			preview := output
			if len(preview) > 200 {
				preview = preview[:200] + "..."
			}
			b.WriteString(fmt.Sprintf("### %s\n\n", t.Title))
			b.WriteString(fmt.Sprintf("- **Task**: %s\n", shortID(t.ID)))
			b.WriteString(fmt.Sprintf("- **Role**: %s\n", t.Role))
			b.WriteString(fmt.Sprintf("- **Output**:\n\n%s\n\n", preview))
		}
	}

	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("_Generated: %s_\n", time.Now().UTC().Format(time.RFC3339)))
	return b.String()
}

// shortID returns the first 8 characters of a task ID.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// WriteOutput writes the Worker's output to output.md.
func (wb *Whiteboard) WriteOutput(taskID, output string) error {
	return wb.writeFile(filepath.Join(wb.TaskDir(taskID), "output.md"), output)
}

// AppendOutput appends to output.md.
func (wb *Whiteboard) AppendOutput(taskID, output string) error {
	path := filepath.Join(wb.TaskDir(taskID), "output.md")
	existing, _ := wb.readFile(path)
	return wb.writeFile(path, existing+output)
}

// ReadOutput reads the Worker's output from output.md.
func (wb *Whiteboard) ReadOutput(taskID string) (string, error) {
	return wb.readFile(filepath.Join(wb.TaskDir(taskID), "output.md"))
}

// WriteVerifier writes the Verifier's result to verifier.md.
func (wb *Whiteboard) WriteVerifier(taskID, result string) error {
	return wb.writeFile(filepath.Join(wb.TaskDir(taskID), "verifier.md"), result)
}

// ReadVerifier reads the Verifier's result from verifier.md.
func (wb *Whiteboard) ReadVerifier(taskID string) (string, error) {
	return wb.readFile(filepath.Join(wb.TaskDir(taskID), "verifier.md"))
}

// WriteStatus writes status.json with the current task state.
func (wb *Whiteboard) WriteStatus(taskID, status string) error {
	data := map[string]interface{}{
		"status":    status,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}
	payload, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal status: %w", err)
	}
	return wb.writeFile(filepath.Join(wb.TaskDir(taskID), "status.json"), string(payload))
}

// ReadInput reads the original task input from input.md.
func (wb *Whiteboard) ReadInput(taskID string) (string, error) {
	return wb.readFile(filepath.Join(wb.TaskDir(taskID), "input.md"))
}

// ListArtifacts lists files in the artifacts directory.
func (wb *Whiteboard) ListArtifacts(taskID string) ([]string, error) {
	artDir := filepath.Join(wb.TaskDir(taskID), "artifacts")
	entries, err := os.ReadDir(artDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// ClearTask removes the entire task directory tree.
func (wb *Whiteboard) ClearTask(taskID string) error {
	taskDir := wb.TaskDir(taskID)
	if _, err := os.Stat(taskDir); os.IsNotExist(err) {
		return nil
	}
	return os.RemoveAll(taskDir)
}

// ---------------------------------------------------------------------------
// Message bus — Agent-to-Agent / Human-to-Agent communication
// ---------------------------------------------------------------------------

// InboxDir returns the inbox directory for a task.
func (wb *Whiteboard) InboxDir(taskID string) string {
	return filepath.Join(wb.TaskDir(taskID), "inbox")
}

// OutboxDir returns the outbox directory for a task.
func (wb *Whiteboard) OutboxDir(taskID string) string {
	return filepath.Join(wb.TaskDir(taskID), "outbox")
}

// WriteMessage writes a message to the target task's inbox.
// It also writes a copy to the sender's outbox.
func (wb *Whiteboard) WriteMessage(toTaskID string, msg Message) error {
	payload, err := json.MarshalIndent(msg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}

	// Write to recipient's inbox.
	inboxDir := wb.InboxDir(toTaskID)
	if err := os.MkdirAll(inboxDir, 0755); err != nil {
		return fmt.Errorf("create inbox dir: %w", err)
	}
	filename := fmt.Sprintf("%s_from_%s.json", time.Now().UTC().Format("20060102-150405.000000000"), safeSenderName(msg.From))
	if err := wb.writeFile(filepath.Join(inboxDir, filename), string(payload)); err != nil {
		return fmt.Errorf("write inbox message: %w", err)
	}

	// Also write to sender's outbox (if sender is an agent task).
	if msg.From != "" && msg.From != "human" && msg.From != "system" {
		outboxDir := wb.OutboxDir(toTaskID)
		_ = os.MkdirAll(outboxDir, 0755)
		_ = wb.writeFile(filepath.Join(outboxDir, "to_"+safeSenderName(msg.To)+"_"+filename), string(payload))
	}

	return nil
}

// ReadInbox reads all unread messages from a task's inbox.
// After reading, messages are not deleted — agents track "seen" via state.
// Returns messages sorted by creation time.
func (wb *Whiteboard) ReadInbox(taskID string) ([]Message, error) {
	inboxDir := wb.InboxDir(taskID)
	entries, err := os.ReadDir(inboxDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read inbox: %w", err)
	}

	var messages []Message
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(inboxDir, e.Name()))
		if err != nil {
			continue
		}
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		messages = append(messages, msg)
	}

	// Sort by creation time.
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].CreatedAt < messages[j].CreatedAt
	})

	return messages, nil
}

// BuildInboxContext builds a formatted string of inbox messages for inclusion
// in an agent's task description. Returns empty string when inbox is empty.
func (wb *Whiteboard) BuildInboxContext(taskID string) (string, error) {
	messages, err := wb.ReadInbox(taskID)
	if err != nil {
		return "", err
	}
	if len(messages) == 0 {
		return "", nil
	}

	var b strings.Builder
	b.WriteString("\n\n## 📨 Inbox Messages\n")
	b.WriteString("You have received the following messages. Read them and respond if needed.\n\n")
	for _, msg := range messages {
		b.WriteString(fmt.Sprintf("### From: %s\n", msg.From))
		b.WriteString(fmt.Sprintf("Time: %s\n", msg.CreatedAt))
		b.WriteString(fmt.Sprintf("Message:\n%s\n\n", msg.Content))
	}

	return b.String(), nil
}

// safeSenderName converts a sender identity into a filesystem-safe name.
func safeSenderName(name string) string {
	r := strings.NewReplacer(
		":", "_",
		"/", "_",
		"\\", "_",
		" ", "_",
		"<", "_",
		">", "_",
		"|", "_",
		"\"", "_",
		"*", "_",
	)
	return r.Replace(name)
}

// ---------------------------------------------------------------------------

	// WriteInboxFile writes the structured inbox.md for a task.
	// Contains: task description, upstream output file references, template,
	// self-split instructions, and retry feedback — all as file paths,
	// not inline content.
	func (wb *Whiteboard) WriteInboxFile(taskID string, params InboxParams) error {
		taskDir := wb.TaskDir(taskID)
		if err := os.MkdirAll(taskDir, 0755); err != nil {
			return fmt.Errorf("create task dir: %w", err)
		}
		if err := os.MkdirAll(filepath.Join(taskDir, "artifacts"), 0755); err != nil {
			return fmt.Errorf("create artifacts dir: %w", err)
		}

		var b strings.Builder
		b.WriteString(fmt.Sprintf("# %s\n\n", params.Title))
		b.WriteString(fmt.Sprintf("**角色**: %s\n\n", params.Role))
		b.WriteString("---\n\n")

		b.WriteString("## 📋 任务描述\n\n")
		b.WriteString(params.Description)
		b.WriteString("\n\n")

		if len(params.UpstreamOutputs) > 0 {
			b.WriteString("## 📥 上游产出（请先阅读）\n\n")
			for _, uo := range params.UpstreamOutputs {
				b.WriteString(fmt.Sprintf("- [%s](%s)\n", uo.Name, uo.Path))
			}
			b.WriteString("\n")
		}

		if params.Output != "" {
			b.WriteString("## 📤 产出文件\n\n")
			b.WriteString(fmt.Sprintf("请将最终产出写入: `%s`\n\n", params.Output))
		}

		if params.Template != "" {
			b.WriteString("## 📄 产出模板\n\n")
			b.WriteString(params.Template)
			b.WriteString("\n\n")
		}

		if params.Memory != "" {
			b.WriteString("## 🧠 团队记忆\n\n")
			b.WriteString(params.Memory)
			b.WriteString("\n\n")
		}

		if params.AllowSelfSplit {
			b.WriteString("## 🔀 自拆分\n\n")
			b.WriteString("评估: 如果产出预计超过 ~200 行或 ~10 个章节，请拆分。\n\n")
			b.WriteString("如需拆分: 不要产出部分内容。在产出开头输出 `[SPLIT_PLAN]`，后跟 JSON 数组（2-3 个子任务）。\n")
			b.WriteString("每个子任务含: title, description, role, output 字段。\n\n")
		}

		if params.RetryFeedback != "" {
			b.WriteString("## 🔄 上一轮审查反馈\n\n")
			b.WriteString(params.RetryFeedback)
			b.WriteString("\n\n")
		}

		return wb.writeFile(filepath.Join(taskDir, "input.md"), b.String())
	}

// Internal helpers
// ---------------------------------------------------------------------------

func (wb *Whiteboard) writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0644)
}

func (wb *Whiteboard) readFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(data), nil
}

// AppendTaskOutput appends a line to the task's output.md file.
// Convenience method used during streaming.
func (wb *Whiteboard) AppendTaskOutput(taskID, line string) error {
	path := filepath.Join(wb.TaskDir(taskID), "output.md")
	existing, _ := wb.readFile(path)
	if existing == "" {
		return wb.writeFile(path, line)
	}
	return wb.writeFile(path, existing+"\n"+line)
}

// CopyArtifact copies content from srcPath inside the workspace into the
// task's artifacts directory with the given filename.
func (wb *Whiteboard) CopyArtifact(taskID, filename, content string) error {
	dest := filepath.Join(wb.TaskDir(taskID), "artifacts", filename)
	// Basic path traversal prevention.
	if strings.Contains(filename, "..") || strings.Contains(filename, "/") || strings.Contains(filename, "\\") {
		return fmt.Errorf("invalid artifact filename: %s", filename)
	}
	return wb.writeFile(dest, content)
}

	// CleanupTask removes all whiteboard files for a single task.
	func (wb *Whiteboard) CleanupTask(taskID string) error {
		return os.RemoveAll(wb.TaskDir(taskID))
	}

	// CleanupMasterTask removes board, deliverable, and all subtask directories.
	func (wb *Whiteboard) CleanupMasterTask(masterTaskID string, subtaskIDs []string) {
		_ = os.Remove(wb.BoardPath())
		_ = os.Remove(wb.DeliverablePath())
		_ = os.Remove(filepath.Join(wb.baseDir, "deliverable.json"))
		for _, id := range subtaskIDs {
			_ = os.RemoveAll(wb.TaskDir(id))
		}
	}
