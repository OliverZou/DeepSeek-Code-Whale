import { useState, useRef, useLayoutEffect } from 'react';
import { createPortal } from 'react-dom';

export function useMenu() {
  const [open, setOpen] = useState(false);
  const [ready, setReady] = useState(false);
  const [pos, setPos] = useState({ x: 0, y: 0 });
  const [items, setItems] = useState<{ label: string; onClick: () => void }[]>([]);
  const ref = useRef<any>(null);
  const menuRef = useRef<HTMLDivElement>(null);

  const show = () => {
    setReady(false);
    setOpen(true);
  };

  const showWith = (menuItems: { label: string; onClick: () => void }[]) => {
    setItems(menuItems);
    setReady(false);
    setOpen(true);
  };

  const hide = () => { setOpen(false); setReady(false); };

  useLayoutEffect(() => {
    if (!open) return;
    const btn = ref.current?.getBoundingClientRect();
    if (!btn) { setReady(true); return; }
    requestAnimationFrame(() => {
      const menu = menuRef.current?.getBoundingClientRect();
      if (!menu) { setReady(true); return; }
      let nx = btn.right + 4;
      let ny = btn.bottom - menu.height;
      if (nx + menu.width > window.innerWidth) nx = btn.left - menu.width - 4;
      if (ny < 0) ny = btn.top;
      setPos({ x: nx, y: ny });
      setReady(true);
    });
  }, [open]);

  const portal = open ? createPortal(
    <>
      <div
        onMouseDown={e => { e.preventDefault(); e.stopPropagation(); hide(); }}
        onClick={e => { e.preventDefault(); e.stopPropagation(); hide(); }}
        onContextMenu={e => { e.preventDefault(); e.stopPropagation(); hide(); }}
        style={{ position: 'fixed', inset: 0, zIndex: 1001 }}
      />
      <div ref={menuRef} style={{
        position: 'fixed', left: pos.x, top: pos.y, zIndex: 1002,
        background: '#1e1e1e', border: '1px solid #333', borderRadius: 8,
        padding: '4px 0', minWidth: 100, boxShadow: '0 4px 12px rgba(0,0,0,0.4)',
        visibility: ready ? 'visible' : 'hidden',
      }}>
        {items.map((item, i) => (
          <div
            key={i}
            onClick={e => { e.stopPropagation(); item.onClick(); hide(); }}
            style={{
              padding: '5px 12px', cursor: 'pointer', fontSize: 12, color: '#bbb',
              whiteSpace: 'nowrap',
            }}
            onMouseEnter={e => (e.currentTarget.style.background = 'rgba(255,255,255,0.06)')}
            onMouseLeave={e => (e.currentTarget.style.background = 'transparent')}
          >{item.label}</div>
        ))}
      </div>
    </>,
    document.body
  ) : null;

  return { open, show, showWith, hide, ref, portal };
}
