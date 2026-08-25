package memory

import "github.com/usewhale/whale/internal/core"

type RewriteReason string

const (
	RewriteReasonCompact  RewriteReason = "compact"
	RewriteReasonRecovery RewriteReason = "recovery"
)

// maxHistoryToolCallInputBytes caps how much of a tool_call input / tool result
// text is kept in the session history. A write/read/shell tool whose payload
// embeds a large output is re-sent to the model on EVERY subsequent turn (the
// loop replays all history), so a handful of such calls dominate the prompt
// budget. read_file/shell already truncate to ~16 KB at the tool layer; this
// budget (16 KB) collapses those too, so a worker that reads many files does not
// replay every file's content every turn. Only payloads above the budget are
// touched, so small tool calls stay byte-for-byte unchanged.
const maxHistoryToolCallInputBytes = 16 * 1024

type AppendOnlyLog struct {
	entries []core.Message
}

func NewAppendOnlyLog() *AppendOnlyLog {
	return &AppendOnlyLog{entries: make([]core.Message, 0)}
}

func (l *AppendOnlyLog) Append(msg core.Message) {
	l.entries = append(l.entries, truncateOversizedToolCallInput(cloneMessage(msg)))
}

func (l *AppendOnlyLog) Extend(msgs []core.Message) {
	for _, m := range msgs {
		l.Append(m)
	}
}

func (l *AppendOnlyLog) Entries() []core.Message {
	return cloneMessages(l.entries)
}

func (l *AppendOnlyLog) RewriteWithReason(reason RewriteReason, replacement []core.Message) bool {
	switch reason {
	case RewriteReasonCompact, RewriteReasonRecovery:
	default:
		return false
	}
	l.entries = cloneMessages(replacement)
	return true
}

func (l *AppendOnlyLog) Len() int {
	return len(l.entries)
}

// truncateOversizedToolCallInput collapses oversized tool_call inputs AND
// oversized tool_result text in a message, so giant payloads are not replayed
// to the model every turn. It is a no-op for small messages, so normal
// conversation semantics are unchanged.
func truncateOversizedToolCallInput(msg core.Message) core.Message {
	var changed bool
	out := msg
	if len(msg.ToolCalls) > 0 {
		out.ToolCalls = append([]core.ToolCall(nil), msg.ToolCalls...)
		for i, tc := range out.ToolCalls {
			if len(tc.Input) <= maxHistoryToolCallInputBytes {
				continue
			}
			out.ToolCalls[i].Input = truncateToolCallInput(tc.Input)
			changed = true
		}
	}
	if len(msg.ToolResults) > 0 {
		out.ToolResults = append([]core.ToolResult(nil), msg.ToolResults...)
		for i, tr := range out.ToolResults {
			if len(tr.ModelText) <= maxHistoryToolCallInputBytes {
				continue
			}
			out.ToolResults[i].ModelText = truncateToolResultText(tr.ModelText)
			changed = true
		}
	}
	if !changed {
		return msg
	}
	return out
}

// truncateToolCallInput keeps the head and tail of an oversized tool_call
// input and flags the elision, so the model still sees the tool name and any
// leading/trailing context (e.g. a file_path prefix) without the bulk.
func truncateToolCallInput(input string) string {
	const head = 2048
	const tail = 1024
	if len(input) <= head+tail {
		return input
	}
	return input[:head] + "\n…[tool_call input truncated: " +
		itoa(len(input)) + " bytes]…\n" + input[len(input)-tail:]
}

// truncateToolResultText keeps the head and tail of an oversized tool result
// text (shell/read output) and flags the elision, so the model still sees the
// leading output without the bulk being replayed every turn.
func truncateToolResultText(text string) string {
	const head = 2048
	const tail = 512
	if len(text) <= head+tail {
		return text
	}
	return text[:head] + "\n…[tool result truncated: " +
		itoa(len(text)) + " bytes]…\n" + text[len(text)-tail:]
}

// itoa is a tiny decimal formatter (avoids importing strconv for a hot path).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
