import { Component, useState, useEffect, useCallback, lazy, Suspense, type ReactNode } from 'react';
import { ChevronLeft } from 'lucide-react';
import { ThemeProvider } from './lib/theme';
import { haptic } from './lib/view-transition';
import { getUuid, setUuid, apiBind, setAccessKey } from './api/client';
import { DebugPanel } from './components/DebugPanel';
import { UpdatePrompt } from './components/UpdatePrompt';
import { enableDebug } from './lib/debug-log';
import { clearTurnNotice } from './lib/turn-notice';

const Workspace = lazy(() => import('./pages/Workspace').then(m => ({ default: m.Workspace })));
const BridgeChat = lazy(() => import('./components/device/BridgeChat').then(m => ({ default: m.BridgeChat })));

type WorkspaceRoute = { deviceId?: string; localSessionId?: string };

/**
 * The server answered 401 — it has an access key configured and this client
 * has not presented it. One input, saved to localStorage + cookie, reload.
 */
function AccessGate() {
  const [open, setOpen] = useState(false);
  const [value, setValue] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    const onUnauthorized = () => setOpen(true);
    window.addEventListener('rcs:unauthorized', onUnauthorized);
    return () => window.removeEventListener('rcs:unauthorized', onUnauthorized);
  }, []);
  if (!open) return null;
  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-label="访问密钥"
      className="fixed inset-0 z-[130] flex items-center justify-center bg-black/50 p-6"
    >
      <form
        className="w-full max-w-sm rounded-2xl border border-border bg-surface-1 p-5 shadow-xl"
        onSubmit={async event => {
          event.preventDefault();
          const key = value.trim();
          if (!key || busy) return;
          setBusy(true);
          setError(null);
          setAccessKey(key);
          // Verify before reloading: a silent reload on a wrong key just showed
          // this dialog again with no explanation, which read as "login broken".
          try {
            const res = await fetch('/web/devices', {
              headers: { Authorization: `Bearer ${key}` },
            });
            if (res.status === 401) {
              setBusy(false);
              setError('密钥不正确，请重新输入');
              return;
            }
            if (!res.ok) {
              setBusy(false);
              setError(`服务器错误（${res.status}），请稍后重试`);
              return;
            }
            window.location.reload();
          } catch {
            setBusy(false);
            setError('网络异常，请检查连接后重试');
          }
        }}
      >
        <p className="text-[15px] font-medium text-text-primary">需要访问密钥</p>
        <p className="mt-1 text-[13px] leading-relaxed text-text-muted">
          这台服务器开启了访问保护。输入服务器上配置的 RCS_WEB_ACCESS_KEY。
        </p>
        <input
          type="password"
          value={value}
          onChange={event => {
            setValue(event.target.value);
            setError(null);
          }}
          placeholder="访问密钥"
          aria-label="访问密钥"
          autoFocus
          className="mt-3 h-11 w-full rounded-xl border border-border bg-surface-2 px-3 text-base text-text-primary outline-none focus:border-brand/50"
        />
        {error && (
          <p role="alert" className="mt-2 text-[12px] leading-relaxed text-destructive">
            {error}
          </p>
        )}
        <button
          type="submit"
          disabled={!value.trim() || busy}
          className="mt-3 h-11 w-full rounded-xl bg-brand text-sm font-medium text-white active:scale-[0.98] disabled:opacity-50"
        >
          {busy ? '验证中…' : '保存并重试'}
        </button>
      </form>
    </div>
  );
}

/**
 * A render error in a WKWebView is an unexplained white page: there is no
 * console to open and no reload gesture. Show what happened and offer the one
 * action that actually helps.
 */
class ErrorBoundary extends Component<{ children: ReactNode }, { error: Error | null }> {
  state: { error: Error | null } = { error: null };

  static getDerivedStateFromError(error: Error) {
    return { error };
  }

  componentDidCatch(error: Error, info: unknown) {
    console.error('[rcs] render error', error, info);
  }

