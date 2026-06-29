// Matches Go MasterTaskJSON
export interface MasterTask {
  id: string;
  goal: string;
  agent?: string;
  session_path?: string;
  workspace_id: string;
  workspace_path: string;
  workspace_label: string;
  status: string;
  created_at: string;
  task_count: number;
  done_count: number;
  active_count: number;
  suspended_count: number;
  workspace_online: boolean;
}

// Matches Go SubtaskJSON
export interface Subtask {
  id: string;
  title: string;
  description: string;
  output: string;
  role: string;
  state: string;
  progress: number;
  created_at: string;
  parent_ids: string[];
  batch_id: string;
  retry_count: number;
  max_retries: number;
  children?: Subtask[];
}

// Matches Go AgentDialogueJSON
export interface DialogueEntry {
  role: string;
  content: string;
}

// Matches Go ActionInfo
export interface ActionInfo {
  mode: string; // "agent" | "team"
  role?: string;
  goal: string;
}

// Matches Go ChatMessageJSON
export interface ChatMessage {
  time: string;
  from: string; // "human" | "agent"
  content: string;
  thinking?: string;
  durationMs?: number;
  needsAction?: boolean;
  actionType?: string;
  action?: ActionInfo;
  to?: string;
}

// Matches Go TaskEvent
export interface TaskEvent {
  type: number; // 0=state, 4=leader log, 5=agent log
  task_id: string;
  title: string;
  progress: number;
  new_state: string;
}

// Team info
export interface TeamInfo {
  name: string;
  label: string;
  category?: string;
  description: string;
  roles: string[];
  capabilities?: string[];
}

// Agent info
export interface AgentInfo {
  name: string;
  role?: string;
  description: string;
  whenToUse?: string;
  category?: string;
  tools?: string[];
  skills?: string[];
}

// Summoned item (expert or team)
export interface SummonedItem {
  type: 'expert' | 'team';
  name: string;
  label: string;
  category?: string;
  description?: string;
}

// Tool call card — extracted from AI reply markdown
export interface ToolCall {
  id: string;
  type: 'read_file' | 'write_file' | 'run_command' | 'search' | 'unknown';
  label: string;        // short label e.g. "读取 src/main.go"
  detail: string;       // file path or command
  content: string;      // code/content block
  language: string;     // code language
  filePath?: string;     // extracted file path (for write/read)
  startLine: number;    // position in original text
  endLine: number;
}

// Matches Go StreamChatChunk
export interface StreamChunk {
  sessionId: string;
  content: string;
  thinking: string;
  done: boolean;
  error?: string;
}

// Matches Go TeamChatMessage
export interface TeamChatMessage {
  from: string;
  to: string;
  content: string;
  timestamp: string;
}

// Matches Go SettingsData
export interface SettingsData {
  apiKey: string;
  model: string;
  temperature: number;
  maxTokens: number;
  theme: string;
}

// Matches Go TaskConfirmation
export interface TaskConfirmation {
  task_id: string;
  task_title: string;
  role: string;
  content: string;
  state: string;
}

// Matches Go MCPServerInfo
export interface MCPServerInfo {
  name: string;
  status: string;
  disabled: boolean;
  connected: boolean;
  tools: number;
  toolNames: string[];
  command?: string;
  url?: string;
  type?: string;
  error?: string;
}
