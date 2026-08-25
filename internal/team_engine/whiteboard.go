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
//
// InboxParams carries all inputs needed to write a task's inbox.md.
type InboxParams struct {
	Title              string
	Role               string
	Description        string
	Output             string
	AcceptanceCriteria []string
	UpstreamOutputs    []UpstreamRef
	Template           string
	Memory             string
	AllowSelfSplit     bool
	RetryFeedback      string
}

// UpstreamRef is a named file reference to an upstream output.
type UpstreamRef struct {
	Name string
	Path string
}

type Whiteboard struct {
	baseDir  string
	masterID string // set via SetMaster to scope under a master task
}

// NewWhiteboard creates a Whiteboard rooted at baseDir.
// 惰性创建：目录只在首次写入时产生（各写方法已各自 MkdirAll 保底）；
// 只读命令（status/list/analyze）不应在未执行任务的位置留下 .whale 目录。
func NewWhiteboard(baseDir string) (*Whiteboard, error) {
	abs, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("resolve base dir: %w", err)
	}
	return &Whiteboard{baseDir: abs}, nil
}

// SetMaster scopes subsequent Whiteboard operations under the given master task.
// TaskDir, global files (board.md, deliverable.md), etc. are written under
// baseDir/masterID/ instead of baseDir/.
func (wb *Whiteboard) SetMaster(masterID string) {
	wb.masterID = masterID
}

// taskRoot returns the root directory for task-level files.
func (wb *Whiteboard) taskRoot() string {
	if wb.masterID != "" {
		return filepath.Join(wb.baseDir, wb.masterID)
	}
	return wb.baseDir
}

// BaseDir returns the absolute path to the whiteboard root.
func (wb *Whiteboard) BaseDir() string {
	return wb.baseDir
}

// TaskDir returns the directory for a given task.
func (wb *Whiteboard) TaskDir(taskID string) string {
	return filepath.Join(wb.baseDir, taskID)
}

// MasterDir returns the directory for per-master-task artifacts
// (plan.json, plan.md, spec.md, board.md, deliverable.md, logs).
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

// BoardPath returns the path to the board.md file. When a master is set it
// lives in the master dir (v47: per-run board, not a shared root file).
func (wb *Whiteboard) BoardPath() string {
	if wb.masterID != "" {
		return filepath.Join(wb.MasterDir(wb.masterID), "board.md")
	}
	return filepath.Join(wb.taskRoot(), "board.md")
}

// DeliverablePath returns the path to the deliverable.md file (master dir
// when a master is set — v47).
func (wb *Whiteboard) DeliverablePath() string {
	if wb.masterID != "" {
		return filepath.Join(wb.MasterDir(wb.masterID), "deliverable.md")
	}
	return filepath.Join(wb.taskRoot(), "deliverable.md")
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
	if err := wb.writeFile(filepath.Join(wb.TaskDir(taskID), "output.md"), output); err != nil {
		return err
	}
	Log("whiteboard", "task %s wrote output.md (%d bytes)", shortID(taskID), len(output))
	return nil
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
	if err := wb.writeFile(filepath.Join(wb.TaskDir(taskID), "verifier.md"), result); err != nil {
		return err
	}
	Log("whiteboard", "task %s wrote verifier.md (%d bytes, verdict=%v)", shortID(taskID), len(result), strings.Contains(result, "VERDICT: PASS"))
	return nil
}

// ReadVerifier reads the Verifier's result from verifier.md.
func (wb *Whiteboard) ReadVerifier(taskID string) (string, error) {
	return wb.readFile(filepath.Join(wb.TaskDir(taskID), "verifier.md"))
}

// WriteDelivered persists the files a worker actually delivered (relative to
// the task workdir), one per line — consumed by completion events so the
// inline leader can render clickable deliverable links in its narration.
func (wb *Whiteboard) WriteDelivered(taskID string, files []string) error {
	if len(files) == 0 {
		return nil
	}
	if err := wb.writeFile(filepath.Join(wb.TaskDir(taskID), "delivered.txt"), strings.Join(files, "\n")); err != nil {
		return err
	}
	Log("whiteboard", "task %s wrote delivered.txt (%d files)", shortID(taskID), len(files))
	return nil
}

