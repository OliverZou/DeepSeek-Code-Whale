import { useState } from 'react';
import { useStore } from '../store';

export default function FunctionList() {
  const activeFunction = useStore(s => s.activeFunction);
  const setActive = (f: string | null) => useStore.setState({ activeFunction: f });
  const [hover, setHover] = useState<Record<string, boolean>>({});

  const isActive = (f: string) => activeFunction === f;

  return (
    <div className="section" style={{ paddingBottom: 4 }}>
      {[
        { id: 'create', icon: '＋', label: '新对话' }
      ].map(({ id, icon, label }) => (
        <div
          key={id}
          onMouseEnter={() => setHover(h => ({ ...h, [id]: true }))}
          onMouseLeave={() => setHover(h => ({ ...h, [id]: false }))}
          onClick={() => setActive(isActive(id) ? null : id)}
          style={{
            fontSize: 13, padding: '8px 14px', margin: '2px 10px', cursor: 'pointer',
            borderRadius: 10, display: 'flex', alignItems: 'center',
            transition: 'background 0.15s, border-color 0.15s',
            border: isActive(id) ? '1px solid rgba(76,175,80,0.35)' : hover[id] ? '1px solid rgba(255,255,255,0.15)' : '1px solid transparent',
            background: isActive(id) ? 'rgba(76,175,80,0.12)' : hover[id] ? 'rgba(255,255,255,0.08)' : 'transparent',
          }}
        >
          <span style={{fontWeight:600, marginRight:8}}>{icon}</span>
          <span>{label}</span>
        </div>
      ))}
    </div>
  );
}
