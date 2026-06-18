import { create } from 'zustand';
import type { MasterTask, Subtask, DialogueEntry, ChatMessage, TaskEvent, TeamInfo } from './types';
import { api } from './wails';

interface PodState {
  workDir: string;
  workDirs: string[];
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

  init: () => Promise<void>;
  loadMasterTasks: () => Promise<void>;
  loadSubtasks: (mtId: string) => Promise<void>;
  selectMasterTask: (id: string) => Promise<void>;
  selectSubtask: (id: string) => Promise<void>;
  startTask: (goal: string, team: string) => Promise<string>;
  sendFeedback: (msg: string) => Promise<void>;
  runSubtask: (taskId: string) => Promise<void>;
  cancelSubtask: (taskId: string) => Promise<void>;
  handleTaskEvent: (event: TaskEvent) => void;
}

export const useStore = create<PodState>((set, get) => ({
  workDir: '',
  workDirs: [],
  masterTasks: [],
  selWorkDir: null,
  selMasterTaskId: null,
  subtasks: [],
  selSubtaskId: null,
  dialogue: [],
  leaderPlan: [],
  chatMessages: [],
  unreadTasks: new Set(),
  activeFunction: null,
  activeTab: 'dialogue',
  teams: [],

  init: async () => {
    const dir = await api.getWorkDir() || '';
    const teams = (await api.listTeams()) || [];
    set({ workDir: dir, workDirs: dir ? [dir] : [], teams });
    await get().loadMasterTasks();
  },

  loadMasterTasks: async () => {
    const tasks = (await api.getMasterTasks()) || [];
    const dirs = [...new Set(tasks.map(t => t.workspace_path).filter(Boolean))];
    set({ masterTasks: tasks, workDirs: dirs });
  },

  loadSubtasks: async (mtId: string) => {
    const sts = await api.getSubtasks(mtId);
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
    });
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

  startTask: async (goal: string, team: string) => {
    const err = await api.startTask(goal, team);
    if (!err) {
      set({ activeFunction: null });
      setTimeout(() => get().loadMasterTasks(), 2000);
    }
    return err;
  },

  sendFeedback: async (msg: string) => {
    const taskId = get().selSubtaskId;
    if (!taskId || taskId === '__leader__') return;
    await api.sendFeedback(taskId, msg);
    const chat = await api.getChatMessages(taskId);
    set({ chatMessages: chat });
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
      if (get().selMasterTaskId) get().loadSubtasks(get().selMasterTaskId!);
    }
  },
}));
