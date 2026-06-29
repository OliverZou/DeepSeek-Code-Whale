import { useStore } from '../store';
import { api } from '../wails';
import ExpertPanel from './ExpertPanel';
import SettingsPanel from './SettingsPanel';
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
    activeFunction, selMasterTaskId, masterTasks, sidebarCollapsed,
    selSubtaskId, activeRightTab, setActiveRightTab,
    rightPanelVisible,
  } = useStore();

  const mt = masterTasks.find(t => t.id === selMasterTaskId);
  const hasTasks = mt && mt.task_count > 0;

  // Build available tabs
  const tabs: { id: string; label: string }[] = [];

  if (hasTasks) {
    tabs.push({ id: 'task', label: '任务' });
  }
  if (activeFunction === 'expert') {
    tabs.push({ id: 'expert', label: '专家' });
  }
  if (activeFunction === 'settings') {
    tabs.push({ id: 'settings', label: '设置' });
  }

  // Auto-select first tab if none active
  if (tabs.length > 0 && !tabs.find(t => t.id === activeRightTab)) {
    // Use setTimeout to avoid setState during render
    setTimeout(() => setActiveRightTab(tabs[0].id), 0);
  }

  const activeTab = activeRightTab && tabs.find(t => t.id === activeRightTab) ? activeRightTab : tabs[0]?.id;

  // Panel hidden by user
  if (!rightPanelVisible) {
    return (
      <div id="right-panel" style={{ width: 0, minWidth: 0, overflow: 'hidden' }}>
        <div className="panel-titlebar" onDoubleClick={() => api.windowMaximize()} />
      </div>
    );
  }

  const renderTabContent = () => {
    switch (activeTab) {
      case 'task':
        return <TaskPanel key={mt?.id} />;
      case 'expert':
        return <ExpertPanel />;
      case 'settings':
        return <SettingsPanel visible={true} onClose={() => useStore.setState({ activeFunction: null })} embedded />;
      default:
        return null;
    }
  };

  return (
    <div id="right-panel" style={{ display: 'flex', flexDirection: 'column', overflow: 'hidden', background: 'var(--panel-bg)' }}>
      <div className="panel-titlebar" onDoubleClick={() => api.windowMaximize()} style={{ paddingRight: 110 }}>
        {sidebarCollapsed && <SidebarToggle />}
      </div>
      {tabs.length > 1 && (
        <div className="tab-bar" style={{ flexShrink: 0, padding: '0 8px' }}>
          {tabs.map(t => (
            <div
              key={t.id}
              className={`tab-btn ${activeTab === t.id ? 'active' : ''}`}
              onClick={() => setActiveRightTab(t.id)}
            >
              {t.label}
            </div>
          ))}
        </div>
      )}
      <div style={{ flex: 1, overflow: 'hidden' }}>
        {tabs.length > 0 ? renderTabContent() : (
          <div style={{ padding: 20, color: '#666', fontSize: 13, textAlign: 'center', marginTop: 40 }}>
            暂无内容，开始与 AI 对话即可
          </div>
        )}
      </div>
    </div>
  );
}
