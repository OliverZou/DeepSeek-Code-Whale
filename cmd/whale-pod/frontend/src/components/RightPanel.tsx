import { useStore } from '../store';
import { api } from '../wails';
import TaskPanel from './TaskPanel';

const sidebarToggleIcon = (
  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
    <rect x="3" y="3" width="18" height="18" rx="2" ry="2" />
    <line x1="9" y1="3" x2="9" y2="21" />
    <line x1="15" y1="12" x2="21" y2="12" />
  </svg>
);

function SidebarToggle() {
  const toggleSidebar = useStore(s => s.toggleSidebar);
  return (
    <span
      onClick={e => { e.stopPropagation(); toggleSidebar(); }}
      title="显示侧栏"
      style={{ cursor: 'pointer', opacity: 0.4, display: 'flex', alignItems: 'center', padding: '0 8px', flexShrink: 0 }}
    >
      {sidebarToggleIcon}
    </span>
  );
}

export default function RightPanel() {
  const {
    selMasterTaskId, masterTasks, sidebarCollapsed,
    rightPanelVisible,
  } = useStore();

  const mt = masterTasks.find(t => t.id === selMasterTaskId);
  const hasTasks = mt && mt.task_count > 0;

  // Panel hidden by user
  if (!rightPanelVisible) {
    return (
      <div id="right-panel" style={{ width: 0, minWidth: 0, overflow: 'hidden' }}>
        <div className="panel-titlebar" onDoubleClick={() => api.windowMaximize()} />
      </div>
    );
  }

  return (
    <div id="right-panel" style={{ display: 'flex', flexDirection: 'column', overflow: 'hidden', background: 'var(--panel-bg)' }}>
      <div className="panel-titlebar" onDoubleClick={() => api.windowMaximize()} style={{ paddingRight: 110 }}>
        {sidebarCollapsed && <SidebarToggle />}
      </div>
      <div style={{ flex: 1, overflow: 'hidden' }}>
        {hasTasks ? <TaskPanel key={mt?.id} /> : (
          <div style={{ padding: 20, color: '#666', fontSize: 13, textAlign: 'center', marginTop: 40 }}>
            暂无内容，开始与 AI 对话即可
          </div>
        )}
      </div>
    </div>
  );
}
