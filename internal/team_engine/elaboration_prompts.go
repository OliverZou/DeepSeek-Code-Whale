package team_engine

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Phase 0: Goal Elaboration — prompt templates
//
// Optimised 3-step process:
//   Step 1+2 — merged completeness check + domain research (single call)
//   Step 3   — spec production (YAML, only when gaps found)
//   Step 4   — user confirmation (reserved)
// ---------------------------------------------------------------------------

// CheckAndResearchPrompt returns the merged Step 1+2 prompt.
// The LLM acts as both completeness checker and domain-knowledge filler:
// judge 6 dimensions (OK/GAP), and for GAP dimensions, provide objective
// domain facts.  Returns strict JSON — no reasoning chain.
//
// Prompt is in Chinese for better results from DeepSeek on semantic judgement.
func CheckAndResearchPrompt(rawGoal string) string {
	return fmt.Sprintf(`你是规格完整性检查器+领域知识填充器。判断目标在6个维度是否完整。
能从常识推断的判OK。仅当真正需用户澄清才判GAP。对GAP维度给出客观领域事实（不做设计决策）。

维度：1.范围 2.接口 3.行为 4.质量 5.依赖 6.约束

输出严格JSON（无其他文字）：
{
  "verdict": "COMPLETE",
  "complexity": "simple",
  "estimated_tasks": 1,
  "dimensions": [
    {"name":"范围","status":"OK","detail":""},
    {"name":"接口","status":"OK","detail":""},
    {"name":"行为","status":"OK","detail":""},
    {"name":"质量","status":"OK","detail":""},
    {"name":"依赖","status":"OK","detail":""},
    {"name":"约束","status":"OK","detail":""}
  ],
  "domain_facts": {
    "domain_overview": "",
    "common_scope": [],
    "typical_interface": "",
    "quality_benchmarks": [],
    "explicitly_out_of_scope": []
  }
}
注：verdict=COMPLETE时domain_facts全留空；INCOMPLETE时必须填充。
complexity评估目标规模：simple(1个文件/单关注点，≤2任务)、medium(多关注点但独立，3-5任务)、complex(多模块/需形式化推理/重构，6+任务)。estimated_tasks给粗略任务数。

目标：%s`, rawGoal)
}

// SpecProductionPrompt returns the Step 3 prompt template.
// The LLM acts as a TeamLeader producing a fully-specified YAML spec
// by combining the raw goal + domain research + identified gaps.
func SpecProductionPrompt(rawGoal string, research *DomainResearch, gaps []DimensionGap) string {
	var sb strings.Builder

	sb.WriteString(`你是TeamLeader。从raw goal生成完整可执行spec。不要分解成任务——只输出spec。

`)

	sb.WriteString(fmt.Sprintf("RAW GOAL: %s\n\n", rawGoal))

	if len(gaps) > 0 {
		sb.WriteString("已识别的缺失维度：\n")
		for _, g := range gaps {
			sb.WriteString(fmt.Sprintf("  - %s: %s\n", g.Name, g.Detail))
		}
		sb.WriteString("\n")
	}

	if research != nil {
		sb.WriteString("领域知识（基于这些事实，不要矛盾）：\n")
		sb.WriteString(fmt.Sprintf("  概述: %s\n", research.DomainOverview))
		if len(research.CommonScope) > 0 {
			sb.WriteString(fmt.Sprintf("  典型范围: %s\n", strings.Join(research.CommonScope, ", ")))
		}
		if research.TypicalInterface != "" {
			sb.WriteString(fmt.Sprintf("  典型接口: %s\n", research.TypicalInterface))
		}
		if len(research.QualityBenchmarks) > 0 {
			sb.WriteString(fmt.Sprintf("  质量标准: %s\n", strings.Join(research.QualityBenchmarks, "; ")))
		}
		if len(research.ExplicitlyOutOfScope) > 0 {
			sb.WriteString(fmt.Sprintf("  明确排除: %s\n", strings.Join(research.ExplicitlyOutOfScope, ", ")))
		}
		sb.WriteString("\n")
	}

	sb.WriteString(`规则：
1. 用领域知识填补所有缺失维度
2. 明确列出OUT of scope（防止Worker越界）

输出（YAML，用---yaml包裹）：
---yaml
goal_summary: <一句话>
scope:
  included: [<项>]
  excluded: [<项>]
interface: <签名、类型、包结构>
behaviour: <核心算法、对齐目标、边界情况>
quality:
  acceptance_criteria: [<标准>]
  precision: <要求>
dependencies:
  external: <列表或"none">
constraints:
  language: <版本>
  style: <规范>
  platform: <目标>
---`)

	return sb.String()
}
