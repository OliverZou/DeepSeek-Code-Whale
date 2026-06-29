import { api } from '../wails';
import { useStore } from '../store';

export default function TitleBar() {
  const rightPanelVisible = useStore(s => s.rightPanelVisible);
  const toggleRightPanel = useStore(s => s.toggleRightPanel);

  return (
    <div className="win-float">
      <button
        onClick={toggleRightPanel}
        title={rightPanelVisible ? '隐藏右侧面板' : '显示右侧面板'}
        style={{ opacity: rightPanelVisible ? 1 : 0.4 }}
      >
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <rect x="3" y="3" width="18" height="18" rx="2" />
          {rightPanelVisible ? (
            <line x1="15" y1="4" x2="15" y2="20" stroke="currentColor" />
          ) : (
            <line x1="15" y1="8" x2="15" y2="16" stroke="currentColor" />
          )}
        </svg>
      </button>
      <button onClick={() => api.windowMinimize()} title="最小化">
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <path d="M5 12h14"/>
        </svg>
      </button>
      <button onClick={() => api.windowMaximize()} title="最大化">
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <rect width="18" height="18" x="3" y="3" rx="2"/>
        </svg>
      </button>
      <button className="btn-close" onClick={() => api.windowClose()} title="关闭">
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <path d="M18 6 6 18"/>
          <path d="m6 6 12 12"/>
        </svg>
      </button>
    </div>
  );
}
