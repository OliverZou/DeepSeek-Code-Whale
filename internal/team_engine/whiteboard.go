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
//	闁宠澹曢弨銏ゅ煘閳?input.md        # Leader 闁告劖鐟ラ崣鍡涙儍閸曨亝宕查柛鏂哄墲瀵寧娼?+ 濞戞挸锕ｇ粭鍛村棘?
//	闁宠澹曢弨銏ゅ煘閳?output.md       # Worker 闁告劖鐟ラ崣鍡涙儍閸曨亪鐛撻柛?
//	闁宠澹曢弨銏ゅ煘閳?verifier.md     # Verifier 闁告劖鐟ラ崣鍡涙儍閸曨剦姊鹃柡灞诲劤缁劑寮?
//	闁宠澹曢弨銏ゅ煘閳?status.json     # 鐟滅増鎸告晶鐘绘偐閼哥鍋撴担绋垮笚闁轰胶澧楀畵?
//	闁宠澹曢弨銏ゅ煘閳?inbox/          # Agent 闂傚倹鎸抽埀顒佷亢椤斿棝鏁嶅顒€绲虹紓浣圭懄濠€?Agent 闁汇劌瀚粔鐑藉箒椤栥倗绀勯柣銏犲綁缁剚绂嶉崫鍕櫢闁稿繈鍎荤槐?
//	闁?  闁宠澹曢弨銏ゅ煘閳?001_from_human.json
//	闁?  闁宠鏌￠弨銏ゅ煘閳?002_from_agent-B.json
//	闁宠澹曢弨銏ゅ煘閳?outbox/         # Agent 闂傚倹鎸抽埀顒佷亢椤斿棝鏁嶅顓熸嫳 Agent 闁告瑦鍨甸崵顓㈡儍閸曨剛啸闁?
//	闁?  闁宠鏌￠弨銏ゅ煘閳?001_to_agent-C.json
//	闁宠鏌￠弨銏ゅ煘閳?artifacts/      # Worker 濞存籂鍐ㄦ瘔闁汇劌瀚崣鎸庢媴閹惧瓨鐎ù鐘侯啇缁辨瑦绂掗敐鍥╁灣缂佹稑顧€缁?
//
// Agent 闂傚倹鎸抽埀顒佷亢椤斿棝宕㈤悢宄扮仧闁挎稑鐗呯粭灞剧閾忕顫﹂柛姘湰濞煎牓鏁嶆径娑氱獥
//   - 濞寸姾顔婄紞宥呫€掗悩璁冲闁挎稑鐗呭Ч澶岀尵濮瑰洠鍋撴稉鍒nt闁靛棔绗抧gine闁挎稑顦甸崗姗€宕ｉ娆庣鞍闂侇偅淇虹换鍐磼閻斿墎顏遍柣?prompt/spawn/abort/kill
//     闁规亽鍎辫ぐ娑㈠箼瀹ュ嫮绋?Agent
//   - Agent 濞戞柨顑夊Λ鍧楀矗椤栨瑤绨伴柛宥呯箣濮瑰鐚剧拋宕囶伇闁哄秶鏌夌换妯兼偘鐏炵瓔妯嬮弶鐑嗗枙濮橈附绂嶉幒鐐电闁告牕鎳忕€氼厽绋夌拠鎻捫楅柟鎭掑姂閳ь兛绀侀幏浼村箰婢舵劖浠橀柡灞诲劥椤?
//   - 婵炴垵鐗婃导鍛偓娑櫭崑宥夊捶?inbox/outbox 濞戞搩鍙忕槐婊眊ent 闁告凹鍨版慨鈺呭籍閹澘娈伴柛鏂诲姀椤曚即宕ｉ弽銊﹀紦閻犲洨绮粔鐑藉箒?
//   - Agent 闂侇偅淇虹换鍐冀閸パ冩珯鐎规悶鍎遍崣璺ㄦ嫚鐠囨彃鏅告繛鎴濈墛娴煎懘鏁嶇仦鑲╃憿濞存粏娅ｇ悮顐︽儍閸曨亝鍞夊ù婊勫笚閺岀喎顕ｈ箛鎾舵殮闁稿繈鍔嬬粩鎾嚊?
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
	baseDir  string
	taskSessionID string // set via SetTaskSession to scope under a master task
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

