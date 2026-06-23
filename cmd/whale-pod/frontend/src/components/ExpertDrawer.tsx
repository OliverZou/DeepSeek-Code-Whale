import { useStore } from '../store';

export default function ExpertButton() {
  const isActive = useStore(s => s.activeFunction === 'expert');

  return (
    <div
      onClick={() => useStore.setState({ activeFunction: isActive ? null : 'expert' })}
      style={{
        cursor: 'pointer',
        display: 'flex', alignItems: 'center', gap: 6,
        padding: '8px 14px', margin: '2px 10px',
        border: isActive ? '1px solid rgba(76,175,80,0.35)' : '1px solid transparent',
        background: isActive ? 'rgba(76,175,80,0.12)' : 'transparent',
        borderRadius: 10,
        fontSize: 13, fontWeight: 600, color: isActive ? '#fff' : '#aaa',
        transition: 'background 0.15s, border-color 0.15s',
      }}
    >
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" style={{ flexShrink: 0 }}>
        <rect x="3" y="7" width="18" height="13" rx="4"/>
        <circle cx="8" cy="13" r="1.5"/>
        <circle cx="16" cy="13" r="1.5"/>
        <path d="M8 4v4"/>
        <path d="M16 4v4"/>
        <circle cx="12" cy="4" r="1.5" fill="currentColor"/>
      </svg>
      <span>专家</span>
    </div>
  );
}
