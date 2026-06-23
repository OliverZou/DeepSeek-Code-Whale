import { useState, type ReactNode } from 'react';

interface Props {
  title: string;
  icon?: ReactNode;
  defaultOpen?: boolean;
  action?: ReactNode;
  children: ReactNode;
}

export default function CollapsibleSection({ title, icon, defaultOpen = true, action, children }: Props) {
  const [open, setOpen] = useState(defaultOpen);

  return (
    <div className="section">
      <div
        className="section-title"
        onClick={() => setOpen(!open)}
        style={{ cursor: 'pointer' }}
      >
        <span style={{ fontSize: 13, fontWeight: 600, display: 'inline-flex', alignItems: 'center' }}>
          {icon}
          <span style={{ marginLeft: 4 }}>{title}</span>
        </span>
        <span style={{ display: 'inline-flex', alignItems: 'center', gap: 4 }}>
          {action}
          <svg
            width="8" height="5" viewBox="0 0 8 5"
            style={{
              opacity: 0.5,
              transition: 'transform 0.15s',
              transform: open ? 'rotate(0deg)' : 'rotate(-90deg)',
            }}
          >
            <path d="M0 0l4 5 4-5" fill="none" stroke="#999" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round"/>
          </svg>
        </span>
      </div>
      {open && children}
    </div>
  );
}
