import { create } from 'zustand';
import type { MasterTask, Subtask, DialogueEntry, ChatMessage, TaskEvent, TeamInfo, AgentInfo, SummonedItem, TeamChatMessage, TaskConfirmation } from './types';
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
  getAgentKey: (agent: string) => string;
  getCurrentTabs: () => string[];
  summonAndOpen: (item: SummonedItem) => Promise<void>;
  toggleSidebar: () => void;
  runSubtask: (taskId: string) => Promise<void>;
  cancelSubtask: (taskId: string) => Promise<void>;
  handleTaskEvent: (event: TaskEvent) => void;
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

  init: async () => {
    const dir = await api.getWorkDir() || '';
    const teams = (await api.listTeams()) || [];
    const teamDetails = (await api.listTeamDetails()) || [];
    const agentDetails = (await api.listAgents()) || [];
    const summoned = (await api.loadSummonedItems()) || [];
    const openWorkspaces = loadWorkspaces();
    set({ workDir: dir, openWorkspaces, teams, teamDetails, agentDetails, summonedItems: summoned });
    await get().loadMasterTasks();
  },

  loadMasterTasks: async () => {
    if (_loadMasterTasksPromise) return _loadMasterTasksPromise;
    _loadMasterTasksPromise = (async () => {
      try {
        let tasks = await api.getMasterTasks();
        if (!tasks || tasks.length === 0) {
          await new Promise(r => setTimeout(r, 500));
          tasks = await api.getMasterTasks();
        }
        set({ masterTasks: tasks || [] });
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
    // Direct chat task: skip subtask loading
    if (task && task.task_count === 0) {
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
    // Get initial AI reply
    const raw = await api.directChat(taskId, goal, deepThink);
    if (raw) {
      try {
        const result = JSON.parse(raw);
        set(s => ({ directMessages: [...s.directMessages, { from: 'agent', content: result.reply || raw, time: '', thinking: result.thinking, durationMs: result.durationMs, needsAction: result.needsAction, actionType: result.actionType }] }));
      } catch {
        set(s => ({ directMessages: [...s.directMessages, { from: 'agent', content: raw, time: '' }] }));
      }
    }
  },

  sendDirectChat: async (message: string, deepThink?: boolean) => {
    const taskId = get().directChatTaskId;
    if (!taskId) return;
    set(s => ({ directMessages: [...s.directMessages, { from: 'human', content: message, time: '' }] }));
    const raw = await api.directChat(taskId, message, deepThink);
    if (raw) {
      try {
        const result = JSON.parse(raw);
        set(s => ({ directMessages: [...s.directMessages, { from: 'agent', content: result.reply || raw, time: '', thinking: result.thinking, durationMs: result.durationMs, needsAction: result.needsAction, actionType: result.actionType }] }));
      } catch {
        set(s => ({ directMessages: [...s.directMessages, { from: 'agent', content: raw, time: '' }] }));
      }
    }
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
}));
