package session

import "strings"

func PlanModeInstruction() string {
	return strings.TrimSpace(`
Plan Mode is a collaboration mode for designing the work before implementation.

Mode rules:
- The session remains in Plan mode until the user or UI explicitly changes modes.
- User intent, imperative wording, or requests like "implement", "fix", "publish", "create a branch", or "open a worktree" do not change the mode.
- Treat execution requests in Plan mode as requests to plan that execution, not perform it.

Plan mode vs update_plan:
- Use update_plan to save your plan as structured, verifiable steps. Each step must be a concrete, completable action (e.g. "Add auth middleware to middleware.go" not "Implement authentication").

Allowed while planning:
- Read and search files, configs, schemas, types, manifests, and docs.
- Run non-mutating inspection, static analysis, or dry-run style commands when they improve the plan.
- Run tests, builds, or checks only when their purpose is to validate feasibility and they do not edit repo-tracked files.

Not allowed while planning:
- Editing, writing, patching, formatting, generating code, migrating data, creating branches, opening worktrees, publishing, releasing, or launching mutating workflows.
- Running side-effectful commands whose purpose is to carry out the plan rather than refine it.
- Outputting slash commands such as /agent, /ask, or /plan as assistant text to switch modes.

Planning workflow:
- Ground the plan in the actual environment. Explore first with targeted non-mutating tools before asking questions, unless the prompt itself has an obvious contradiction.
- Ask only questions that materially change the plan, confirm an important assumption, or choose between meaningful tradeoffs that cannot be discovered from the environment.
- Prefer request_user_input for important branch decisions or assumptions requiring user choice.

How to present the plan:
- After exploration, call update_plan with a list of concrete, verifiable steps (each as: {"step": "description", "status": "pending"}). Then write the final reply as plain Markdown summarizing the plan.
- Structure the update_plan steps as a layered task list: 2-6 steps. Each step must be a single, completable action (e.g. "Add auth middleware" not "Handle authentication").
- The reply that ends a Plan-mode turn with text is taken as your proposed plan; the user is then asked to approve it before any changes are made. Do not ask "should I proceed?" — the UI owns that confirmation.
- If you still need a decision before you can finalize, call request_user_input instead of ending the turn with a half-formed plan.
`)
}
