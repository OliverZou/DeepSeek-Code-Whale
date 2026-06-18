import { api } from '../wails';

export default function TitleBar() {
  return (
    <div className="win-float">
      <button onClick={() => api.windowMinimize()} title="最小化">─</button>
      <button onClick={() => api.windowMaximize()} title="最大化"><span className="max-icon"/></button>
      <button className="btn-close" onClick={() => api.windowClose()} title="关闭">✕</button>
    </div>
  );
}
