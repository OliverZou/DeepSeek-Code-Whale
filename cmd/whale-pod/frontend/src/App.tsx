import { useEffect, useState, useCallback } from 'react';
import { useStore } from './store';
import TitleBar from './components/TitleBar';
import Sidebar from './components/Sidebar';
import ChatArea from './components/ChatArea';
import RightPanel from './components/RightPanel';
import Resizer from './components/Resizer';
import SettingsPanel from './components/SettingsPanel';
import ErrorBoundary from './components/ErrorBoundary';
import { useKeyboard } from './components/useKeyboard';

export default function App() {
  const init = useStore(s => s.init);
  const sidebarCollapsed = useStore(s => s.sidebarCollapsed);
  const [showSettings, setShowSettings] = useState(false);

  useEffect(() => { useStore.getState().init(); }, []);

  // Listen for toggle-settings event from Sidebar
  useEffect(() => {
    const handler = () => setShowSettings(s => !s);
    window.addEventListener('toggle-settings', handler);
    return () => window.removeEventListener('toggle-settings', handler);
  }, []);

  const handleNewChat = useCallback(() => {
    useStore.setState({
      selMasterTaskId: null,
      directChatTaskId: null,
      directMessages: [],
      activeFunction: 'chat',
    });
  }, []);

  const handleCloseTab = useCallback(() => {
    const { openTabs, selAgentId, selMasterTaskId, removeTab } = useStore.getState();
    const key = selAgentId || '';
    const tabs = openTabs[key] || [];
    if (selMasterTaskId && tabs.includes(selMasterTaskId)) {
      removeTab(selMasterTaskId);
    }
  }, []);

  const handleNextTab = useCallback(() => {
    const { openTabs, selAgentId, selMasterTaskId } = useStore.getState();
    const key = selAgentId || '';
    const tabs = openTabs[key] || [];
    if (tabs.length < 2) return;
    const idx = selMasterTaskId ? tabs.indexOf(selMasterTaskId) : -1;
    const next = idx >= 0 ? tabs[(idx + 1) % tabs.length] : tabs[0];
    useStore.getState().selectMasterTask(next);
  }, []);

  const handlePrevTab = useCallback(() => {
    const { openTabs, selAgentId, selMasterTaskId } = useStore.getState();
    const key = selAgentId || '';
    const tabs = openTabs[key] || [];
    if (tabs.length < 2) return;
    const idx = selMasterTaskId ? tabs.indexOf(selMasterTaskId) : 0;
    const prev = idx > 0 ? tabs[idx - 1] : tabs[tabs.length - 1];
    useStore.getState().selectMasterTask(prev);
  }, []);

  useKeyboard({
    onNewChat: handleNewChat,
    onToggleSettings: () => setShowSettings(s => !s),
    onCloseTab: handleCloseTab,
    onNextTab: handleNextTab,
    onPrevTab: handlePrevTab,
  });

  return (
    <ErrorBoundary>
      <div className="app-frame">
        <TitleBar />
        <div className="app-container">
          <Sidebar />
          {!sidebarCollapsed && <Resizer target="sidebar" side="right" />}
          <ChatArea />
          <Resizer target="right-panel" side="left" />
          <RightPanel />
        </div>

        <SettingsPanel visible={showSettings} onClose={() => setShowSettings(false)} />
      </div>
    </ErrorBoundary>
  );
}
