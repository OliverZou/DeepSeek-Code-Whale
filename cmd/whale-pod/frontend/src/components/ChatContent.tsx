import MarkdownMessage from './MarkdownMessage';
import ToolCallCard from './ToolCallCard';
import { parseToolCalls } from './toolCallParser';

interface ChatContentProps {
  content: string;
}

/**
 * Renders a chat message by splitting it into markdown-text segments
 * and tool-call card segments. Code blocks that look like file writes,
 * command executions, or file reads are rendered as interactive cards;
 * everything else goes through standard markdown rendering.
 */
export default function ChatContent({ content }: ChatContentProps) {
  const segments = parseToolCalls(content);

  // If no tool calls found, fall back to pure markdown
  const hasToolCalls = segments.some(s => s.type === 'toolcall');
  if (!hasToolCalls) {
    return <MarkdownMessage content={content} />;
  }

  return (
    <div>
      {segments.map((seg, i) => {
        if (seg.type === 'toolcall' && seg.toolCall) {
          return <ToolCallCard key={seg.toolCall.id} toolCall={seg.toolCall} />;
        }
        if (seg.type === 'text' && seg.text) {
          return <MarkdownMessage key={`t${i}`} content={seg.text} />;
        }
        return null;
      })}
    </div>
  );
}
