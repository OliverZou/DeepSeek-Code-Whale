import { useState, useRef, useEffect, useCallback } from 'react';
import { useStore } from '../store';
import { api } from '../wails';

function TabItem({ tabId, active, fullLabel, truncated, isLast, onSelect, onClose, onRename }: {
  tabId: string; active: boolean; fullLabel: string; truncated: string;
  isLast: boolean; onSelect: () => void; onClose: () => void;
  onRename: (val: string) => void;
}) {
  const [hover, setHover] = useState(false);
  const [renaming, setRenaming] = useState(false);
  const [editValue, setEditValue] = useState('');
  const inputRef = useRef<HTMLInputElement>(null);

  const startRename = () => {
    setEditValue(fullLabel);
    setRenaming(true);
    setTimeout(() => inputRef.current?.select(), 0);
  };

  const commitRename = () => {
    const val = editValue.trim();
    if (val) onRename(val);
    setRenaming(false);
  };

  return (
    <div
      data-tab-id={tabId}
      onClick={() => { if (!renaming) onSelect(); }}
      onDoubleClick={e => { e.stopPropagation(); startRename(); }}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      title={renaming ? undefined : fullLabel}
      style={{
        display: 'flex', alignItems: 'center', gap: 3,
        padding: '0 10px', cursor: 'pointer',
        fontSize: 12, color: active ? '#eee' : '#777',
        background: active ? 'rgba(76,175,80,0.1)' : hover ? 'rgba(255,255,255,0.03)' : 'transparent',
        borderRight: '1px solid rgba(255,255,255,0.04)',
        borderBottom: active ? '2px solid #4CAF50' : '2px solid transparent',
        whiteSpace: 'nowrap', maxWidth: 160,
        transition: 'background 0.12s, color 0.12s',
      }}
    >
      {renaming ? (
        <input
          ref={inputRef}
          value={editValue}
          onChange={e => setEditValue(e.target.value)}
          onBlur={commitRename}
          onKeyDown={e => {
            if (e.key === 'Enter') commitRename();
            if (e.key === 'Escape') setRenaming(false);
          }}
          onClick={e => e.stopPropagation()}
          style={{
            width: 100, background: '#1e1e1e', border: '1px solid #4CAF50',
            borderRadius: 3, color: '#fff', fontSize: 12, padding: '1px 4px',
            outline: 'none',
          }}
        />
      ) : (
        <span style={{ overflow: 'hidden', textOverflow: 'ellipsis' }}>{truncated}</span>
      )}
      {!isLast && (
        <span
          onClick={e => { e.stopPropagation(); onClose(); }}
          title="关闭标签"
          style={{
            color: '#555', fontSize: 10, cursor: 'pointer',
            opacity: hover ? 1 : 0, transition: 'opacity 0.12s',
            padding: '0 1px', flexShrink: 0, lineHeight: 1,
            ...(hover ? { color: '#f44336' } : {}),
          }}
        >✕</span>
      )}
    </div>
  );
}

