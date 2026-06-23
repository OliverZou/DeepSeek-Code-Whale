import { useState } from 'react';
import { useStore } from '../store';
import { api } from '../wails';
import CollapsibleSection from './CollapsibleSection';
import { useMenu } from './ContextMenu';
import type { SummonedItem as SummonedItemType } from '../types';

function TeamItem({ item, open, onToggle }: { item: SummonedItemType; open: boolean; onToggle: () => void }) {
  const menu = useMenu();
  const [hover, setHover] = useState(false);
  return (
    <div>
      <div
        className="tree-dir"
        onMouseEnter={() => setHover(true)}
        onMouseLeave={() => setHover(false)}
        style={{ cursor: 'pointer', display: 'flex', alignItems: 'center', gap: 6, position: 'relative' }}
      >
        <span onClick={onToggle} style={{ display: 'flex', alignItems: 'center', gap: 6, flex: 1 }}>
          <svg width="8" height="5" viewBox="0 0 8 5" style={{
            opacity: 0.5, flexShrink: 0, transition: 'transform 0.15s',
            transform: open ? 'rotate(0deg)' : 'rotate(-90deg)',
          }}>
            <path d="M0 0l4 5 4-5" fill="none" stroke="#999" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round"/>
          </svg>
          {item.label || item.name}
        </span>
        <div
          ref={menu.ref}
          onClick={e => { e.stopPropagation(); e.preventDefault(); menu.showWith([
            { label: '发起对话', onClick: () => useStore.setState({ activeFunction: 'create' }) },
            { label: '删除', onClick: () => {
              const next = useStore.getState().summonedItems.filter(st => !(st.name === item.name && st.type === item.type));
              useStore.setState({ summonedItems: next });
              api.saveSummonedItems(next);
            }},
          ]); }}
          style={{ position: 'relative', display: 'flex', alignItems: 'center', justifyContent: 'center', width: 24, height: 24, cursor: 'pointer' }}
        >
          <svg
            width="14" height="3" viewBox="0 0 14 3" fill="currentColor"
            style={{ cursor: 'pointer', opacity: hover || menu.open ? 0.6 : 0, transition: 'opacity 0.15s' }}
          >
            <circle cx="1.5" cy="1.5" r="1.5"/><circle cx="7" cy="1.5" r="1.5"/><circle cx="12.5" cy="1.5" r="1.5"/>
          </svg>
          {menu.portal}
        </div>
      </div>
      {open && item.type === 'team' && (
        <div className="tree-item" style={{ fontSize: 12, paddingLeft: 28 }}>
          <span className="tree-label">{item.description || '专家团'}</span>
        </div>
      )}
    </div>
  );
}

export default function TeamList() {
  const summonedItems = useStore(s => s.summonedItems) || [];
  const [expanded, setExpanded] = useState<Set<string>>(new Set());

  const toggle = (name: string) => {
    setExpanded(prev => {
      const next = new Set(prev);
      next.has(name) ? next.delete(name) : next.add(name);
      return next;
    });
  };

  const icon = (
    <svg width="18" height="18" viewBox="0 0 1024 1024" fill="currentColor" xmlns="http://www.w3.org/2000/svg" style={{ flexShrink: 0 }}>
      <path d="M824.2 699.9c-25.4-25.4-54.7-45.7-86.4-60.4C783.1 602.8 812 546.8 812 484c0-110.8-92.4-201.7-203.2-200-109.1 1.7-197 90.6-197 200 0 62.8 29 118.8 74.2 155.5-31.7 14.7-60.9 34.9-86.4 60.4C345 754.6 314 826.8 312 903.8c-0.1 4.5 3.5 8.2 8 8.2h56c4.3 0 7.9-3.4 8-7.7 1.9-58 25.4-112.3 66.7-153.5C493.8 707.7 551.1 684 612 684c60.9 0 118.2 23.7 161.3 66.8C814.5 792 838 846.3 840 904.3c0.1 4.3 3.7 7.7 8 7.7h56c4.5 0 8.1-3.7 8-8.2-2-77-33-149.2-87.8-203.9zM612 612c-34.2 0-66.4-13.3-90.5-37.5-24.5-24.5-37.9-57.1-37.5-91.8 0.3-32.8 13.4-64.5 36.3-88 24-24.6 56.1-38.3 90.4-38.7 33.9-0.3 66.8 12.9 91 36.6 24.8 24.3 38.4 56.8 38.4 91.4 0 34.2-13.3 66.3-37.5 90.5-24.2 24.2-56.4 37.5-90.6 37.5z"/>
      <path d="M361.5 510.4c-0.9-8.7-1.4-17.5-1.4-26.4 0-15.9 1.5-31.4 4.3-46.5 0.7-3.6-1.2-7.3-4.5-8.8-13.6-6.1-26.1-14.5-36.9-25.1-25.8-25.2-39.7-59.3-38.7-95.4 0.9-32.1 13.8-62.6 36.3-85.6 24.7-25.3 57.9-39.1 93.2-38.7 31.9 0.3 62.7 12.6 86 34.4 7.9 7.4 14.7 15.6 20.4 24.4 2 3.1 5.9 4.4 9.3 3.2 17.6-6.1 36.2-10.4 55.3-12.4 5.6-0.6 8.8-6.6 6.3-11.6-32.5-64.3-98.9-108.7-175.7-109.9-110.9-1.7-203.3 89.2-203.3 199.9 0 62.8 28.9 118.8 74.2 155.5-31.8 14.7-61.1 35-86.5 60.4-54.8 54.7-85.8 126.9-87.8 204-0.1 4.5 3.5 8.2 8 8.2h56.1c4.3 0 7.9-3.4 8-7.7 1.9-58 25.4-112.3 66.7-153.5 29.4-29.4 65.4-49.8 104.7-59.7 3.9-1 6.5-4.7 6-8.7z"/>
    </svg>
  );

  return (
    <CollapsibleSection title="AI 团队" icon={icon} defaultOpen>
      {summonedItems.map(t => (
        <TeamItem key={`${t.type}:${t.name}`} item={t} open={expanded.has(t.name)} onToggle={() => toggle(t.name)} />
      ))}
      {summonedItems.length === 0 && (
        <div className="empty-hint" style={{ padding: '4px 14px' }}>暂无 AI 专家，请在专家面板召唤</div>
      )}
    </CollapsibleSection>
  );
}
