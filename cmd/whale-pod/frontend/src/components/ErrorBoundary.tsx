import React from 'react';

interface Props {
  children: React.ReactNode;
}

interface State {
  hasError: boolean;
  error: string;
}

export default class ErrorBoundary extends React.Component<Props, State> {
  constructor(props: Props) {
    super(props);
    this.state = { hasError: false, error: '' };
  }

  static getDerivedStateFromError(error: Error): State {
    return { hasError: true, error: error.message || String(error) };
  }

  componentDidCatch(error: Error) {
    console.error('ErrorBoundary caught:', error);
  }

  render() {
    if (this.state.hasError) {
      return (
        <div style={{
          display: 'flex', flexDirection: 'column',
          alignItems: 'center', justifyContent: 'center',
          height: '100vh', background: '#0f1117', color: '#e0e0e0',
          padding: 40, textAlign: 'center',
        }}>
          <div style={{ fontSize: 48, marginBottom: 16 }}>😵</div>
          <h2 style={{ marginBottom: 8, fontWeight: 600 }}>出了点问题</h2>
          <p style={{ color: '#888', fontSize: 13, maxWidth: 400, marginBottom: 20 }}>
            应用遇到了一个意外错误。请尝试刷新页面。
          </p>
          <pre style={{
            padding: '10px 16px', borderRadius: 8,
            background: '#1e1e1e', color: '#f85149',
            fontSize: 12, maxWidth: 500, overflow: 'auto',
            marginBottom: 20,
          }}>
            {this.state.error}
          </pre>
          <button
            onClick={() => {
              this.setState({ hasError: false, error: '' });
              window.location.reload();
            }}
            style={{
              padding: '8px 24px', borderRadius: 8, border: 'none',
              background: '#4CAF50', color: '#fff', fontSize: 14,
              fontWeight: 600, cursor: 'pointer',
            }}
          >
            刷新页面
          </button>
        </div>
      );
    }

    return this.props.children;
  }
}
