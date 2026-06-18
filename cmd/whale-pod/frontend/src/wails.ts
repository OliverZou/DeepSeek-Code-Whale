// Wails bridge — wraps window.go.main.App.* calls
import type { MasterTask, Subtask, DialogueEntry, ChatMessage } from './types';

const go = () => (window as any).go?.main?.App;

export const api = {
  setWorkDir: (path: string): Promise<string> => go()?.SetWorkDir(path) ?? '',
  getWorkDir: (): Promise<string> => go()?.GetWorkDir() ?? '',
  listTeams: (): Promise<string[]> => go()?.ListTeams() ?? [],
  startTask: (goal: string, team: string): Promise<string> => go()?.StartTask(goal, team) ?? '',
  getMasterTasks: (): Promise<MasterTask[]> => go()?.GetMasterTasks() ?? [],
  getSubtasks: (mtId: string): Promise<Subtask[]> => go()?.GetSubtasks(mtId) ?? [],
  getAgentDialogue: (taskId: string): Promise<DialogueEntry[]> => go()?.GetAgentDialogue(taskId) ?? [],
  getLeaderPlan: (): Promise<DialogueEntry[]> => go()?.GetLeaderPlan() ?? [],
  sendFeedback: (taskId: string, msg: string): Promise<string> => go()?.SendFeedback(taskId, msg) ?? '',
  getChatMessages: (taskId: string): Promise<ChatMessage[]> => go()?.GetChatMessages(taskId) ?? [],
  runSubtask: (taskId: string): Promise<string> => go()?.RunSubtask(taskId) ?? '',
  cancelSubtask: (taskId: string): Promise<string> => go()?.CancelSubtask(taskId) ?? '',
  openTerminal: (): Promise<string> => go()?.OpenTerminal() ?? '',
  windowMinimize: () => go()?.WindowMinimize(),
  windowMaximize: () => go()?.WindowMaximize(),
  windowClose: () => go()?.WindowClose(),
  startWindowDrag: () => go()?.StartWindowDrag(),
};
