import React from 'react';
import ReactDOM from 'react-dom/client';
import App from './App';
import './styles/global.css';

const rootEl = document.getElementById('root');
if (rootEl) {
  ReactDOM.createRoot(rootEl).render(
    <React.StrictMode><App /></React.StrictMode>
  );

  // Wails events
  const runtime = (window as any).runtime;
  if (runtime?.EventsOn) {
    runtime.EventsOn('task-event', (evt: any) => {
      import('./store').then(({ useStore }) => {
        useStore.getState().handleTaskEvent(evt);
      }).catch(() => {});
    });
    runtime.EventsOn('update', () => {
      import('./store').then(({ useStore }) => {
        useStore.getState().loadMasterTasks();
      }).catch(() => {});
    });
  }
  // HMR: reload data after hot module replacement
  if (typeof (window as any).__VITE_HMR__ !== 'undefined') {
    (window as any).__WHALE_HMR_RELOAD__ = () => {
      import('./store').then(({ useStore }) => {
        useStore.getState().loadMasterTasks();
      }).catch(() => {});
    };
  }
} else {
  document.body.innerHTML = '<div style="color:#fff;padding:20px">FATAL: root element not found</div>';
}
