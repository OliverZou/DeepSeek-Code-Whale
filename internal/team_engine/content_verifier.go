package team_engine

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// ContentVerifier splits large content outputs into chunks and verifies
// each in parallel using LLM sub-agents. For code tasks, the in-process
// Checker is sufficient — ContentVerifier is only used for content roles
// (researcher, writer, evaluator, synthesizer).
type ContentVerifier struct {
	runner      *AgentRunner
	maxParallel int           // max concurrent sub-agents (default 4)
	chunkSize   int           // max chars per chunk (default 100K)
	timeout     time.Duration // per sub-agent timeout (default 120s)
}

// NewContentVerifier creates a ContentVerifier using the given AgentRunner.
func NewContentVerifier(runner *AgentRunner) *ContentVerifier {
	return &ContentVerifier{
		runner:      runner,
		maxParallel: 4,
		chunkSize:   100000, // 100K chars per chunk
		timeout:     120 * time.Second,
	}
}

// chunk represents a split segment of worker output.
type chunk struct {
	index int
	text  string
}

// splitChunks splits text into chunks at paragraph boundaries, limited to
// chunkSize chars each.
func splitChunks(text string, chunkSize int) []*chunk {
	if len(text) <= chunkSize {
		return []*chunk{{index: 0, text: text}}
	}
	var chunks []*chunk
	start := 0
	idx := 0
	for start < len(text) {
		end := start + chunkSize
		if end >= len(text) {
			end = len(text)
		} else {
			// Try to find a paragraph break near the end.
			if i := strings.LastIndex(text[start:end], "\n\n"); i > chunkSize/2 {
				end = start + i + 2
			} else if i := strings.LastIndexByte(text[start:end], '\n'); i > chunkSize/2 {
				end = start + i + 1
			}
		}
		chunks = append(chunks, &chunk{index: idx, text: text[start:end]})
		idx++
		start = end
	}
	return chunks
}

// Verify runs parallel LLM verification on each chunk and synthesizes
// the results into a single verdict. It returns the verdict string
// suitable for passing to parseVerdict.
func (cv *ContentVerifier) Verify(task *Task, workerOutput string) string {
	chunks := splitChunks(workerOutput, cv.chunkSize)
	if len(chunks) == 0 {
		return "VERDICT:PASS\n"
	}

	// Limit parallelism.
	sem := make(chan struct{}, cv.maxParallel)
	var wg sync.WaitGroup
	results := make([]string, len(chunks))

	for i, c := range chunks {
		wg.Add(1)
		go func(i int, c *chunk) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			prompt := fmt.Sprintf(`Verify this content against the task requirements. Report issues only.

TASK: %s

CONTENT (part %d/%d):
%s

Check: factual accuracy, completeness, clarity, consistency.
Output: VERDICT:PASS if good, or VERDICT:FAIL with specific issues.`,
				task.Description, c.index+1, len(chunks), truncateStr(c.text, 50000))

			result := cv.runner.Run(prompt, task.Workdir, "read", cv.timeout)
			if result.Success {
				results[c.index] = result.Stdout
			} else {
				results[c.index] = fmt.Sprintf("VERDICT:FAIL sub-agent error: %s", result.Stderr)
			}
		}(i, c)
	}
	wg.Wait()

	// Synthesize: if all chunks PASS, overall PASS. Otherwise list failures.
	var fails []string
	for _, r := range results {
		if !strings.Contains(strings.ToUpper(r), "VERDICT:PASS") {
			fails = append(fails, r)
		}
	}
	if len(fails) == 0 {
		return "VERDICT:PASS\n"
	}
	return fmt.Sprintf("VERDICT:FAIL\n%d/%d chunks failed:\n%s", len(fails), len(results), strings.Join(fails, "\n---\n"))
}
