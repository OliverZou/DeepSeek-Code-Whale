import { useEffect } from 'react';
import { useStore } from './store';
import TitleBar from './components/TitleBar';
import Sidebar from './components/Sidebar';
import RightPanel from './components/RightPanel';
import Resizer from './components/Resizer';

export default function App() {
  const init = useStore(s => s.init);
  const sidebarCollapsed = useStore(s => s.sidebarCollapsed);

  useEffect(() => { useStore.getState().init(); }, []);

  return (
    <div className="app-frame">
      <TitleBar />
      <div className="app-container">
        <Sidebar />
        {!sidebarCollapsed && <Resizer target="sidebar" side="right" />}
        <RightPanel />
      </div>
    </div>
  );
}
