import { useCallback, useRef } from 'react';

interface Props { target: string; side: 'left' | 'right'; }

export default function Resizer({ target, side }: Props) {
  const ghostRef = useRef<HTMLDivElement>(null);

  const onDown = useCallback((e: React.MouseEvent) => {
    e.preventDefault();
    const el = document.getElementById(target);
    if (!el) return;
    const startX = e.clientX;
    const startW = el.getBoundingClientRect().width;
    ghostRef.current?.classList.add('active');

    const onMove = (ev: MouseEvent) => {
      let dx = ev.clientX - startX;
      if (side === 'left') dx = -dx;
      let w = startW + dx;
      w = Math.max(160, Math.min(500, w));
      el.style.width = w + 'px';
    };
    const onUp = () => {
      ghostRef.current?.classList.remove('active');
      document.removeEventListener('mousemove', onMove);
      document.removeEventListener('mouseup', onUp);
    };
    document.addEventListener('mousemove', onMove);
    document.addEventListener('mouseup', onUp);
  }, [target, side]);

  return (
    <>
      <div id="resize-ghost" ref={ghostRef} />
      <div className="resizer" onMouseDown={onDown} />
    </>
  );
}