  render() {
    if (!this.state.error) return this.props.children;
    return (
      <div role="alert" className="flex flex-1 flex-col items-center justify-center gap-3 px-6 text-center">
        <p className="text-[15px] font-medium text-text-primary">页面出错了</p>
        <p className="max-w-xs break-words text-[13px] leading-relaxed text-text-muted">
          {this.state.error.message || '未知错误'}
        </p>
        <button
          type="button"
          onClick={() => window.location.reload()}
          className="mt-1 rounded-full bg-brand px-4 py-2 text-sm font-medium text-white active:scale-95"
        >
          重新加载
        </button>
      </div>
    );
  }
}

/**
 * Signals the shell to fade the boot splash. It lives inside the Suspense
 * boundary, so it only commits once the lazy workspace/chat chunk is ready —
 * the splash never fades onto an empty screen.
 */
function ReadySignal() {
  useEffect(() => {
    window.dispatchEvent(new Event('ccremote:ready'));
  }, []);
  return null;
}

/**
 * Thin router. The web client is intentionally minimal — the device/session
 * APIs are the contract shared with the future iOS client, and this UI only
 * renders them:
 *
 *   /code/                          device → project → session workspace
 *   /code/d/<deviceId>/s/<localId>  stable device-session deep link
 *   /code/<sessionId>               bridge deep link (printed by the CLI)
 */
