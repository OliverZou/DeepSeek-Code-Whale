import { useStore } from '../store';
import CreateTaskView from './CreateTaskView';
import TaskObserverView from './TaskObserverView';
import AgentChatView from './AgentChatView';

export default function RightPanel() {
  const { activeFunction, selMasterTaskId, selSubtaskId, masterTasks, subtasks } = useStore();

  // If "创建新任务" is active
  if (activeFunction === 'create') {
    return (
      <div id="right-panel">
        <CreateTaskView />
      </div>
    );
  }

  // If a master task is selected and it has subtasks (team plan), show observer
  const mt = masterTasks.find(t => t.id === selMasterTaskId);
  if (mt && mt.task_count > 0 && selSubtaskId) {
    return (
      <div id="right-panel">
        <TaskObserverView />
      </div>
    );
  }

  // If a subtask is selected but no plan, show chat placeholder
  if (selSubtaskId && selSubtaskId !== '__leader__') {
    return (
      <div id="right-panel">
        <AgentChatView />
      </div>
    );
  }

  // Default: empty state
  return (
    <div id="right-panel">
      <div className="empty-state">
        <div className="empty-icon">🐋</div>
        <div>选择任务或点击"创建新任务"开始</div>
      </div>
    </div>
  );
}
