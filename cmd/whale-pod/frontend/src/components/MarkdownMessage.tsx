import { useState, useCallback, memo } from 'react';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { Prism as SyntaxHighlighter } from 'react-syntax-highlighter';
import { oneDark } from 'react-syntax-highlighter/dist/esm/styles/prism';
import { api } from '../wails';
import { useStore } from '../store';

interface CodeBlockProps {
  language: string;
  value: string;
}

// extractFilePath tries to find a file path from the language tag or first line.
function extractFilePath(language: string, value: string): string | null {
  // Pattern: language:path (e.g. "go:cmd/main.go" or "typescript:src/app.tsx")
  if (language.includes(':') && language.includes('.')) {
    return language;
  }
  // Pattern: path as language with extension (e.g. ```main.go)
  if (language && language.includes('.') && !language.includes(' ')) {
    return language;
  }
  // Pattern: first line is "// file: path/to/file"
  const firstLine = value.split('\n')[0];
  const fileMatch = firstLine.match(/(?:\/\/|#)\s*file:\s*(.+)/i);
  if (fileMatch) return fileMatch[1].trim();
  // Pattern: "// path/to/file.ext" alone on first line
  const pathMatch = firstLine.match(/^\/\/\s*([\w.\-/\\]+\.\w{1,8})\s*$/);
  if (pathMatch) return pathMatch[1].trim();
  return null;
}

function CodeBlock({ language, value }: CodeBlockProps) {
  const [copied, setCopied] = useState(false);
  const [applying, setApplying] = useState(false);
  const [applied, setApplied] = useState(false);

  const filePath = extractFilePath(language, value);
  const displayLang = filePath && filePath.includes(':') ? filePath.split(':')[0] : (language || 'code');

  const handleCopy = useCallback(() => {
    navigator.clipboard.writeText(value).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    }).catch(() => {});
  }, [value]);

  const handleApply = async () => {
    if (!filePath) return;
    setApplying(true);
    const sessionId = useStore.getState().directChatTaskId || useStore.getState().selMasterTaskId;
    if (!sessionId) { setApplying(false); return; }
    const path = filePath.includes(':') ? filePath.split(':').slice(1).join(':') : filePath;
    const result = await api.executeAction(sessionId, 'create_file', JSON.stringify({ path, content: value }));
    if (result?.success) {
      setApplied(true);
      setTimeout(() => setApplied(false), 3000);
    }
    setApplying(false);
  };

  return (
    <div style={{ position: 'relative', margin: '8px 0', borderRadius: 8, overflow: 'hidden' }}>
      {/* header bar */}
      <div style={{
        display: 'flex', alignItems: 'center', justifyContent: 'space-between',
        padding: '4px 12px', background: 'rgba(255,255,255,0.06)',
        borderBottom: '1px solid rgba(255,255,255,0.06)',
      }}>
        <span style={{ fontSize: 11, color: '#aaa' }}>
          <span style={{ color: '#888' }}>{displayLang}</span>
          {filePath && (
            <span style={{ color: '#58a6ff', marginLeft: 4 }}>
              {filePath.includes(':') ? filePath.split(':').slice(1).join(':') : filePath}
            </span>
          )}
        </span>
        <div style={{ display: 'flex', gap: 4 }}>
          {filePath && (
            <button
              onClick={handleApply}
              disabled={applying}
              style={{
                padding: '2px 8px', borderRadius: 4, border: 'none',
                background: applied ? 'rgba(76,175,80,0.2)' : 'rgba(88,166,255,0.12)',
                color: applied ? '#4CAF50' : '#58a6ff',
                fontSize: 11, cursor: 'pointer',
                transition: 'all 0.15s',
              }}
            >
              {applied ? '已应用 ✓' : applying ? '应用中…' : '应用'}
            </button>
          )}
          <button
            onClick={handleCopy}
            style={{
              padding: '2px 8px', borderRadius: 4, border: 'none',
              background: copied ? 'rgba(76,175,80,0.2)' : 'rgba(255,255,255,0.06)',
              color: copied ? '#4CAF50' : '#888',
              fontSize: 11, cursor: 'pointer',
              transition: 'all 0.15s',
            }}
          >
            {copied ? '已复制 ✓' : '复制'}
          </button>
        </div>
      </div>
      <SyntaxHighlighter
        language={language || 'text'}
        style={oneDark}
        customStyle={{
          margin: 0,
          padding: '12px 14px',
          background: 'rgba(0,0,0,0.35)',
          fontSize: 13,
          lineHeight: 1.5,
          borderRadius: '0 0 8px 8px',
        }}
        codeTagProps={{
          style: { background: 'transparent', fontFamily: "'JetBrains Mono', 'Fira Code', 'Cascadia Code', Consolas, monospace" }
        }}
      >
        {value}
      </SyntaxHighlighter>
    </div>
  );
}

interface MarkdownMessageProps {
  content: string;
}

export default memo(function MarkdownMessage({ content }: MarkdownMessageProps) {
  return (
    <ReactMarkdown
      remarkPlugins={[remarkGfm]}
      components={{
        code({ node, className, children, ...props }) {
          const match = /language-(\w+)/.exec(className || '');
          const isInline = !match && !String(children).includes('\n');

          if (isInline) {
            return (
              <code
                className={className}
                style={{
                  background: 'rgba(0,0,0,0.3)',
                  padding: '2px 6px',
                  borderRadius: 4,
                  fontSize: 13,
                }}
                {...props}
              >
                {children}
              </code>
            );
          }

          return (
            <CodeBlock
              language={match ? match[1] : ''}
              value={String(children).replace(/\n$/, '')}
            />
          );
        },
        // External links open in system browser
        a({ href, children, ...props }) {
          return (
            <a
              href={href}
              target="_blank"
              rel="noopener noreferrer"
              style={{ color: '#58a6ff' }}
              {...props}
            >
              {children}
            </a>
          );
        },
        // Style tables
        table({ children }) {
          return (
            <div style={{ overflowX: 'auto', margin: '8px 0' }}>
              <table style={{
                borderCollapse: 'collapse',
                width: '100%',
              }}>
                {children}
              </table>
            </div>
          );
        },
        th({ children }) {
          return (
            <th style={{
              border: '1px solid #444',
              padding: '6px 12px',
              fontSize: 13,
              fontWeight: 600,
              background: 'rgba(255,255,255,0.04)',
              textAlign: 'left',
            }}>
              {children}
            </th>
          );
        },
        td({ children }) {
          return (
            <td style={{
              border: '1px solid #444',
              padding: '6px 12px',
              fontSize: 13,
            }}>
              {children}
            </td>
          );
        },
        blockquote({ children }) {
          return (
            <blockquote style={{
              borderLeft: '3px solid #4CAF50',
              paddingLeft: 12,
              margin: '8px 0',
              opacity: 0.85,
            }}>
              {children}
            </blockquote>
          );
        },
        // Style images
        img({ src, alt }) {
          return (
            <img
              src={src}
              alt={alt}
              style={{
                maxWidth: '100%',
                borderRadius: 8,
                margin: '8px 0',
              }}
              loading="lazy"
            />
          );
        },
        // Style paragraphs
        p({ children }) {
          return <p style={{ margin: '0 0 8px' }}>{children}</p>;
        },
      }}
    >
      {content}
    </ReactMarkdown>
  );
});
