import { api } from '../wails';

export default function TitleBar() {
  return (
    <div className="win-float">
      <button onClick={() => api.windowMinimize()} title="最小化">
        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <path d="M5 12h14"/>
        </svg>
      </button>
      <button onClick={() => api.windowMaximize()} title="最大化">
        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <rect width="18" height="18" x="3" y="3" rx="2"/>
        </svg>
      </button>
      <button className="btn-close" onClick={() => api.windowClose()} title="关闭">
        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <path d="M18 6 6 18"/>
          <path d="m6 6 12 12"/>
        </svg>
      </button>
    </div>
  );
}
