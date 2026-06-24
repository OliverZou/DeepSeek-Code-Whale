import { useState, useRef, useEffect } from 'react';
import { useStore } from '../store';
import { api } from '../wails';
import MinimalSelect from './MinimalSelect';

export default function CreateTaskView() {
  const { workDir, openWorkspaces, startDirectChat, openWorkspace, summonedItems, preselectedExpert } = useStore();
  const [goal, setGoal] = useState('');
  const [dir, setDir] = useState('');
  const [loading, setLoading] = useState(false);
  const [focused, setFocused] = useState(false);
  const [deepThink, setDeepThink] = useState(false);
  const [chatMode, setChatMode] = useState<'chat' | 'plan' | 'agent'>('chat');
  const [expert, setExpert] = useState('');
  const taRef = useRef<HTMLTextAreaElement>(null);
  useEffect(() => {
    if (!preselectedExpert) return;
    // 找到对应的 summon 条目
    const item = summonedItems.find(s => s.name === preselectedExpert);
    if (item) {
      setExpert(`${item.type}:${item.name}`);
    }
    // 消费后清除，避免下次进入又选中
    useStore.setState({ preselectedExpert: '' });
  }, []);

  const workspaceOptions = [
    ...(dir ? [{ value: dir, label: dir.split('\\').pop() || dir }] : []),
    ...openWorkspaces.filter(d => d !== dir).map((d: string) => ({ value: d, label: d.split('\\').pop() || d })),
    { value: '__pick__', label: '选择文件夹…', special: true },
    { value: '', label: '不用工作空间', special: true },
  ];

  const expertOptions = [
    { value: '', label: '🐋 Whale' },
    ...summonedItems.filter(s => s.type === 'expert').map(s => ({ value: `expert:${s.name}`, label: `🧑‍💻 ${s.label || s.name}` })),
    ...summonedItems.filter(s => s.type === 'team').map(s => ({ value: `team:${s.name}`, label: `👥 ${s.label || s.name}` })),
    { value: '__summon__', label: '召唤其他专家…', special: true },
  ];

  const handleWorkspace = async (v: string) => {
    if (v === '__pick__') {
      const picked = await api.pickFolder();
      if (picked) {
        setDir(picked);
        await openWorkspace(picked);
      }
      return;
    }
    setDir(v);
  };

  const handleExpert = (v: string) => {
    if (v === '__summon__') {
      useStore.setState({ activeFunction: 'expert' });
      return;
    }
    setExpert(v);
  };

  const handleSubmit = async (text?: string) => {
    const g = (text || goal).trim();
    if (!g) return;
    setLoading(true);
    // 所有 agent 统一走聊天，任务以后再启动
    await startDirectChat(g, dir || undefined, expert || undefined, deepThink);
    setGoal('');
    setLoading(false);
  };

  const keyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      handleSubmit();
    }
  };

  const charCount = goal.trim().length;

  return (
    <div style={{
      display: 'flex', flexDirection: 'column', alignItems: 'center',
      justifyContent: 'center', height: '100%', padding: '40px 10%',
      background: '#141414'
    }}>
      <img src="/whale.png" width="128" height="128" alt="Whale" style={{ marginBottom: 16 }} />

      <div style={{ fontSize: 22, fontWeight: 700, color: '#fff', marginBottom: 4 }}>
        Whale Pod Team Agent
      </div>

      <div style={{ width: '100%', marginTop: 12, alignSelf: 'stretch' }}>

        {/* workspace selector */}
        <div style={{ display: 'flex', alignItems: 'center', gap: 4, marginBottom: 8, paddingLeft: 14 }}>
          <span style={{ fontSize: 12, color: '#888', whiteSpace: 'nowrap' }}>运行于</span>
          <MinimalSelect value={dir} options={workspaceOptions} onChange={handleWorkspace} />
        </div>

        {/* input area */}
        <div style={{
          position: 'relative',
          border: `1px solid ${focused ? '#4CAF50' : '#333'}`,
          borderRadius: 12, background: '#1e1e1e',
          transition: 'border-color 0.2s',
          boxShadow: focused ? '0 0 0 2px rgba(76,175,80,0.12)' : 'none',
          marginBottom: 14,
        }}>
          <textarea
            ref={taRef}
            value={goal}
            onChange={e => setGoal(e.target.value)}
            onFocus={() => setFocused(true)}
            onBlur={() => setFocused(false)}
            onKeyDown={keyDown}
            placeholder="输入问题或任务，Enter 开始对话…"
            style={{
              width: '100%', minHeight: 120,
              background: 'transparent', border: 'none', outline: 'none',
              color: '#e0e0e0', fontSize: 14, lineHeight: 1.6,
              padding: '12px 46px 12px 14px', resize: 'none',
              fontFamily: 'inherit'
            }}
          />

          {/* bottom toolbar: 专家 | ... | 深度思考 模式切换 [Enter] [send] */}
          <div style={{
            position: 'absolute', bottom: 6, left: 10, right: 10,
            display: 'flex', alignItems: 'center', gap: 4,
          }}>
            {/* expert selector — left */}
            <MinimalSelect value={expert} options={expertOptions} onChange={handleExpert} style={{ zIndex: 1 }} />

            {/* spacer */}
            <div style={{ flex: 1 }} />

            {/* deep think toggle */}
            <div
              onClick={() => setDeepThink(!deepThink)}
              title="开启后使用 deepseek-reasoner 模型，回复更深入但更慢"
              style={{
                display: 'flex', alignItems: 'center', gap: 4,
                cursor: 'pointer', opacity: deepThink ? 1 : 0.35,
                transition: 'opacity 0.15s', marginRight: 6,
              }}
            >
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke={deepThink ? '#4CAF50' : '#888'} strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
                <path d="M15 14c.2-1 .7-1.7 1.5-2.5 1-.9 1.5-2.2 1.5-3.5A6 6 0 0 0 6 8c0 1 .2 2.2 1.5 3.5.7.7 1.3 1.5 1.5 2.5"/>
                <path d="M9 18h6"/>
                <path d="M10 22h4"/>
              </svg>
              <span style={{ fontSize: 12, color: deepThink ? '#4CAF50' : '#555' }}>深度思考</span>
            </div>

            {/* mode selector */}
            <div style={{ display: 'flex', alignItems: 'center', gap: 2, marginRight: 4 }}>
              {([
                ['chat' as const, 'M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z'],
                ['plan' as const, 'M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z M14 2v6h6 M16 13H8 M16 17H8 M10 9H8'],
                ['agent' as const, 'M13 2L3 14h9l-1 8 10-12h-9l1-8z'],
              ]).map(([m, d]) => (
                <div
                  key={m}
                  onClick={() => setChatMode(m as 'chat' | 'plan' | 'agent')}
                  title={m === 'chat' ? '纯聊天' : m === 'plan' ? '规划模式' : '操作模式'}
                  style={{
                    padding: '2px 4px', cursor: 'pointer',
                    color: chatMode === m ? '#4CAF50' : '#555',
                    transition: 'all 0.15s',
                  }}
                >
                  <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
                    <path d={d}/>
                  </svg>
                </div>
              ))}
            </div>

            {/* Enter hint + send button */}
            <span style={{ fontSize: 10, color: '#555' }}>
              {charCount > 0 ? `${charCount}字` : 'Enter'}
            </span>
            <button
              onClick={() => handleSubmit()}
              disabled={!charCount || loading}
              style={{
                width: 28, height: 28, borderRadius: 8, border: 'none',
                background: (charCount && !loading) ? '#4CAF50' : '#333',
                cursor: (charCount && !loading) ? 'pointer' : 'default',
                display: 'flex', alignItems: 'center', justifyContent: 'center',
                opacity: (charCount && !loading) ? 1 : 0.4,
                transition: 'all .2s', flexShrink: 0,
              }}
            >
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="white" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round">
                <line x1="12" y1="5" x2="12" y2="19" />
                <polyline points="5 12 12 5 19 12" />
              </svg>
            </button>
          </div>

        </div>
      </div>
    </div>
  );
}
