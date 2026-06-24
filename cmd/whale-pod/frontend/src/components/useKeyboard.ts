import { useEffect } from 'react';
import { useStore } from '../store';

interface UseKeyboardProps {
  onNewChat?: () => void;
  onToggleSettings?: () => void;
  onCloseTab?: () => void;
  onNextTab?: () => void;
  onPrevTab?: () => void;
}

export function useKeyboard({
  onNewChat,
  onToggleSettings,
  onCloseTab,
  onNextTab,
  onPrevTab,
}: UseKeyboardProps) {
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      const mod = e.ctrlKey || e.metaKey;

      // Ctrl+N: 新建对话
      if (mod && e.key === 'n' && !e.shiftKey) {
        e.preventDefault();
        onNewChat?.();
        return;
      }

      // Ctrl+Shift+N: 新建任务
      if (mod && e.shiftKey && e.key === 'N') {
        e.preventDefault();
        useStore.setState({ activeFunction: 'create' });
        return;
      }

      // Ctrl+,: 打开设置
      if (mod && e.key === ',') {
        e.preventDefault();
        onToggleSettings?.();
        return;
      }

      // Ctrl+W: 关闭当前标签
      if (mod && e.key === 'w') {
        e.preventDefault();
        onCloseTab?.();
        return;
      }

      // Ctrl+Tab: 下一个标签
      if (mod && e.key === 'Tab' && !e.shiftKey) {
        e.preventDefault();
        onNextTab?.();
        return;
      }

      // Ctrl+Shift+Tab: 上一个标签
      if (mod && e.shiftKey && e.key === 'Tab') {
        e.preventDefault();
        onPrevTab?.();
        return;
      }

      // Escape: 关闭面板
      if (e.key === 'Escape') {
        const state = useStore.getState();
        if (state.activeFunction) {
          useStore.setState({ activeFunction: null });
        }
        onToggleSettings?.(); // close settings if open
      }
    };

    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [onNewChat, onToggleSettings, onCloseTab, onNextTab, onPrevTab]);
}
