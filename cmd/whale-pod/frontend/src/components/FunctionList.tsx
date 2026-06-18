import { useStore } from '../store';

export default function FunctionList() {
  const activeFunction = useStore(s => s.activeFunction);
  const setActive = (f: string | null) => useStore.setState({ activeFunction: f });

  return (
    <div className="section" style={{ paddingBottom: 4 }}>
      <div
        className={`section-item ${activeFunction === 'create' ? 'active' : ''}`}
        onClick={() => setActive(activeFunction === 'create' ? null : 'create')}
        style={{ fontSize: 15, padding: '10px 14px' }}
      >
        📝 创建新任务
      </div>
    </div>
  );
}
