import FunctionList from './FunctionList';
import ExpertButton from './ExpertDrawer';
import TaskTree from './TaskTree';
import { api } from '../wails';
import { useStore } from '../store';

export default function Sidebar() {
  const toggleSidebar = useStore(s => s.toggleSidebar);
  const collapsed = useStore(s => s.sidebarCollapsed);
  return (
    <div
      id="sidebar"
      style={{
        width: collapsed ? 0 : undefined,
        minWidth: collapsed ? 0 : undefined,
        borderRightWidth: collapsed ? 0 : undefined,
        overflow: 'hidden',
      }}
    >
      <div className="panel-titlebar" onDoubleClick={() => api.windowMaximize()}>
        <img src="/whale.png" width="36" height="36" alt="Whale" />
        <span className="titlebar-label">Whale Pod</span>
        <span
          onClick={e => { e.stopPropagation(); toggleSidebar(); }}
          title="隐藏侧栏"
          style={{ marginLeft: 'auto', cursor: 'pointer', opacity: 0.4, display: 'flex', alignItems: 'center', padding: 4 }}
        >
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <rect x="3" y="3" width="18" height="18" rx="2" ry="2" />
            <line x1="9" y1="3" x2="9" y2="21" />
            <line x1="15" y1="12" x2="9" y2="12" />
          </svg>
        </span>
      </div>
      <div style={{ flex: 1, overflowY: 'auto', overflowX: 'hidden' }}>
        <FunctionList />
        <ExpertButton />
        <TaskTree />
      </div>
    </div>
  );
}
