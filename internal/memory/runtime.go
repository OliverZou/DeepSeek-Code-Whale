package memory

import (
	"strings"

	"github.com/usewhale/whale/internal/core"
)

type RuntimeState struct {
	Prefix        *ImmutablePrefix
	runtimeBlocks []string
	Log           *AppendOnlyLog
	Scratch       *VolatileScratch
}

func NewRuntimeState(prefix *ImmutablePrefix) *RuntimeState {
	if prefix == nil {
		prefix = NewImmutablePrefix(nil)
	}
	return &RuntimeState{
		Prefix:  prefix,
		Log:     NewAppendOnlyLog(),
		Scratch: NewVolatileScratch(),
	}
}

func (r *RuntimeState) SetRuntimeBlocks(blocks []string) {
	if r == nil {
		return
	}
	r.runtimeBlocks = append([]string(nil), blocks...)
}

func (r *RuntimeState) RuntimeBlocks() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.runtimeBlocks...)
}

func (r *RuntimeState) BuildProviderHistory() []core.Message {
	out := make([]core.Message, 0, 2+r.Log.Len())
	out = append(out, r.Prefix.ToMessages()...)
	if len(r.runtimeBlocks) > 0 {
		out = append(out, core.Message{
			Role: core.RoleSystem,
			Text: strings.Join(r.runtimeBlocks, "\n\n"),
		})
	}
	// The log entries surface to the provider as the turn history. Truncate any
	// oversized tool_call input here — not just at Append time — so history
	// restored from a persisted session (retry/continue via SessionOps) is also
	// compressed before every provider call. Otherwise a giant `write` input is
	// replayed every turn (see AppendOnlyLog.Append truncation for the budget).
	for _, msg := range r.Log.Entries() {
		out = append(out, truncateOversizedToolCallInput(msg))
	}
	return out
}
