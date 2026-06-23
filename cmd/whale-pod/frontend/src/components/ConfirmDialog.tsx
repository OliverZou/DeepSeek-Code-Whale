import { useEffect, useRef } from 'react';

interface ConfirmDialogProps {
  open: boolean;
  title: string;
  message: string;
  confirmLabel?: string;
  danger?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}

export default function ConfirmDialog({ open, title, message, confirmLabel = '确定', danger, onConfirm, onCancel }: ConfirmDialogProps) {
  const cancelRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    if (open) cancelRef.current?.focus();
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const handler = (e: KeyboardEvent) => { if (e.key === 'Escape') onCancel(); };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [open, onCancel]);

  if (!open) return null;

  return (
    <div style={{
      position: 'fixed', inset: 0, zIndex: 9999,
      display: 'flex', alignItems: 'center', justifyContent: 'center',
      background: 'rgba(0,0,0,0.5)', backdropFilter: 'blur(2px)',
    }} onClick={onCancel}>
      <div style={{
        background: '#1e1e1e', border: '1px solid rgba(255,255,255,0.1)',
        borderRadius: 12, padding: '24px 28px', minWidth: 320, maxWidth: 420,
        boxShadow: '0 8px 32px rgba(0,0,0,0.4)',
      }} onClick={e => e.stopPropagation()}>
        <div style={{ fontSize: 15, fontWeight: 600, color: '#eee', marginBottom: 8 }}>{title}</div>
        <div style={{ fontSize: 13, color: '#999', lineHeight: 1.6, marginBottom: 24 }}>{message}</div>
        <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 10 }}>
          <button
            ref={cancelRef}
            onClick={onCancel}
            style={{
              padding: '7px 18px', borderRadius: 8, border: '1px solid rgba(255,255,255,0.12)',
              background: 'transparent', color: '#aaa', fontSize: 13, cursor: 'pointer',
            }}
          >取消</button>
          <button
            onClick={onConfirm}
            style={{
              padding: '7px 18px', borderRadius: 8, border: 'none',
              background: danger ? '#c0392b' : '#4CAF50', color: '#fff',
              fontSize: 13, cursor: 'pointer', fontWeight: 500,
            }}
          >{confirmLabel}</button>
        </div>
      </div>
    </div>
  );
}