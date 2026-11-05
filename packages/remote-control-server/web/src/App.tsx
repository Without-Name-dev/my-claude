import { useState, useEffect, useCallback, lazy, Suspense } from 'react';
import { ThemeProvider } from './lib/theme';
import { haptic, withViewTransition } from './lib/view-transition';
import { getUuid, setUuid, apiBind } from './api/client';
import { DebugPanel } from './components/DebugPanel';
import { enableDebug } from './lib/debug-log';

const Workspace = lazy(() => import('./pages/Workspace').then(m => ({ default: m.Workspace })));
const BridgeChat = lazy(() => import('./components/device/BridgeChat').then(m => ({ default: m.BridgeChat })));

type WorkspaceRoute = { deviceId?: string; localSessionId?: string };

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

  useEffect(() => {
    parseRoute();
    const onPop = () => {
      haptic('light');
      withViewTransition(parseRoute);
    };
    window.addEventListener('popstate', onPop);
    return () => window.removeEventListener('popstate', onPop);
  }, [parseRoute]);

  const goWorkspace = useCallback(() => {
    haptic('light');
    withViewTransition(() => {
      window.history.pushState(null, '', '/code/');
      setBridgeSessionId(null);
      setWorkspace({});
    });
  }, []);

  const navigateSession = useCallback((deviceId: string, localSessionId?: string) => {
    haptic('light');
    const path = localSessionId
      ? `/code/d/${encodeURIComponent(deviceId)}/s/${encodeURIComponent(localSessionId)}`
      : `/code/d/${encodeURIComponent(deviceId)}`;
    withViewTransition(() => {
      window.history.pushState(null, '', path);
      setBridgeSessionId(null);
      setWorkspace(localSessionId ? { deviceId, localSessionId } : { deviceId });
    });
  }, []);

  return (
    <ThemeProvider defaultTheme="system">
      <div className="app-shell safe-top flex flex-col overflow-hidden bg-surface-0 text-text-primary">
        <Suspense
          fallback={
            <div className="flex flex-1 items-center justify-center">
              <span className="h-2 w-2 animate-pulse rounded-full bg-brand" />
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
                <div className="flex items-center gap-2 border-b border-border bg-surface-1 px-2 py-1.5">
                  <button
                    type="button"
                    onClick={goWorkspace}
                    className="flex h-9 w-9 items-center justify-center rounded-lg text-lg text-text-muted transition-colors hover:bg-surface-2 hover:text-text-secondary"
                    aria-label="返回"
                  >
                    &larr;
                  </button>
                  <span className="truncate text-sm text-text-secondary">远程会话</span>
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
        <DebugPanel />
      </div>
    </ThemeProvider>
  );
}