export default function App() {
  const [workspace, setWorkspace] = useState<WorkspaceRoute | null>(null);
  const [bridgeSessionId, setBridgeSessionId] = useState<string | null>(null);
  const [routeReady, setRouteReady] = useState(false);

  const parseRoute = useCallback(() => {
    // Ensure a stable identity exists for session ownership.
    getUuid();

    const params = new URLSearchParams(window.location.search);

    // Navigation drops the query string; persist debug mode for the session
    // so the log button does not vanish after the first route change.
    if (params.get('debug') === '1') enableDebug();

    const importUuid = params.get('uuid');
    if (importUuid) {
      setUuid(importUuid);
      const url = new URL(window.location.href);
      url.searchParams.delete('uuid');
      window.history.replaceState(null, '', url);
    }

    // CLI session bind (?sid=xxx): bind to this identity, then chat.
    const sid = params.get('sid');
    if (sid) {
      const url = new URL(window.location.href);
      url.searchParams.delete('sid');
      window.history.replaceState(null, '', `/code/${sid}`);
      setWorkspace(null);
      setBridgeSessionId(sid);
      void apiBind(sid).catch(() => {});
      setRouteReady(true);
      return;
    }

    const path = window.location.pathname;
    const deviceSession = path.match(/^\/code\/d\/([^/]+)\/s\/([^/]+)\/?$/);
    const deviceOnly = path.match(/^\/code\/d\/([^/]+)\/?$/);

    if (deviceSession) {
      setBridgeSessionId(null);
      setWorkspace({
        deviceId: decodeURIComponent(deviceSession[1]!),
        localSessionId: decodeURIComponent(deviceSession[2]!),
      });
    } else if (deviceOnly) {
      setBridgeSessionId(null);
      setWorkspace({ deviceId: decodeURIComponent(deviceOnly[1]!) });
    } else {
      // /code and /code/ are the workspace home; any other single-segment
      // path is a session id from a CLI-printed link.
      const legacy = path.match(/^\/code\/([^/]+)\/?$/);
      if (legacy) {
        setWorkspace(null);
        setBridgeSessionId(legacy[1]!);
      } else {
        setBridgeSessionId(null);
        setWorkspace({});
      }
    }
    setRouteReady(true);
  }, []);

  // iOS gives the keyboard its own visual viewport; CSS 100dvh alone does not
  // shrink in a WKWebView, which leaves the composer under the keyboard.
  useEffect(() => {
    const viewport = window.visualViewport;
    if (!viewport) return;
    let lastHeight = 0;
    // Only react to real height changes (the keyboard). Listening to scroll and
    // forcing scrollTo(0,0) fights the user's gesture and makes the page shake.
    const sync = () => {
      const height = Math.round(viewport.height);
      if (height === lastHeight) return;
      lastHeight = height;
      document.documentElement.style.setProperty('--app-height', `${height}px`);
      // Keyboard open? Then the home-indicator inset must not be reserved
      // above the keys, and the composer can sit flush with the keyboard.
      const layoutHeight = window.innerHeight;
      document.documentElement.classList.toggle('kb-open', layoutHeight - height > 120);
      // Before this resize lands iOS scrolls the document up to reveal the
      // focused composer. The shell now fits the keyboard, so there is nothing
      // left to scroll — reset once. Only height changes run this; doing it on
      // every scroll event is what used to make the page shake.
      const reset = () => {
        window.scrollTo(0, 0);
        document.documentElement.scrollTop = 0;
        document.body.scrollTop = 0;
      };
      reset();
      requestAnimationFrame(reset);
    };
    sync();
    viewport.addEventListener('resize', sync);
    return () => viewport.removeEventListener('resize', sync);
  }, []);

  // The turn-finished marker lives in the title while the page is hidden.
  useEffect(() => {
    const onVisible = () => {
      if (!document.hidden) clearTurnNotice();
    };
    document.addEventListener('visibilitychange', onVisible);
    return () => document.removeEventListener('visibilitychange', onVisible);
  }, []);

  useEffect(() => {
    parseRoute();
    const onPop = () => {
      haptic('light');
      // Back/forward already animate: iOS plays its own interactive swipe, and
      // a View Transition on top of it flashes the snapshot mid-gesture.
      parseRoute();
    };
    window.addEventListener('popstate', onPop);
    return () => window.removeEventListener('popstate', onPop);
  }, [parseRoute]);

  const goWorkspace = useCallback(() => {
    haptic('light');
    window.history.pushState(null, '', '/code/');
    setBridgeSessionId(null);
    setWorkspace({});
  }, []);

  const navigateSession = useCallback((deviceId: string, localSessionId?: string) => {
    haptic('light');
    const path = localSessionId
      ? `/code/d/${encodeURIComponent(deviceId)}/s/${encodeURIComponent(localSessionId)}`
      : `/code/d/${encodeURIComponent(deviceId)}`;
    window.history.pushState(null, '', path);
    setBridgeSessionId(null);
    setWorkspace(localSessionId ? { deviceId, localSessionId } : { deviceId });
  }, []);

  return (
    <ThemeProvider defaultTheme="system">
      <div className="app-shell safe-top safe-x flex flex-col overflow-hidden bg-surface-0 text-text-primary">
        <ErrorBoundary>
          <Suspense
            fallback={
              <div role="status" aria-live="polite" className="flex flex-1 items-center justify-center">
                <span className="h-2 w-2 animate-pulse rounded-full bg-brand" aria-hidden="true" />
                <span className="sr-only">正在加载…</span>
              </div>
            }
          >
            {routeReady ? <ReadySignal /> : null}
            {routeReady && bridgeSessionId ? (
              <BridgeChat
                key={bridgeSessionId}
                sessionId={bridgeSessionId}
                onBack={goWorkspace}
                header={
                  <div className="flex h-12 items-center gap-0.5 border-b border-border/70 bg-surface-0 px-1.5">
                    <button
                      type="button"
                      onClick={goWorkspace}
                      className="flex h-11 w-11 shrink-0 items-center justify-center rounded-full text-text-secondary transition-colors active:bg-surface-1"
                      aria-label="返回"
                    >
                      <ChevronLeft className="h-5 w-5" />
                    </button>
                    <h2 className="truncate text-[15px] font-medium text-text-primary">远程会话</h2>
                  </div>
                }
              />
            ) : routeReady && workspace ? (
              <Workspace
                initialDeviceId={workspace.deviceId}
                initialLocalSessionId={workspace.localSessionId}
                onNavigate={navigateSession}
              />
            ) : null}
          </Suspense>
        </ErrorBoundary>
        <UpdatePrompt />
        <AccessGate />
        <DebugPanel />
      </div>
    </ThemeProvider>
  );
}