export default function ChatTabBar() {
  const openTabs = useStore(s => s.openTabs);
  const selAgentId = useStore(s => s.selAgentId);
  const selMasterTaskId = useStore(s => s.selMasterTaskId);
  const masterTasks = useStore(s => s.masterTasks);
  const selectMasterTask = useStore(s => s.selectMasterTask);
  const removeTab = useStore(s => s.removeTab);

  const currentTabs = openTabs[selAgentId || ''] || [];

  const scrollRef = useRef<HTMLDivElement>(null);
  const [hiddenTabs, setHiddenTabs] = useState<string[]>([]);
  const [overflowOpen, setOverflowOpen] = useState(false);

  const checkOverflow = useCallback(() => {
    const el = scrollRef.current;
    if (!el) return;
    const hidden: string[] = [];
    const children = el.children;
    for (let i = 0; i < children.length; i++) {
      const child = children[i] as HTMLElement;
      const r = child.getBoundingClientRect();
      const pr = el.getBoundingClientRect();
      if (r.right > pr.right + 1) {
        hidden.push(child.dataset.tabId || '');
      }
    }
    setHiddenTabs(hidden);
  }, []);

  useEffect(() => {
    checkOverflow();
    const el = scrollRef.current;
    if (!el) return;
    const ro = new ResizeObserver(checkOverflow);
    ro.observe(el);
    // Also observe children changes
    for (const child of el.children) ro.observe(child);
    return () => ro.disconnect();
  }, [currentTabs, checkOverflow]);

  if (currentTabs.length === 0) return null;

  const handleRename = async (tabId: string, newGoal: string) => {
    await api.renameMasterTask(tabId, newGoal);
    useStore.getState().loadMasterTasks();
  };

  return (
    <>
      <div ref={scrollRef} style={{ display: 'flex', alignItems: 'stretch', gap: 0, overflow: 'hidden', flex: 1, minWidth: 0, height: '100%' }}>
        {currentTabs.map((tabId) => {
          const mt = masterTasks.find(t => t.id === tabId);
          const fullLabel = mt?.goal || tabId.slice(0, 8);
          const truncated = fullLabel.length > 12 ? fullLabel.slice(0, 10) + '…' : fullLabel;
          return (
            <TabItem
              key={tabId}
              tabId={tabId}
              active={tabId === selMasterTaskId}
              fullLabel={fullLabel}
              truncated={truncated}
              isLast={currentTabs.length === 1}
              onSelect={() => selectMasterTask(tabId)}
              onClose={() => removeTab(tabId)}
              onRename={(val: string) => handleRename(tabId, val)}
            />
          );
        })}
      </div>
      {/* overflow chevron */}
      {hiddenTabs.length > 0 && (
        <div style={{ position: 'relative', display: 'flex', alignItems: 'center', flexShrink: 0 }}>
          <div
            onClick={() => setOverflowOpen(!overflowOpen)}
            style={{
              display: 'flex', alignItems: 'center', justifyContent: 'center',
              width: 28, height: '100%', cursor: 'pointer',
              color: '#888', borderLeft: '1px solid rgba(255,255,255,0.06)',
            }}
            title="更多标签…"
          >
            <svg width="10" height="6" viewBox="0 0 10 6" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
              <path d="M1 1l4 4 4-4"/>
            </svg>
          </div>
          {overflowOpen && (
            <>
              <div
                style={{ position: 'fixed', inset: 0, zIndex: 99 }}
                onClick={() => setOverflowOpen(false)}
              />
              <div style={{
                position: 'absolute', top: '100%', right: 0,
                minWidth: 130, background: '#1e1e1e',
                border: '1px solid #333', borderRadius: 8,
                padding: '4px 0', zIndex: 100,
                boxShadow: '0 4px 12px rgba(0,0,0,0.4)',
              }}>
                {hiddenTabs.map(tabId => {
                  const mt = masterTasks.find(t => t.id === tabId);
                  const label = mt?.goal || tabId.slice(0, 8);
                  const active = tabId === selMasterTaskId;
                  return (
                    <div
                      key={tabId}
                      onClick={() => { selectMasterTask(tabId); setOverflowOpen(false); }}
                      style={{
                        padding: '5px 10px', cursor: 'pointer',
                        fontSize: 12, color: active ? '#4CAF50' : '#bbb',
                        whiteSpace: 'nowrap',
                      }}
                      onMouseEnter={e => (e.currentTarget.style.background = 'rgba(255,255,255,0.06)')}
                      onMouseLeave={e => (e.currentTarget.style.background = 'transparent')}
                    >
                      {label.length > 24 ? label.slice(0, 22) + '…' : label}
                    </div>
                  );
                })}
              </div>
            </>
          )}
        </div>
      )}
    </>
  );
}
