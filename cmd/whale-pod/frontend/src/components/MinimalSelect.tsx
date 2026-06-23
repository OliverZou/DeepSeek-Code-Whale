import { useState, useRef, useEffect, useLayoutEffect } from 'react';

interface Props {
  value: string;
  options: { value: string; label: string; special?: boolean }[];
  onChange: (value: string) => void;
  style?: React.CSSProperties;
  placeholder?: string;
}

export default function MinimalSelect({ value, options, onChange, style, placeholder }: Props) {
  const [open, setOpen] = useState(false);
  const [ready, setReady] = useState(false);
  const [dropdownStyle, setDropdownStyle] = useState<React.CSSProperties>({});
  const ref = useRef<HTMLDivElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);

  // 打开 / 关闭
  const toggle = () => {
    if (open) { setOpen(false); setReady(false); return; }
    setOpen(true);
    setReady(false);
  };

  // 关闭：点击外部
  useEffect(() => {
    if (!open) return;
    const handler = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', handler);
    return () => document.removeEventListener('mousedown', handler);
  }, [open]);

  // 弹窗定位：useLayoutEffect 在绘制前执行，+ visibility 控制避免闪烁
  useLayoutEffect(() => {
    if (!open) { setReady(false); setDropdownStyle({}); return; }
    const btn = ref.current?.getBoundingClientRect();
    const menu = menuRef.current?.getBoundingClientRect();
    if (!btn || !menu) { setReady(true); return; }
    const st: React.CSSProperties = {
      position: 'absolute', left: -4,
      minWidth: 130, background: '#1e1e1e',
      border: '1px solid #333', borderRadius: 8,
      padding: '4px 0', zIndex: 50,
      boxShadow: '0 4px 12px rgba(0,0,0,0.4)',
    };
    const spaceBelow = window.innerHeight - btn.bottom;
    if (spaceBelow >= menu.height + 4) {
      st.top = '100%';
    } else {
      const spaceAbove = btn.top;
      if (spaceAbove >= menu.height + 4) {
        st.bottom = '100%';
      } else {
        st.top = spaceBelow > spaceAbove ? '100%' : undefined;
        st.bottom = spaceBelow <= spaceAbove ? '100%' : undefined;
      }
    }
    const spaceRight = window.innerWidth - btn.left;
    if (spaceRight < menu.width + 4) {
      st.right = -4;
      st.left = undefined;
    }
    setDropdownStyle(st);
    setReady(true);
  }, [open]);

  const selected = options.find(o => o.value === value);

  return (
    <div ref={ref} style={{ position: 'relative', userSelect: 'none', ...style }}>
      <div
        onClick={toggle}
        style={{
          display: 'flex', alignItems: 'center', gap: 5,
          padding: '1px 2px', cursor: 'pointer',
          color: '#aaa', fontSize: 11,
          border: 'none', background: 'transparent',
        }}
      >
        <span style={{ color: !selected && placeholder ? '#666' : open ? '#fff' : '#aaa' }}>{selected?.label || placeholder || value}</span>
        <svg width="8" height="5" viewBox="0 0 8 5" style={{ opacity: 0.5 }}>
          <path d="M0 0l4 5 4-5" fill="none" stroke="#999" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round"/>
        </svg>
      </div>
      {open && (
        <div ref={menuRef} style={{ ...dropdownStyle, visibility: ready ? 'visible' : 'hidden' }}>
          {options.map(o => (
            <div
              key={o.value}
              onClick={() => { onChange(o.value); setOpen(false); }}
              style={{
                padding: '5px 10px', cursor: 'pointer',
                fontSize: 12, color: o.value === value ? '#4CAF50' : (o.special ? '#888' : '#bbb'),
                fontStyle: o.special ? 'italic' : 'normal',
                whiteSpace: 'nowrap',
              }}
              onMouseEnter={e => (e.currentTarget.style.background = 'rgba(255,255,255,0.06)')}
              onMouseLeave={e => (e.currentTarget.style.background = 'transparent')}
            >
              {o.label}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
