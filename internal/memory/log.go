package memory

import "github.com/usewhale/whale/internal/core"

type RewriteReason string

const (
	RewriteReasonCompact  RewriteReason = "compact"
	RewriteReasonRecovery RewriteReason = "recovery"
)

// maxHistoryToolCallInputBytes caps how much of a tool_call's input payload is
// kept in the session history. A write/read tool_call whose input embeds the
// full file content is re-sent to the model on EVERY subsequent turn (the loop
// replays all history), so a single such call can dominate the prompt budget —
// e.g. a 383-line file written via `write` produced a ~159 KB tool_call input
// that was replayed ~17 times, ~2M tokens of pure history. Truncating the
// oversized input in history keeps the loop honest (the model can re-read the
// file via read_file if it needs the content) while collapsing that cost.
//
// Only inputs strictly above this threshold are touched, so ordinary small
// tool calls are left byte-for-byte unchanged.
const maxHistoryToolCallInputBytes = 24 * 1024

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

// truncateOversizedToolCallInput collapses any tool_call in the message whose
// input payload exceeds maxHistoryToolCallInputBytes into a compact head+tail
// summary, so the giant payload is not replayed every turn. It is a no-op for
// most messages (small tool_calls, text-only messages), so it never changes
// normal conversation semantics.
func truncateOversizedToolCallInput(msg core.Message) core.Message {
	if len(msg.ToolCalls) == 0 {
		return msg
	}
	var changed bool
	out := msg
	out.ToolCalls = append([]core.ToolCall(nil), msg.ToolCalls...)
	for i, tc := range out.ToolCalls {
		if len(tc.Input) <= maxHistoryToolCallInputBytes {
			continue
		}
		out.ToolCalls[i].Input = truncateToolCallInput(tc.Input)
		changed = true
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