// ReadDelivered reads the delivered-file list written by WriteDelivered.
func (wb *Whiteboard) ReadDelivered(taskID string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(wb.TaskDir(taskID), "delivered.txt"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// WriteConfirmation writes the agent's confirmation request.
func (wb *Whiteboard) WriteConfirmation(taskID, content string) error {
	return wb.writeFile(filepath.Join(wb.TaskDir(taskID), "confirmation.md"), content)
}

// ReadConfirmation reads the agent's confirmation request.
func (wb *Whiteboard) ReadConfirmation(taskID string) (string, error) {
	return wb.readFile(filepath.Join(wb.TaskDir(taskID), "confirmation.md"))
}

// ClearConfirmation removes the confirmation file after it's been handled.
func (wb *Whiteboard) ClearConfirmation(taskID string) error {
	return os.Remove(filepath.Join(wb.TaskDir(taskID), "confirmation.md"))
}

// HasConfirmation reports whether a confirmation request exists for a task.
func (wb *Whiteboard) HasConfirmation(taskID string) bool {
	_, err := os.Stat(filepath.Join(wb.TaskDir(taskID), "confirmation.md"))
	return err == nil
}

// MasterDir returns the directory for a master task.
func (wb *Whiteboard) MasterDir(masterTaskID string) string {
	return filepath.Join(wb.baseDir, masterTaskID)
}

// ChatDir returns the chat directory for a master task.
func (wb *Whiteboard) ChatDir(masterTaskID string) string {
	return filepath.Join(wb.MasterDir(masterTaskID), "chat")
}

// WriteChatMessage writes a chat message to the master task's chat directory.
func (wb *Whiteboard) WriteChatMessage(masterTaskID, from, to, content string) error {
	chatDir := wb.ChatDir(masterTaskID)
	if err := os.MkdirAll(chatDir, 0755); err != nil {
		return fmt.Errorf("create chat dir: %w", err)
	}
	seq := time.Now().UTC().Format("20060102-150405.000000000")
	meta := fmt.Sprintf("---\nfrom: %s\nto: %s\ntimestamp: %s\n---\n\n%s", from, to, time.Now().UTC().Format(time.RFC3339), content)
	filename := fmt.Sprintf("%s_%s.md", seq, safeSenderName(from))
	return wb.writeFile(filepath.Join(chatDir, filename), meta)
}

// ChatMessage represents a single chat message in the master task conversation.
type ChatMessage struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
	Filename  string `json:"filename"`
}

// ReadChatMessages reads all chat messages for a master task, sorted by time.
func (wb *Whiteboard) ReadChatMessages(masterTaskID string) ([]ChatMessage, error) {
	chatDir := wb.ChatDir(masterTaskID)
	entries, err := os.ReadDir(chatDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read chat dir: %w", err)
	}
	var messages []ChatMessage
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(chatDir, e.Name()))
		if err != nil {
			continue
		}
		msg := ChatMessage{Filename: e.Name()}
		content := string(data)
		if strings.HasPrefix(content, "---") {
			end := strings.Index(content[3:], "---")
			if end > 0 {
				frontmatter := content[3 : end+3]
				body := strings.TrimSpace(content[end+6:])
				for _, line := range strings.Split(frontmatter, "\n") {
					line = strings.TrimSpace(line)
					if strings.HasPrefix(line, "from:") {
						msg.From = strings.TrimSpace(strings.TrimPrefix(line, "from:"))
					} else if strings.HasPrefix(line, "to:") {
						msg.To = strings.TrimSpace(strings.TrimPrefix(line, "to:"))
					} else if strings.HasPrefix(line, "timestamp:") {
						msg.Timestamp = strings.TrimSpace(strings.TrimPrefix(line, "timestamp:"))
					}
				}
				msg.Content = body
			} else {
				msg.Content = content
			}
		} else {
			msg.Content = content
		}
		messages = append(messages, msg)
	}
	sort.Slice(messages, func(i, j int) bool {
		return messages[i].Filename < messages[j].Filename
	})
	return messages, nil
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

	if len(params.AcceptanceCriteria) > 0 {
		b.WriteString("## ✅ 验收标准（交付前逐项自检核对）\n\n")
		for i, c := range params.AcceptanceCriteria {
			b.WriteString(fmt.Sprintf("%d. %s\n", i+1, c))
		}
		b.WriteString("\n")
	}

	if len(params.UpstreamOutputs) > 0 {
		b.WriteString("## 📥 上游产出（请先阅读）\n\n")
		for _, uo := range params.UpstreamOutputs {
			b.WriteString(fmt.Sprintf("- [%s](%s)\n", uo.Name, uo.Path))
		}
		b.WriteString("\n")
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

	if params.RetryFeedback != "" {
		b.WriteString("## 🔄 上一轮审查反馈\n\n")
		b.WriteString(params.RetryFeedback)
		b.WriteString("\n\n")
	}
	if err := wb.writeFile(filepath.Join(taskDir, "input.md"), b.String()); err != nil {
		return err
	}
	Log("whiteboard", "task %s wrote input.md (%d chars, retry_feedback=%v)", shortID(taskID), b.Len(), params.RetryFeedback != "")
	return nil
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
