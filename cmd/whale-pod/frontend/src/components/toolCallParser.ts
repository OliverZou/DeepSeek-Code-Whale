import type { ToolCall } from '../types';

let _idSeq = 0;
function nextId(): string {
  _idSeq++;
  return `tc_${Date.now()}_${_idSeq}`;
}

/**
 * Parse a markdown string and extract tool calls from code blocks.
 * Returns:
 *  - segments: alternating text/toolcall segments in order
 *  - toolCalls: flat array of all extracted tool calls
 */
export interface ContentSegment {
  type: 'text' | 'toolcall';
  text?: string;          // for text segments
  toolCall?: ToolCall;    // for toolcall segments
}

export function parseToolCalls(markdown: string): ContentSegment[] {
  const segments: ContentSegment[] = [];
  const lines = markdown.split('\n');
  let i = 0;
  let textBuf: string[] = [];

  function flushText() {
    if (textBuf.length > 0) {
      segments.push({ type: 'text', text: textBuf.join('\n') });
      textBuf = [];
    }
  }

  while (i < lines.length) {
    const trimmed = lines[i].trim();

    // Detect fenced code block start: ```lang or ```lang:filepath
    const fenceMatch = trimmed.match(/^```(\S*)/);
    if (fenceMatch) {
      const fenceInfo = fenceMatch[1] || '';
      const j = findFenceEnd(lines, i + 1);
      if (j === -1) {
        // No closing fence — treat as regular text
        textBuf.push(lines[i]);
        i++;
        continue;
      }

      // Extract code block content
      const codeLines = lines.slice(i + 1, j);
      const codeContent = codeLines.join('\n');

      // Classify the code block
      const tc = classifyCodeBlock(fenceInfo, codeContent, i, j);
      if (tc) {
        flushText();
        segments.push({ type: 'toolcall', toolCall: tc });
        i = j + 1;
        continue;
      }

      // Not a tool call — keep as text in markdown
      textBuf.push(lines[i]);
      i++;
      continue;
    }

    textBuf.push(lines[i]);
    i++;
  }

  flushText();
  return segments;
}

function findFenceEnd(lines: string[], start: number): number {
  for (let k = start; k < lines.length; k++) {
    if (lines[k].trim() === '```') return k;
  }
  return -1;
}

function classifyCodeBlock(
  fenceInfo: string,
  codeContent: string,
  startLine: number,
  endLine: number,
): ToolCall | null {
  // Extract language and optional filepath
  let language = '';
  let filePath: string | undefined;

  const colonIdx = fenceInfo.indexOf(':');
  if (colonIdx >= 0) {
    language = fenceInfo.substring(0, colonIdx);
    filePath = fenceInfo.substring(colonIdx + 1);
  } else {
    language = fenceInfo;
    // If the "language" looks like a file path, treat it as one
    if (fenceInfo.includes('.') && !/\s/.test(fenceInfo) && !isKnownLang(fenceInfo)) {
      filePath = fenceInfo;
      language = detectLangFromPath(fenceInfo);
    }
  }

  // Command execution: bash / shell / sh / cmd / powershell / pwsh
  if (/^(bash|shell|sh|cmd|powershell|pwsh|zsh)$/i.test(language) && !filePath) {
    const firstLine = codeContent.trim().split('\n')[0].replace(/^[#$]\s*/, '');
    return {
      id: nextId(),
      type: 'run_command',
      label: `执行命令`,
      detail: firstLine.length > 60 ? firstLine.substring(0, 60) + '…' : firstLine,
      content: codeContent,
      language,
      filePath: undefined,
      startLine,
      endLine,
    };
  }

  // File write: has a filepath
  if (filePath) {
    return {
      id: nextId(),
      type: 'write_file',
      label: `写入文件`,
      detail: filePath,
      content: codeContent,
      language: language || detectLangFromPath(filePath),
      filePath,
      startLine,
      endLine,
    };
  }

  // Read file reference: language looks like a path or first line has // file: path
  const readMatch = codeContent.match(/^\s*(?:\/\/|#)\s*file:\s*(.+)/m);
  if (readMatch) {
    return {
      id: nextId(),
      type: 'read_file',
      label: `读取文件`,
      detail: readMatch[1].trim(),
      content: codeContent,
      language,
      filePath: readMatch[1].trim(),
      startLine,
      endLine,
    };
  }

  // Plain code block with language → not a tool call
  if (language && !filePath) {
    return null;
  }

  // Unknown / no language → not a tool call
  return null;
}

function isKnownLang(lang: string): boolean {
  const known = new Set([
    'go', 'rust', 'python', 'js', 'ts', 'jsx', 'tsx',
    'java', 'c', 'cpp', 'cs', 'rb', 'php', 'swift', 'kt',
    'scala', 'r', 'sql', 'graphql', 'yaml', 'yml', 'json',
    'xml', 'toml', 'ini', 'cfg', 'conf', 'makefile', 'dockerfile',
    'html', 'css', 'scss', 'less', 'vue', 'svelte',
    'bash', 'shell', 'sh', 'zsh', 'cmd', 'powershell', 'pwsh',
    'markdown', 'md', 'text', 'txt', 'diff', 'patch',
    'proto', 'hcl', 'tf',
  ]);
  return known.has(lang.toLowerCase());
}

function detectLangFromPath(path: string): string {
  const ext = path.split('.').pop()?.toLowerCase() || '';
  const map: Record<string, string> = {
    go: 'go', rs: 'rust', py: 'python', js: 'javascript', ts: 'typescript',
    jsx: 'jsx', tsx: 'tsx', java: 'java', c: 'c', cpp: 'cpp', h: 'c',
    cs: 'csharp', rb: 'ruby', php: 'php', swift: 'swift', kt: 'kotlin',
    scala: 'scala', r: 'r', sql: 'sql', graphql: 'graphql',
    yaml: 'yaml', yml: 'yaml', json: 'json', xml: 'xml', toml: 'toml',
    html: 'html', css: 'css', scss: 'scss', vue: 'vue', svelte: 'svelte',
    md: 'markdown', txt: 'text', sh: 'bash', ps1: 'powershell',
    dockerfile: 'dockerfile', proto: 'proto', tf: 'hcl',
  };
  return map[ext] || 'text';
}
