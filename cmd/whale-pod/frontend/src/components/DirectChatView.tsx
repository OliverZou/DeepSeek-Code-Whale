import { useState, useRef, useEffect } from 'react';
import { useStore } from '../store';
import { api } from '../wails';
import { marked } from 'marked';
import MinimalSelect from './MinimalSelect';

export default function DirectChatView() {
  const { directMessages, directChatTaskId, sendDirectChat, selMasterTaskId, summonedItems } = useStore();
  const [input, setInput] = useState('');
  const [focused, setFocused] = useState(false);
  const [sending, setSending] = useState(false);
  const [deepThink, setDeepThink] = useState(false);
  const [chatMode, setChatMode] = useState<'chat' | 'plan' | 'agent'>('chat');
  const [expandedThinking, setExpandedThinking] = useState<Set<number>>(new Set());
  const [expert, setExpert] = useState('');
  const msgsRef = useRef<HTMLDivElement>(null);
  const taRef = useRef<HTMLTextAreaElement>(null);

  const expertOptions = [
    { value: '', label: '🐋 Whale' },
    ...summonedItems.map(t => ({ value: t.name, label: t.label || t.name })),
    { value: '__summon__', label: '召唤其他专家…', special: true },
  ];

  const handleExpert = (v: string) => {
    if (v === '__summon__') {
      useStore.setState({ activeFunction: 'expert' });
      return;
    }
    setExpert(v);
  };

  const charCount = input.trim().length;

  useEffect(() => {
    if (msgsRef.current) {
      msgsRef.current.scrollTop = msgsRef.current.scrollHeight;
    }
  }, [directMessages]);

  // Load messages when task changed
  const taskId = directChatTaskId || selMasterTaskId;
  useEffect(() => {
    if (!taskId) return;
    useStore.setState({ directMessages: [] });
    api.getChatMessages(taskId).then(msgs => {
      useStore.setState({ directMessages: msgs || [] });
    });
  }, [taskId]);

  const handleSend = async () => {
    const msg = input.trim();
    if (!msg || sending) return;
    setInput('');
    setSending(true);
    await sendDirectChat(msg, deepThink);
    setSending(false);
  };

  const keyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      handleSend();
    }
  };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', background: '#141414' }}>
      {/* messages */}
      <div ref={msgsRef} style={{ flex: 1, overflowY: 'auto', padding: '16px 10%' }}>
        {directMessages.length === 0 && (
          <div style={{ textAlign: 'center', color: '#666', marginTop: 40 }}>开始新对话</div>
        )}
        {directMessages.map((m, i) => (
          <div
            key={i}
            style={{
              display: 'flex', flexDirection: 'column',
              alignItems: m.from === 'human' ? 'flex-end' : 'flex-start',
              marginBottom: 16,
            }}
          >
            {m.from !== 'human' && (
              <div style={{ fontSize: 13, color: '#888', marginBottom: 4, marginLeft: 4 }}>
                <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
                  <img src="/whale.png" width="24" height="24" alt="" style={{ borderRadius: 4 }} />
                  Whale Pod
                  {m.durationMs != null && (
                    <span
                      onClick={() => {
                        if (!m.thinking) return;
                        setExpandedThinking(prev => {
                          const next = new Set(prev);
                          next.has(i) ? next.delete(i) : next.add(i);
                          return next;
                        });
                      }}
                      style={{
                        display: 'inline-flex', alignItems: 'center', gap: 3,
                        cursor: m.thinking ? 'pointer' : 'default',
                        color: '#555', fontSize: 11, fontWeight: 400, marginLeft: 4,
                      }}
                    >
                       <span style={{ display: 'inline-flex', alignItems: 'center', gap: 1 }}>
                        思考 {(m.durationMs / 1000).toFixed(1)}s
                      </span>
                      {m.thinking && (
                        <svg width="8" height="5" viewBox="0 0 8 5" style={{
                          opacity: 0.5, transition: 'transform 0.15s',
                          transform: expandedThinking.has(i) ? 'rotate(0deg)' : 'rotate(-90deg)',
                        }}>
                          <path d="M0 0l4 5 4-5" fill="none" stroke="#999" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round"/>
                        </svg>
                      )}
                    </span>
                  )}
                </span>
              </div>
            )}
            {/* thinking content */}
            {m.thinking && expandedThinking.has(i) && (
              <div style={{
                marginLeft: 34, marginBottom: 4, padding: '8px 12px',
                background: 'rgba(0,0,0,0.25)', borderRadius: 8,
                fontSize: 12, color: '#777', lineHeight: 1.5,
                maxWidth: '80%',
              }}>
                {m.thinking}
              </div>
            )}
            <div
              className={`chat-md ${m.from}`}
              style={{
                maxWidth: '80%', padding: '10px 16px', borderRadius: 14,
                fontSize: 14, lineHeight: 1.6,
                wordBreak: 'break-word',
                background: m.from === 'human' ? 'rgba(76,175,80,0.15)' : '#1e1e1e',
                color: m.from === 'human' ? '#fff' : '#e0e0e0',
                border: m.from === 'human' ? '1px solid rgba(76,175,80,0.25)' : '1px solid #333',
                borderBottomRightRadius: m.from === 'human' ? 4 : 14,
                borderBottomLeftRadius: m.from === 'human' ? 14 : 4,
              }}
              dangerouslySetInnerHTML={{ __html: marked.parse(m.content) as string }}
            />
          </div>
        ))}
        {sending && (
          <div style={{ color: '#666', fontSize: 12, padding: 8 }}>AI 思考中…</div>
        )}
        {/* action confirmation */}
        {directMessages.length > 0 && (() => {
          const last = directMessages[directMessages.length - 1];
          if (last.from === 'agent' && last.needsAction) {
            // Only show if current mode is lower than needed
            const needed = last.actionType || 'agent';
            const modeRank: Record<string, number> = { chat: 0, plan: 1, agent: 2 };
            if (modeRank[chatMode] >= modeRank[needed]) return null;
            const label = needed === 'agent' ? '需要我执行这些操作吗？' : '需要我帮你规划一下吗？';
            const targetMode = needed === 'agent' ? 'agent' as const : 'plan' as const;
            return (
              <div style={{
                display: 'flex', justifyContent: 'flex-start', padding: '0 0 12px',
              }}>
                <button
                  onClick={() => setChatMode(targetMode)}
                  style={{
                    marginLeft: 34, padding: '8px 18px', borderRadius: 10,
                    border: '1px solid #4CAF50', background: 'rgba(76,175,80,0.12)',
                    color: '#4CAF50', fontSize: 13, fontWeight: 600, cursor: 'pointer',
                  }}
                >{label}</button>
              </div>
            );
          }
          return null;
        })()}
      </div>

      {/* input */}
      <div style={{ padding: '0 10% 20px' }}>
        <div style={{
          position: 'relative',
          border: `1px solid ${focused ? '#4CAF50' : '#333'}`,
          borderRadius: 12, background: '#1e1e1e',
          transition: 'border-color 0.2s',
          boxShadow: focused ? '0 0 0 2px rgba(76,175,80,0.12)' : 'none',
        }}>
          <textarea
            ref={taRef}
            value={input}
            onChange={e => setInput(e.target.value)}
            onFocus={() => setFocused(true)}
            onBlur={() => setFocused(false)}
            onKeyDown={keyDown}
            placeholder="输入消息… Enter 发送"
            style={{
              width: '100%', minHeight: 112, maxHeight: 280,
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
              onClick={handleSend}
              disabled={!charCount || sending}
              style={{
                width: 28, height: 28, borderRadius: 8, border: 'none',
                background: (charCount && !sending) ? '#4CAF50' : '#333',
                cursor: (charCount && !sending) ? 'pointer' : 'default',
                display: 'flex', alignItems: 'center', justifyContent: 'center',
                opacity: (charCount && !sending) ? 1 : 0.4,
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
