import { useState, useCallback } from 'react';
import { Prism as SyntaxHighlighter } from 'react-syntax-highlighter';
import { oneDark } from 'react-syntax-highlighter/dist/esm/styles/prism';
import { api } from '../wails';
import { useStore } from '../store';
import type { ToolCall } from '../types';

interface ToolCallCardProps {
  toolCall: ToolCall;
}

const TYPE_CONFIG: Record<string, { icon: string; color: string; bg: string; label: string }> = {
  read_file:  { icon: '📖', color: '#58a6ff', bg: 'rgba(88,166,255,0.08)',  label: '读取' },
  write_file: { icon: '✏️', color: '#3fb950', bg: 'rgba(63,185,80,0.08)',   label: '写入' },
  run_command:{ icon: '⚡', color: '#d29922', bg: 'rgba(210,153,34,0.08)',   label: '执行' },
  search:     { icon: '🔍', color: '#a371f7', bg: 'rgba(163,113,247,0.08)',  label: '搜索' },
  unknown:    { icon: '🔧', color: '#888',    bg: 'rgba(136,136,136,0.08)',  label: '工具' },
};

export default function ToolCallCard({ toolCall }: ToolCallCardProps) {
  const [expanded, setExpanded] = useState(false);
  const [executing, setExecuting] = useState(false);
  const [execResult, setExecResult] = useState<string | null>(null);
  const [execError, setExecError] = useState<string | null>(null);

  const cfg = TYPE_CONFIG[toolCall.type] || TYPE_CONFIG.unknown;

  const handleExecute = useCallback(async () => {
    setExecuting(true);
    setExecResult(null);
    setExecError(null);

    const sessionId = useStore.getState().directChatTaskId || useStore.getState().selMasterTaskId;
    if (!sessionId) { setExecuting(false); return; }

    try {
      if (toolCall.type === 'write_file' && toolCall.filePath) {
        const result = await api.executeAction(sessionId, 'create_file', JSON.stringify({
          path: toolCall.filePath,
          content: toolCall.content,
        }));
        if (result?.success) {
          setExecResult(`✅ 文件已写入: ${toolCall.filePath}`);
        } else {
          setExecError(result?.error || '写入失败');
        }
      } else if (toolCall.type === 'run_command') {
        const result = await api.executeAction(sessionId, 'run_command', JSON.stringify({
          command: toolCall.content,
        }));
        if (result?.success) {
          setExecResult(result.output || '(无输出)');
          if (result.error) setExecError(result.error);
        } else {
          setExecError(result?.error || '执行失败');
        }
      }
    } catch (e: any) {
      setExecError(e?.message || '执行异常');
    }
    setExecuting(false);
  }, [toolCall]);

  const canExecute = toolCall.type === 'write_file' || toolCall.type === 'run_command';

  return (
    <div style={{
      margin: '8px 0', borderRadius: 10, overflow: 'hidden',
      border: `1px solid ${cfg.color}33`,
      background: cfg.bg,
      maxWidth: '100%',
    }}>
      {/* Header bar — always visible */}
      <div
        onClick={() => setExpanded(!expanded)}
        style={{
          display: 'flex', alignItems: 'center', gap: 10,
          padding: '8px 14px', cursor: 'pointer',
          userSelect: 'none',
          transition: 'background 0.15s',
        }}
        onMouseEnter={e => { e.currentTarget.style.background = 'rgba(255,255,255,0.03)'; }}
        onMouseLeave={e => { e.currentTarget.style.background = 'transparent'; }}
      >
        {/* Icon */}
        <span style={{ fontSize: 16, flexShrink: 0 }}>{cfg.icon}</span>

        {/* Label + detail */}
        <div style={{ flex: 1, minWidth: 0 }}>
          <div style={{ fontSize: 12, fontWeight: 600, color: cfg.color }}>
            {cfg.label}
          </div>
          <div style={{
            fontSize: 12, color: '#aaa',
            overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
            fontFamily: toolCall.filePath ? "'JetBrains Mono', 'Fira Code', Consolas, monospace" : 'inherit',
          }}>
            {toolCall.detail}
          </div>
        </div>

        {/* Actions */}
        <div style={{ display: 'flex', gap: 6, alignItems: 'center' }} onClick={e => e.stopPropagation()}>
          {canExecute && !execResult && (
            <button
              onClick={handleExecute}
              disabled={executing}
              style={{
                padding: '3px 10px', borderRadius: 5, border: `1px solid ${cfg.color}66`,
                background: executing ? 'rgba(255,255,255,0.05)' : `${cfg.color}22`,
                color: cfg.color, fontSize: 11, fontWeight: 600, cursor: 'pointer',
              }}
            >
              {executing ? '执行中…' : '执行'}
            </button>
          )}
          {/* Expand arrow */}
          <svg width="10" height="6" viewBox="0 0 10 6" style={{
            transition: 'transform 0.15s',
            transform: expanded ? 'rotate(180deg)' : 'rotate(0deg)',
            flexShrink: 0,
          }}>
            <path d="M0 0l5 6 5-6" fill="none" stroke="#888" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round"/>
          </svg>
        </div>
      </div>

      {/* Exec result / error — always visible after execution */}
      {(execResult || execError) && (
        <div style={{ padding: '0 14px 8px' }}>
          {execResult && (
            <pre style={{
              margin: 0, padding: '6px 10px', borderRadius: 6,
              background: 'rgba(0,0,0,0.25)', color: '#3fb950',
              fontSize: 11, maxHeight: 200, overflow: 'auto',
              whiteSpace: 'pre-wrap', wordBreak: 'break-all',
            }}>
              {execResult}
            </pre>
          )}
          {execError && (
            <div style={{
              marginTop: execResult ? 4 : 0, padding: '6px 10px', borderRadius: 6,
              background: 'rgba(248,81,73,0.1)', color: '#f85149',
              fontSize: 11,
            }}>
              {execError}
            </div>
          )}
        </div>
      )}

      {/* Expandable content */}
      {expanded && (
        <div style={{ borderTop: '1px solid rgba(255,255,255,0.04)' }}>
          {/* Meta info */}
          <div style={{
            padding: '6px 14px', display: 'flex', gap: 16,
            fontSize: 11, color: '#666',
          }}>
            <span>语言: {toolCall.language || 'text'}</span>
            {toolCall.filePath && <span>路径: {toolCall.filePath}</span>}
            <span>{toolCall.content.split('\n').length} 行</span>
          </div>
          {/* Code */}
          <SyntaxHighlighter
            language={toolCall.language || 'text'}
            style={oneDark}
            customStyle={{
              margin: 0,
              padding: '10px 14px',
              background: 'rgba(0,0,0,0.3)',
              fontSize: 12,
              lineHeight: 1.5,
              maxHeight: 300,
              overflow: 'auto',
            }}
          >
            {toolCall.content}
          </SyntaxHighlighter>
        </div>
      )}
    </div>
  );
}
