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
  }
} else {
  document.body.innerHTML = '<div style="color:#fff;padding:20px">FATAL: root element not found</div>';
}
