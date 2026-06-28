import { create } from 'zustand';
import type { MasterTask, Subtask, DialogueEntry, ChatMessage, TaskEvent, TeamInfo, AgentInfo, SummonedItem, TeamChatMessage, TaskConfirmation, StreamChunk } from './types';
import { api } from './wails';

const PINNED_KEY = 'whale_pinned_task_ids';
const OLD_PINNED_KEY = 'whale-pod-pinned-tasks';
const TABS_KEY = 'whale_open_tabs';
const WORKSPACES_KEY = 'whale-pod-open-workspaces';

function loadPinnedIds(): string[] {
  try {
    const raw = localStorage.getItem(PINNED_KEY);
    if (raw) return JSON.parse(raw);
    // 迁移旧 key 数据
    const old = localStorage.getItem(OLD_PINNED_KEY);
    if (old) {
      const ids = JSON.parse(old);
      localStorage.setItem(PINNED_KEY, old);
      localStorage.removeItem(OLD_PINNED_KEY);
      return ids;
    }
    return [];
  } catch { return []; }
}

let _loadMasterTasksPromise: Promise<void> | null = null;

export function persistPinnedIds(ids: string[]) {
  try { localStorage.setItem(PINNED_KEY, JSON.stringify(ids)); } catch { /* ignore */ }
}

function loadTabs(): Record<string, string[]> {
  try {
    const raw = localStorage.getItem(TABS_KEY);
    return raw ? JSON.parse(raw) : {};
  } catch { return {}; }
}

function persistTabs(tabs: Record<string, string[]>) {
  try { localStorage.setItem(TABS_KEY, JSON.stringify(tabs)); } catch { /* ignore */ }
}

function loadWorkspaces(): string[] {
  try {
    const raw = localStorage.getItem(WORKSPACES_KEY);
    return raw ? JSON.parse(raw) : [];
  } catch { return []; }
}

function persistWorkspaces(dirs: string[]) {
  try { localStorage.setItem(WORKSPACES_KEY, JSON.stringify(dirs)); } catch { /* ignore */ }
}

interface PodState {
  workDir: string;
  openWorkspaces: string[];
  masterTasks: MasterTask[];
  selWorkDir: string | null;
  selMasterTaskId: string | null;
  subtasks: Subtask[];
  selSubtaskId: string | null;
  dialogue: DialogueEntry[];
  leaderPlan: DialogueEntry[];
  chatMessages: ChatMessage[];
  unreadTasks: Set<string>;
  activeFunction: string | null;
  activeTab: 'dialogue' | 'observer';
  teams: string[];
  teamDetails: TeamInfo[];
  agentDetails: AgentInfo[];
  summonedItems: SummonedItem[];
  preselectedExpert: string;
  pinnedTaskIds: string[];
  directChatTaskId: string | null;
  directMessages: ChatMessage[];
  teamChatMessages: TeamChatMessage[];
  confirmations: TaskConfirmation[];
  targetRole: string;
  sidebarCollapsed: boolean;
  selAgentId: string | null;
  openTabs: Record<string, string[]>;

  // Streaming state
  isStreaming: boolean;
  streamingContent: string;
  streamingThinking: string;
  chatVersion: number;
  abortStreaming: () => void;

  init: () => Promise<void>;
  loadMasterTasks: () => Promise<void>;
  loadSubtasks: (mtId: string) => Promise<void>;
  selectMasterTask: (id: string) => Promise<void>;
  selectSubtask: (id: string) => Promise<void>;
  startTask: (goal: string, team: string, workDir?: string) => Promise<string>;
  startDirectChat: (goal: string, workDir?: string, agent?: string, deepThink?: boolean) => Promise<void>;
  sendDirectChat: (message: string, deepThink?: boolean) => Promise<void>;
  sendFeedback: (msg: string) => Promise<void>;
  sendTeamChat: (message: string, targetRole?: string) => Promise<void>;
  loadTeamChat: () => Promise<void>;
  confirmTask: (taskId: string, approved: boolean, feedback?: string) => Promise<void>;
  loadConfirmations: () => Promise<void>;
  summonItem: (item: SummonedItem) => void;
  dismissItem: (name: string, type: string) => void;
  selectAgent: (agentId: string, agentName: string, agentType: 'expert' | 'team' | 'whale') => void;
  addTab: (sessionId: string) => void;
  removeTab: (sessionId: string) => void;
  bringTabToFront: (tabId: string) => void;
  getAgentKey: (agent: string) => string;
  getCurrentTabs: () => string[];
  summonAndOpen: (item: SummonedItem) => Promise<void>;
  toggleSidebar: () => void;
  runSubtask: (taskId: string) => Promise<void>;
  cancelSubtask: (taskId: string) => Promise<void>;
  handleTaskEvent: (event: TaskEvent) => void;
  handleStreamChunk: (chunk: StreamChunk) => void;
  handleChatAction: (data: { sessionId: string; mode: string; role?: string; goal: string }) => Promise<void>;
  regenerateLast: (deepThink?: boolean) => Promise<void>;
  deleteMessage: (index: number) => Promise<void>;
  openWorkspace: (dir: string) => Promise<void>;
  removeWorkspace: (dir: string) => Promise<void>;
}

