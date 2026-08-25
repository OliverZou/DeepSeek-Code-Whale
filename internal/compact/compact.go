package compact

import (
	"fmt"
	"strings"

	"github.com/usewhale/whale/internal/core"
)

const (
	// 重放压缩阈值：超过即把工具结果压成 head+tail 摘要再发给模型。
	// 收紧到 4KB/600 token（原 12KB/2000）：会话历史在每轮全量重发，工具结果
	// 是上下文增长的主源（v34 fix 会话 82KB 重放、毛 token 1.45M 的 98% 是缓存
	// 命中但毛值虚高）；4KB 以上（game.js/style.css 全文、bash 输出）即压缩，
	// 历史增长放缓，毛 token 显著下降。Full raw 仍在 Whale 会话历史中。
	maxToolResultReplayTokens    = 600
	maxToolResultReplayChars     = 4 * 1024
	compactedToolResultKeepRunes = 1500
)

func EstimateMessagesTokens(msgs []core.Message) int {
	total := 0
	for _, m := range msgs {
		total += EstimateTokens(core.MessagePlainText(m))
		total += EstimateTokens(m.Reasoning)
		for _, tc := range m.ToolCalls {
			total += EstimateTokens(tc.Name) + EstimateTokens(tc.Input)
		}
		for _, tr := range m.ToolResults {
			total += EstimateTokens(tr.Name) + EstimateTokens(core.ToolResultModelText(tr))
		}
	}
	return total
}

func EstimateTokens(s string) int {
	if strings.TrimSpace(s) == "" {
		return 0
	}
	// Rough estimator: ASCII runs are compressed, CJK/non-ASCII is near 1 rune/token.
	asciiRunes := 0
	nonASCII := 0
	for _, r := range s {
		if r < 128 {
			asciiRunes++
		} else {
			nonASCII++
		}
	}
	asciiTok := (asciiRunes + 3) / 4
	return asciiTok + nonASCII
}

func ToolResultReplayContent(content string) string {
	estimatedTokens := EstimateTokens(content)
	if estimatedTokens <= maxToolResultReplayTokens && len(content) <= maxToolResultReplayChars {
		return content
	}
	runes := []rune(content)
	if len(runes) <= compactedToolResultKeepRunes {
		return content
	}
	headRunes := compactedToolResultKeepRunes / 2
	tailRunes := compactedToolResultKeepRunes - headRunes
	head := string(runes[:headRunes])
	tail := string(runes[len(runes)-tailRunes:])
	return fmt.Sprintf(
		"[tool result compacted for model replay]\n"+
			"original_estimated_tokens=%d original_chars=%d retained_head_runes=%d retained_tail_runes=%d\n"+
			"Full raw tool result remains in Whale session history; this provider replay is abbreviated.\n\n"+
			"--- head ---\n%s\n\n"+
			"--- omitted ---\n[... omitted %d runes from tool result replay ...]\n\n"+
			"--- tail ---\n%s",
		estimatedTokens,
		len(content),
		headRunes,
		tailRunes,
		head,
		len(runes)-headRunes-tailRunes,
		tail,
	)
}