// SetTaskSession scopes subsequent Whiteboard operations under the given master task.
// TaskDir, global files (board.md, deliverable.md), etc. are written under
// baseDir/taskSessionID/ instead of baseDir/.
func (wb *Whiteboard) SetTaskSession(taskSessionID string) {
	wb.taskSessionID = taskSessionID
}

// taskRoot returns the root directory for task-level files.
func (wb *Whiteboard) taskRoot() string {
	if wb.taskSessionID != "" {
		return filepath.Join(wb.baseDir, wb.taskSessionID)
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

// TaskSessionDir returns the directory for per-master-task artifacts
// (plan.json, plan.md, spec.md, board.md, deliverable.md, logs).
// InitTask creates the task directory and writes input.md.
// NOTE: does NOT overwrite status.json 闁?the caller is responsible for
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
// Board 闁?闁稿繈鍔岄惇顒佹交濞戞ê顔婇柣褑濮ゅ?(board.md) 闁告粌濂斿锔界濡厧鈷栨慨鐟版处閳?(deliverable.md)
// ---------------------------------------------------------------------------

// BoardPath returns the path to the global board.md file.
func (wb *Whiteboard) BoardPath() string {
	return filepath.Join(wb.taskRoot(), "board.md")
}

// DeliverablePath returns the path to the global deliverable.md file.
func (wb *Whiteboard) DeliverablePath() string {
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

// TaskSessionDir returns the directory for a master task.
func (wb *Whiteboard) TaskSessionDir(taskSessionID string) string {
	return filepath.Join(wb.baseDir, taskSessionID)
}

// ChatDir returns the chat directory for a master task.
func (wb *Whiteboard) ChatDir(taskSessionID string) string {
	return filepath.Join(wb.TaskSessionDir(taskSessionID), "chat")
}

// WriteChatMessage writes a chat message to the master task's chat directory.
func (wb *Whiteboard) WriteChatMessage(taskSessionID, from, to, content string) error {
	chatDir := wb.ChatDir(taskSessionID)
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
func (wb *Whiteboard) ReadChatMessages(taskSessionID string) ([]ChatMessage, error) {
	chatDir := wb.ChatDir(taskSessionID)
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
// Message bus 闁?Agent-to-Agent / Human-to-Agent communication
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
// After reading, messages are not deleted 闁?agents track "seen" via state.
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
	b.WriteString("\n\n## 妫ｅ啯鎲?Inbox Messages\n")
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
	// self-split instructions, and retry feedback 闁?all as file paths,
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
		b.WriteString(fmt.Sprintf("**閻熸瑦甯熸竟?*: %s\n\n", params.Role))
		b.WriteString("---\n\n")

		b.WriteString("## 妫ｅ啯鎯?濞寸姾顕ф慨鐔煎箵韫囨艾鐗歕n\n")
		b.WriteString(params.Description)
		b.WriteString("\n\n")

		if len(params.UpstreamOutputs) > 0 {
			b.WriteString("## 妫ｅ啯鎲?濞戞挸锕ラ悥鑸电瑜嶉崵顓㈡晬閸綆鍤為柛蹇撶墦濡插嫮鎷犳导娆戠\n\n")
			for _, uo := range params.UpstreamOutputs {
				b.WriteString(fmt.Sprintf("- [%s](%s)\n", uo.Name, uo.Path))
			}
			b.WriteString("\n")
		}

		if params.Template != "" {
			b.WriteString("## 妫ｅ啯鎯?濞存籂鍐ㄦ瘔婵☆垪鍓濆姒巒\n")
			b.WriteString(params.Template)
			b.WriteString("\n\n")
		}

		if params.Memory != "" {
			b.WriteString("## 妫ｅ喚娼?闁搞儯鍨藉Σ锔炬媼閺夎法绠揬n\n")
			b.WriteString(params.Memory)
			b.WriteString("\n\n")
		}


		if params.RetryFeedback != "" {
			b.WriteString("## 妫ｅ啯鏁?濞戞挸锕ｇ粩瀛樻姜椤旂⒈鍚€闁哄被鍎卞鑺ワ純閸︾单\n")
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

	// CleanupTaskSession removes board, deliverable, and all subtask directories.
	func (wb *Whiteboard) CleanupTaskSession(taskSessionID string, subtaskIDs []string) {
		_ = os.Remove(wb.BoardPath())
		_ = os.Remove(wb.DeliverablePath())
		_ = os.Remove(filepath.Join(wb.baseDir, "deliverable.json"))
		for _, id := range subtaskIDs {
			_ = os.RemoveAll(wb.TaskDir(id))
		}
	}