export const useStore = create<PodState>((set, get) => ({
  workDir: '',
  openWorkspaces: loadWorkspaces(),
  masterTasks: [],
  selWorkDir: null,
  selMasterTaskId: null,
  subtasks: [],
  selSubtaskId: null,
  dialogue: [],
  leaderPlan: [],
  chatMessages: [],
  unreadTasks: new Set(),
  activeFunction: 'create',
  activeTab: 'dialogue',
  teams: [],
  teamDetails: [],
  agentDetails: [] as AgentInfo[],
  summonedItems: [] as SummonedItem[],
  preselectedExpert: '',
  sidebarCollapsed: false,
  selAgentId: null,
  openTabs: loadTabs(),
  pinnedTaskIds: loadPinnedIds(),
  directChatTaskId: null,
  directMessages: [],
  teamChatMessages: [],
  confirmations: [],
  targetRole: '',

  // Streaming state defaults
  isStreaming: false,
  streamingContent: '',
  streamingThinking: '',
  chatVersion: 0,

  init: async () => {
    // Guard against double-init (StrictMode / HMR / multiple calls)
    if ((get() as any)._initDone) return;
    (get() as any)._initDone = true;

    const dir = await api.getWorkDir() || '';
    const teams = (await api.listTeams()) || [];
    const teamDetails = (await api.listTeamDetails()) || [];
    const agentDetails = (await api.listAgents()) || [];
    const summoned = (await api.loadSummonedItems()) || [];
    const openWorkspaces = loadWorkspaces();
    set({ workDir: dir, openWorkspaces, teams, teamDetails, agentDetails, summonedItems: summoned });
    await get().loadMasterTasks();

    // Listen for streaming chat chunks — only once
    const wails = (window as any).runtime;
    if (wails?.EventsOn) {
      wails.EventsOn('chat-chunk', (chunk: StreamChunk) => {
        get().handleStreamChunk(chunk);
      });
      wails.EventsOn('chat-action', (data: { sessionId: string; mode: string; role?: string; goal: string }) => {
        get().handleChatAction(data);
      });
      wails.EventsOn('session-update', (sessionID: string) => {
        if (sessionID === get().directChatTaskId) {
          api.getChatMessages(sessionID).then(msgs => {
            if (msgs && msgs.length > 0) {
              const displayMsgs: ChatMessage[] = msgs.map((m: ChatMessage) => ({
                from: m.from === 'agent' ? 'agent' : 'human',
                content: m.content,
                time: m.time || '',
                thinking: m.thinking || undefined,
                durationMs: m.durationMs != null ? m.durationMs : undefined,
              }));
              set({ directMessages: displayMsgs, chatVersion: get().chatVersion + 1 });
            }
          });
          get().loadMasterTasks();
        }
      });
    }
  },

  loadMasterTasks: async () => {
    if (_loadMasterTasksPromise) return _loadMasterTasksPromise;
    _loadMasterTasksPromise = (async () => {
      try {
        let tasks = await api.getMasterTasks();
        if (!tasks || tasks.length === 0) {
          await new Promise(r => setTimeout(r, 500));
          // Re-check: another loadMasterTasks call may have set tasks in between.
          tasks = await api.getMasterTasks();
        }
        set({ masterTasks: tasks || [] });
      } catch (err) {
        console.error('loadMasterTasks failed:', err);
      } finally {
        _loadMasterTasksPromise = null;
      }
    })();
    return _loadMasterTasksPromise;
  },

  loadSubtasks: async (sessionId: string) => {
    const sts = await api.getSubtasksBySession(sessionId);
    set({ subtasks: sts });
  },

  selectMasterTask: async (id: string) => {
    const task = get().masterTasks.find(t => t.id === id);
    set({
      selMasterTaskId: id,
      selSubtaskId: null,
      dialogue: [],
      leaderPlan: [],
      activeFunction: null,
      directChatTaskId: task && task.task_count === 0 ? id : get().directChatTaskId,
    });
    // Direct chat task: load messages from history
    if (task && task.task_count === 0) {
      const msgs = await api.getChatMessages(id);
      if (msgs && msgs.length > 0) {
        const displayMsgs: ChatMessage[] = msgs.map(m => ({
          from: m.from === 'agent' ? 'agent' : 'human',
          content: m.content,
          time: m.time || '',
          thinking: m.thinking || undefined,
          durationMs: m.durationMs != null ? m.durationMs : undefined,
        }));
        set({ directMessages: displayMsgs, chatVersion: get().chatVersion + 1 });
      }
      return;
    }
    await get().loadSubtasks(id);
    if (task?.task_count && task.task_count > 0) {
      const plan = await api.getLeaderPlan();
      set({ leaderPlan: plan });
    }
  },

  selectSubtask: async (id: string) => {
    const unread = new Set(get().unreadTasks);
    unread.delete(id);
    set({ selSubtaskId: id, unreadTasks: unread, activeFunction: null });
    if (id === '__leader__') {
      const plan = await api.getLeaderPlan();
      set({ leaderPlan: plan, dialogue: [] });
    } else {
      const dlg = await api.getAgentDialogue(id);
      const chat = await api.getChatMessages(id);
      set({ dialogue: dlg, chatMessages: chat, leaderPlan: [] });
    }
  },

  startTask: async (goal: string, team: string, workDir?: string) => {
    const err = await api.startTask(goal, team, workDir);
    if (!err) {
      set({ activeFunction: null });
      setTimeout(() => get().loadMasterTasks(), 2000);
    }
    return err;
  },

  startDirectChat: async (goal: string, workDir?: string, agent?: string, deepThink?: boolean) => {
    const taskId = await api.createDirectTask(goal, workDir, agent, deepThink);
    if (!taskId) return;
    set({ directChatTaskId: taskId, selMasterTaskId: taskId, activeFunction: 'chat', directMessages: [{ from: 'human', content: goal, time: '' }] });
    const { openTabs, selAgentId } = get();
    const key = selAgentId || '';
    const agentTabs = openTabs[key] || [];
    if (!agentTabs.includes(taskId)) {
      const next = { ...openTabs, [key]: [...agentTabs, taskId] };
      persistTabs(next);
      set({ openTabs: next });
    }
    setTimeout(() => get().loadMasterTasks(), 1000);
    // Start streaming for the initial AI reply
    (get() as any)._streamStart = Date.now();
    set({ isStreaming: true, streamingContent: '', streamingThinking: '' });
    await api.streamChat(taskId, goal, deepThink);
  },

  sendDirectChat: async (message: string, deepThink?: boolean) => {
    const taskId = get().directChatTaskId;
    if (!taskId) return;
    // Add user message immediately
    set(s => ({ directMessages: [...s.directMessages, { from: 'human', content: message, time: '' }], chatVersion: s.chatVersion + 1 }));
    // Start streaming for the initial AI reply
    (get() as any)._streamStart = Date.now();
    set({ isStreaming: true, streamingContent: '', streamingThinking: '' });
    await api.streamChat(taskId, message, deepThink);
  },

  abortStreaming: () => {
    const taskId = get().directChatTaskId;
    if (!taskId) return;
    api.abortChat(taskId);
  },

  handleStreamChunk: (chunk: StreamChunk) => {
    const { directChatTaskId, isStreaming, directMessages } = get();
    if (chunk.sessionId !== directChatTaskId) return;

    // Guard: ignore chunks when not streaming (stream already finalized)
    if (!get().isStreaming) return;

    if (chunk.error === 'cancelled') {
      set({ isStreaming: false });
      get().loadMasterTasks();
      return;
    }

    if (chunk.done) {
      // Use setTimeout to ensure React processes this state update
      setTimeout(() => {
      const { streamingContent, streamingThinking } = get();
      if (streamingContent || streamingThinking) {
        const start = (get() as any)._streamStart || Date.now();
        const durationMs = Date.now() - start;
        const displayContent = streamingContent || (streamingThinking ? '[仅含思考内容]' : '');
        const agentMsg: ChatMessage = {
          from: 'agent',
          content: displayContent,
          time: '',
          thinking: streamingThinking || undefined,
          durationMs,
        };
        set(s => ({
          directMessages: [...s.directMessages, agentMsg],
          isStreaming: false,
          streamingContent: '',
          streamingThinking: '',
          chatVersion: s.chatVersion + 1,
        }));
      } else {
        set({ isStreaming: false });
      }
      // Reload session list to update preview
      setTimeout(() => get().loadMasterTasks(), 300);
      }, 0);
      return;
    }

    // Accumulate streaming content
    if (chunk.content || chunk.thinking) {
      set(s => ({
        streamingContent: s.streamingContent + chunk.content,
        streamingThinking: s.streamingThinking + chunk.thinking,
      }));
    }
  },

  handleChatAction: async (data: { sessionId: string; mode: string; role?: string; goal: string }) => {
    const { sessionId, mode, role, goal } = data;
    const selAgentId = get().selAgentId || '';
    const workDir = '';

    if (mode === 'team') {
      const teamName = selAgentId.startsWith('team:') ? selAgentId.slice(5) : '';
      if (!teamName) { console.warn('[handleChatAction] team mode but no teamName from selAgentId:', selAgentId); return; }
      await api.startTaskInSession(sessionId, goal, teamName, workDir);
    } else if (mode === 'agent') {
      const agentName = role || (selAgentId.startsWith('expert:') ? selAgentId.slice(7) : '');
      if (!agentName) { console.warn('[handleChatAction] agent mode but no agentName, role:', role, 'selAgentId:', selAgentId); return; }
      await api.startExpertTaskInSession(sessionId, goal, agentName, workDir);
    }

    setTimeout(() => get().loadMasterTasks(), 500);
  },

  sendFeedback: async (msg: string) => {
    const taskId = get().selSubtaskId;
    if (!taskId || taskId === '__leader__') return;
    await api.sendFeedback(taskId, msg);
    const chat = await api.getChatMessages(taskId);
    set({ chatMessages: chat });
  },

  sendTeamChat: async (message: string, targetRole?: string) => {
    const masterTaskId = get().selMasterTaskId;
    if (!masterTaskId) return;
    const role = targetRole || get().targetRole || '';
    await api.sendTeamChat(masterTaskId, message, role);
    await get().loadTeamChat();
  },

  loadTeamChat: async () => {
    const masterTaskId = get().selMasterTaskId;
    if (!masterTaskId) return;
    const messages = await api.getTeamChat(masterTaskId);
    set({ teamChatMessages: messages || [] });
  },

  confirmTask: async (taskId: string, approved: boolean, feedback?: string) => {
    await api.confirmTask(taskId, approved, feedback || '');
    await get().loadConfirmations();
    if (get().selMasterTaskId) get().loadSubtasks(get().selMasterTaskId!);
  },

  loadConfirmations: async () => {
    const masterTaskId = get().selMasterTaskId;
    if (!masterTaskId) return;
    const confs = await api.getConfirmationsForMaster(masterTaskId);
    set({ confirmations: confs || [] });
  },

  runSubtask: async (taskId: string) => {
    await api.runSubtask(taskId);
    setTimeout(() => {
      if (get().selMasterTaskId) get().loadSubtasks(get().selMasterTaskId!);
    }, 1000);
  },

  cancelSubtask: async (taskId: string) => {
    await api.cancelSubtask(taskId);
    setTimeout(() => {
      if (get().selMasterTaskId) get().loadSubtasks(get().selMasterTaskId!);
    }, 1000);
  },

  summonItem: (item: SummonedItem) => {
    const current = get().summonedItems;
    if (current.find(s => s.name === item.name && s.type === item.type)) return;
    const next = [...current, item];
    set({ summonedItems: next });
    api.saveSummonedItems(next);
  },

  dismissItem: (name: string, type: string) => {
    const next = get().summonedItems.filter(s => !(s.name === name && s.type === type));
    set({ summonedItems: next });
    api.saveSummonedItems(next);
  },

  selectAgent: async (agentId: string, agentName: string, agentType: 'expert' | 'team' | 'whale') => {
    const agentKey = agentType === 'whale' ? '' : agentType + ':' + agentName;
    set({ selAgentId: agentKey, activeFunction: null, directMessages: [] });
    const tasks = get().masterTasks.filter(t => (t.agent || '') === agentKey);
    const allTabs = { ...get().openTabs };
    const agentTabs = allTabs[agentKey] || [];
    if (tasks.length > 0) {
      const firstId = tasks[0].id;
      if (!agentTabs.includes(firstId)) {
        allTabs[agentKey] = [...agentTabs, firstId];
        persistTabs(allTabs);
        set({ openTabs: allTabs });
      }
      await get().selectMasterTask(firstId);
    } else {
      set({ selMasterTaskId: null, activeFunction: 'chat', directChatTaskId: null });
    }
  },

  getAgentKey: (agent: string) => agent,

  getCurrentTabs: () => {
    const { openTabs, selAgentId } = get();
    return openTabs[selAgentId || ''] || [];
  },

  addTab: (sessionId: string) => {
    const { openTabs, selAgentId } = get();
    const key = selAgentId || '';
    const agentTabs = openTabs[key] || [];
    if (!agentTabs.includes(sessionId)) {
      const next = { ...openTabs, [key]: [...agentTabs, sessionId] };
      persistTabs(next);
      set({ openTabs: next });
    }
  },

  removeTab: (sessionId: string) => {
    const { openTabs, selAgentId, selMasterTaskId } = get();
    const key = selAgentId || '';
    const agentTabs = (openTabs[key] || []).filter(id => id !== sessionId);
    const next = { ...openTabs, [key]: agentTabs };
    persistTabs(next);
    set({ openTabs: next });
    if (selMasterTaskId === sessionId) {
      if (agentTabs.length > 0) {
        get().selectMasterTask(agentTabs[agentTabs.length - 1]);
      } else {
        set({ selMasterTaskId: null, activeFunction: 'chat', directChatTaskId: null });
      }
    }
  },

  bringTabToFront: (tabId: string) => {
    const { openTabs, selAgentId } = get();
    const key = selAgentId || '';
    const agentTabs = openTabs[key] || [];
    const idx = agentTabs.indexOf(tabId);
    if (idx <= 0) return; // already at front or not found
    const reordered = [tabId, ...agentTabs.filter(id => id !== tabId)];
    const next = { ...openTabs, [key]: reordered };
    persistTabs(next);
    set({ openTabs: next });
  },

  summonAndOpen: async (item: SummonedItem) => {
    // 召唤（不重复）
    const current = get().summonedItems;
    if (!current.find(s => s.name === item.name && s.type === item.type)) {
      const next = [...current, item];
      set({ summonedItems: next });
      api.saveSummonedItems(next);
    }
    // 设置预选专家 → 打开新对话
    set({ preselectedExpert: item.name, activeFunction: 'create' });
  },

  toggleSidebar: () => set(s => ({ sidebarCollapsed: !s.sidebarCollapsed })),

  handleTaskEvent: (event: TaskEvent) => {
    const unread = new Set(get().unreadTasks);
    if (event.type === 4) { // leader log
      unread.add('__leader__');
      set({ unreadTasks: unread });
      if (get().selSubtaskId === '__leader__') {
        api.getLeaderPlan().then(p => set({ leaderPlan: p }));
      }
    } else if (event.type === 5) { // agent log
      if (event.task_id) {
        unread.add(event.task_id);
        set({ unreadTasks: unread });
        if (get().selSubtaskId === event.task_id) {
          api.getAgentDialogue(event.task_id).then(d => set({ dialogue: d }));
        }
      }
    } else if (event.type === 0) { // state changed
      get().loadMasterTasks();
      if (get().selMasterTaskId) {
        get().loadSubtasks(get().selMasterTaskId!);
        get().loadConfirmations();
      }
    }
  },

  openWorkspace: async (dir: string) => {
    const current = get().openWorkspaces;
    if (!current.includes(dir)) {
      const updated = [...current, dir];
      persistWorkspaces(updated);
      set({ openWorkspaces: updated });
    }
    await get().loadMasterTasks();
  },

  removeWorkspace: async (dir: string) => {
    const updated = get().openWorkspaces.filter(d => d !== dir);
    persistWorkspaces(updated);
    set({ openWorkspaces: updated });
    await get().loadMasterTasks();
  },

  regenerateLast: async (deepThink?: boolean) => {
    const { directMessages, directChatTaskId, isStreaming } = get();
    if (isStreaming || !directChatTaskId || directMessages.length < 2) return;
    // Remove last AI message
    const msgs = directMessages.slice(0, -1);
    // Get the last user message
    const lastUserMsg = [...msgs].reverse().find(m => m.from === 'human');
    if (!lastUserMsg) return;
    set({ directMessages: msgs });
    // Re-send the last user message
    await get().sendDirectChat(lastUserMsg.content, deepThink);
  },

  deleteMessage: async (index: number) => {
    const { directMessages, isStreaming } = get();
    if (isStreaming || index < 0 || index >= directMessages.length) return;
    const msgs = [...directMessages];
    msgs.splice(index, 1);
    set({ directMessages: msgs });
  },
}));
