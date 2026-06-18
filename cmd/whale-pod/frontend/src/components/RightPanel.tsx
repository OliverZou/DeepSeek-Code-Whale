import { useStore } from '../store';
import CreateTaskView from './CreateTaskView';
import TaskObserverView from './TaskObserverView';
import AgentChatView from './AgentChatView';

export default function RightPanel() {
  const { activeFunction, selMasterTaskId, selSubtaskId, masterTasks } =
    useStore();

  const mt = masterTasks.find(t => t.id === selMasterTaskId);

  // If "创建新任务" is active
  if (activeFunction === 'create') {
    return (
      <div id="right-panel">
        <div className="panel-titlebar" />
        <CreateTaskView />
      </div>
    );
  }

  // If a subtask is selected: show title with task info
  if (mt && mt.task_count > 0 && selSubtaskId) {
    return (
      <div id="right-panel">
        <div className="panel-titlebar" />
        <TaskObserverView />
      </div>
    );
  }

  // Chat view
  if (selSubtaskId && selSubtaskId !== '__leader__') {
    return (
      <div id="right-panel">
        <div className="panel-titlebar" />
        <AgentChatView />
      </div>
    );
  }

  // Default: empty state
  return (
    <div id="right-panel">
      <div className="panel-titlebar" />
      <div className="empty-state">
        <div className="empty-icon">🐋</div>
        <div>选择任务或点击"创建新任务"开始</div>
      </div>
    </div>
  );
}
