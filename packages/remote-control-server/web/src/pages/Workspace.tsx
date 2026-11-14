import { lazy, Suspense, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { ChevronDown, ChevronRight, Folder, Plus, RefreshCw, Search, Sparkles } from 'lucide-react';
import { cn } from '../lib/utils';
import { formatRelativeTime } from '../lib/device-history';
import { mergeSessionCatalog } from '../lib/session-catalog';
import {
  fetchDevices,
  fetchOperation,
  fetchProjects,
  fetchSessions,
  fetchTerminals,
  openNewSession,
  type DeviceInfo,
  type DeviceProject,
  type DeviceSessionSummary,
} from '../api/devices';
import { DirectoryPicker } from '../components/device/DirectoryPicker';
import { DebugButton } from '../components/DebugPanel';
import { ThemeToggle } from '../components/ThemeToggle';

const DeviceSessionChat = lazy(() =>
  import('../components/device/DeviceSessionChat').then(module => ({ default: module.DeviceSessionChat })),
);

/**
 * Chat-first workspace: devices → projects → local sessions on the left,
 * one conversation on the right. Selecting a session does not start a
 * terminal; the first message does.
 */
export function Workspace({
  initialDeviceId,
  initialLocalSessionId,
  onNavigate,
}: {
  initialDeviceId?: string;
  initialLocalSessionId?: string;
  onNavigate: (deviceId: string, localSessionId?: string) => void;
}) {
  const [devices, setDevices] = useState<DeviceInfo[]>([]);
  const [devicesError, setDevicesError] = useState('');
  const [loadingDevices, setLoadingDevices] = useState(true);
  const [refreshingDevices, setRefreshingDevices] = useState(false);
  const [activeDeviceId, setActiveDeviceId] = useState(initialDeviceId || '');
  const [projects, setProjects] = useState<DeviceProject[]>([]);
  const [projectsLoading, setProjectsLoading] = useState(true);
  const [projectsError, setProjectsError] = useState('');
  const [catalogNonce, setCatalogNonce] = useState(0);
  const refreshCatalog = useCallback(() => setCatalogNonce(n => n + 1), []);
  const [expandedProjectId, setExpandedProjectId] = useState<string | null>(null);
  const [collapsedSearchProjects, setCollapsedSearchProjects] = useState<Set<string>>(() => new Set());
  // Load every session for the device once and filter locally: expanding a
  // project used to trigger a multi-second disk scan on the PC.
  const [allSessions, setAllSessions] = useState<DeviceSessionSummary[]>([]);
  const [sessionsError, setSessionsError] = useState('');
  const [sessionsLoading, setSessionsLoading] = useState(false);
  // The device answers one page at a time; older sessions used to be dropped
  // silently once a machine had more than the first page.
  const [sessionsHasMore, setSessionsHasMore] = useState(false);
  const [loadingMoreSessions, setLoadingMoreSessions] = useState(false);
  // A page requested for one device can finish after the user switches to
  // another. Keep it from being merged into the newly selected device.
  const paginationDevice = useRef(activeDeviceId);
  const paginationGeneration = useRef(0);
  if (paginationDevice.current !== activeDeviceId) {
    paginationDevice.current = activeDeviceId;
    paginationGeneration.current += 1;
  }
  useEffect(() => setLoadingMoreSessions(false), [activeDeviceId]);
  const [selectedLocalSessionId, setSelectedLocalSessionId] = useState(initialLocalSessionId || '');
  const [mobileChat, setMobileChat] = useState(Boolean(initialLocalSessionId));
  const [search, setSearch] = useState('');

  // The list is display:none while a chat is open on a phone, which also
  // throws away its scroll position. The position must be captured while the
  // list is still laid out — BEFORE the state change hides it — because a
  // hidden element always reports scrollTop 0 (and reading it in a layout
  // effect after the commit is exactly that mistake).
  const listScrollRef = useRef<HTMLDivElement | null>(null);
  const savedListScroll = useRef(0);
  const captureListScroll = useCallback(() => {
    const el = listScrollRef.current;
    // offsetHeight is 0 while display:none; never overwrite a real save with
    // a hidden read.
    if (el && el.offsetHeight > 0) savedListScroll.current = el.scrollTop;
  }, []);

  // Route changes (the iOS edge swipe, browser back) must land in the same
  // commit. Doing this in an effect painted the chat for one frame after the
  // gesture had already slid to the list, which read as a flash.
  const routeSessionId = initialLocalSessionId || '';
  const lastRouteSessionId = useRef(routeSessionId);
  if (lastRouteSessionId.current !== routeSessionId) {
    // Entering a chat via forward navigation: the list is still laid out in
    // this pre-commit render, so its scrollTop is live.
    if (!lastRouteSessionId.current && routeSessionId) captureListScroll();
    lastRouteSessionId.current = routeSessionId;
    setSelectedLocalSessionId(routeSessionId);
    setMobileChat(Boolean(routeSessionId));
  }

  // Panel-level slide instead of a full-page snapshot: the chat slides in over
  // the list, and returning fades the list in. Snapshot transitions flashed
  // white inside the WKWebView.
  const [navDir, setNavDir] = useState<'push' | 'pop' | null>(null);
  const playNav = useCallback((dir: 'push' | 'pop') => setNavDir(dir), []);
  useEffect(() => {
    if (!navDir) return;
    const timer = window.setTimeout(() => setNavDir(null), 340);
    return () => window.clearTimeout(timer);
  }, [navDir]);

  const prevMobileChat = useRef(mobileChat);
  useLayoutEffect(() => {
    if (prevMobileChat.current === mobileChat) return;
    prevMobileChat.current = mobileChat;
    if (!mobileChat) {
      // Chromium preserves scrollTop across display:none and gets the same
      // value written back; WebKit resets to 0 and needs the restore.
      const restore = savedListScroll.current;
      requestAnimationFrame(() => {
        if (listScrollRef.current) listScrollRef.current.scrollTop = restore;
      });
    }
  }, [mobileChat]);

  const refreshDevices = useCallback(
    async (manual = false) => {
      if (manual) setRefreshingDevices(true);
      try {
        const result = await fetchDevices();
        setDevices(result.devices);
        setDevicesError('');
        setActiveDeviceId(current => {
          if (current && result.devices.some(device => device.id === current)) {
            return current;
          }
          const preferred = initialDeviceId ? result.devices.find(device => device.id === initialDeviceId) : undefined;
          return preferred?.id || result.devices.find(device => device.online)?.id || result.devices[0]?.id || '';
        });
      } catch (error) {
        setDevicesError(error instanceof Error ? error.message : '无法读取设备列表');
      } finally {
        setLoadingDevices(false);
        if (manual) setRefreshingDevices(false);
      }
    },
    [initialDeviceId],
  );

  useEffect(() => {
    void refreshDevices();
    // Polling while the app is in the background only burns battery; catch up
    // on the first frame after it comes back.
    const timer = setInterval(() => {
      if (!document.hidden) void refreshDevices();
    }, 10_000);
    const onVisible = () => {
      if (!document.hidden) void refreshDevices();
    };
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      clearInterval(timer);
      document.removeEventListener('visibilitychange', onVisible);
    };
  }, [refreshDevices]);

  // The URL (via these props) is authoritative: back/forward and the header
  // home button must move the visible selection with it.
  useEffect(() => {
    if (!initialDeviceId) {
      // The URL points at the workspace home: do not keep a stale selection.
      setSelectedLocalSessionId('');
      setMobileChat(false);
      return;
    }
    if (devices.length === 0) return;
    if (!devices.some(device => device.id === initialDeviceId)) {
      setSelectedLocalSessionId('');
      setMobileChat(false);
      return;
    }
    setActiveDeviceId(initialDeviceId);
    setSelectedLocalSessionId(initialLocalSessionId || '');
    setMobileChat(Boolean(initialLocalSessionId));
  }, [initialDeviceId, initialLocalSessionId, devices]);

  // A catalog refresh (retry, new-session ready, header refresh) must not
  // wipe what the user is looking at: only a real device switch resets the
  // expansion, the loaded pages and the scroll.
  const catalogDeviceRef = useRef('');
  useEffect(() => {
    if (!activeDeviceId) {
      catalogDeviceRef.current = '';
      setProjects([]);
      setAllSessions([]);
      setProjectsLoading(false);
      return;
    }
    const deviceChanged = catalogDeviceRef.current !== activeDeviceId;
    catalogDeviceRef.current = activeDeviceId;
    let cancelled = false;
    setProjectsError('');
    setSessionsError('');
    setProjectsLoading(true);
    if (deviceChanged) {
      setExpandedProjectId(null);
      setAllSessions([]);
      setSessionsHasMore(false);
    }
    setSessionsLoading(true);
    // Let each request update the sidebar as soon as it completes. Waiting
    // for both makes a slow session scan hide the already-loaded projects.
    void fetchProjects(activeDeviceId)
      .then(result => {
        if (!cancelled) setProjects(result.projects);
      })
      .catch(error => {
        if (cancelled) return;
        setProjects([]);
        setProjectsError(error instanceof Error ? error.message : '无法读取项目');
      })
      .finally(() => {
        if (!cancelled) setProjectsLoading(false);
      });
    void fetchSessions(activeDeviceId, undefined, 500)
      .then(result => {
        if (cancelled) return;
        const fresh = result.sessions;
        if (deviceChanged) {
          setAllSessions(fresh);
        } else {
          // Keep any older pages the user already loaded; the refresh only
          // describes the first 500.
          setAllSessions(previous => mergeSessionCatalog(previous, fresh, Boolean(result.hasMore)));
        }
        setSessionsHasMore(Boolean(result.hasMore));
      })
      .catch(error => {
        if (!cancelled) setSessionsError(error instanceof Error ? error.message : '无法读取会话');
      })
      .finally(() => {
        if (!cancelled) setSessionsLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [activeDeviceId, catalogNonce]);

  // Terminal presence per session. This endpoint only reads the server's
  // in-memory bridge state, so polling it never touches the PC — unlike the
  // session catalog, which is a disk scan and stays on demand.
  const [liveTerminals, setLiveTerminals] = useState<Record<string, boolean>>({});
  useEffect(() => {
    if (!activeDeviceId) {
      setLiveTerminals({});
      return;
    }
    let cancelled = false;
    const load = async () => {
      if (document.hidden) return;
      try {
        const result = await fetchTerminals(activeDeviceId);
        if (cancelled) return;
        const next: Record<string, boolean> = {};
        for (const terminal of result.terminals) {
          if (terminal.online) next[terminal.localSessionId] = true;
        }
        setLiveTerminals(next);
      } catch {
        // Keep the last picture; the next tick retries.
      }
    };
    void load();
    const timer = window.setInterval(() => void load(), 10_000);
    const onVisible = () => {
      if (!document.hidden) void load();
    };
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
      document.removeEventListener('visibilitychange', onVisible);
    };
  }, [activeDeviceId]);

  const sessionsByProject = useMemo(() => {
    const grouped: Record<string, DeviceSessionSummary[]> = {};
    for (const session of allSessions) {
      (grouped[session.projectId] ??= []).push(session);
    }
    for (const list of Object.values(grouped)) {
      // Sessions with a live terminal float to the top of their project; the
      // rest stay newest-first.
      list.sort((a, b) => {
        const liveDelta =
          Number(Boolean(liveTerminals[b.localSessionId])) - Number(Boolean(liveTerminals[a.localSessionId]));
        if (liveDelta !== 0) return liveDelta;
        return b.updatedAt - a.updatedAt;
      });
    }
    return grouped;
  }, [allSessions, liveTerminals]);

  // A direct link or browser reload should reveal the selected conversation
  // in the desktop sidebar. Do this once per selection so a manual collapse
  // is not undone by subsequent catalog refreshes.
  const revealedSelection = useRef('');
  useEffect(() => {
    if (!activeDeviceId || !selectedLocalSessionId) {
      revealedSelection.current = '';
      return;
    }
    const key = `${activeDeviceId}:${selectedLocalSessionId}`;
    if (revealedSelection.current === key) return;
    const selected = allSessions.find(session => session.localSessionId === selectedLocalSessionId);
    if (!selected) return;
    revealedSelection.current = key;
    setExpandedProjectId(selected.projectId);
  }, [activeDeviceId, allSessions, selectedLocalSessionId]);

  const toggleProject = useCallback(
    (project: DeviceProject) => {
      if (search.trim()) {
        setCollapsedSearchProjects(current => {
          const next = new Set(current);
          if (next.has(project.projectId)) next.delete(project.projectId);
          else next.add(project.projectId);
          return next;
        });
      } else {
        setExpandedProjectId(current => (current === project.projectId ? null : project.projectId));
      }
      setSessionsError('');
    },
    [search],
  );

  useEffect(() => {
    setCollapsedSearchProjects(new Set());
  }, [search, activeDeviceId]);

  // Fetch the next page of the session index. Sessions are merged by id so a
  // refresh that raced with this call cannot produce duplicates.
  const loadMoreSessions = useCallback(async () => {
    if (!activeDeviceId || !sessionsHasMore || loadingMoreSessions) return;
    const generation = paginationGeneration.current;
    setLoadingMoreSessions(true);
    try {
      const page = await fetchSessions(activeDeviceId, undefined, 500, allSessions.length);
      if (generation !== paginationGeneration.current) return;
      setAllSessions(previous => {
        const seen = new Set(previous.map(session => session.localSessionId));
        return [...previous, ...page.sessions.filter(session => !seen.has(session.localSessionId))];
      });
      setSessionsHasMore(Boolean(page.hasMore));
    } catch (error) {
      if (generation !== paginationGeneration.current) return;
      setSessionsError(error instanceof Error ? error.message : '加载更多会话失败');
    } finally {
      if (generation === paginationGeneration.current) setLoadingMoreSessions(false);
    }
  }, [activeDeviceId, allSessions.length, loadingMoreSessions, sessionsHasMore]);

  const selectSession = useCallback(
    (localSessionId: string) => {
      captureListScroll();
      playNav('push');
      setSelectedLocalSessionId(localSessionId);
      setMobileChat(true);
      onNavigate(activeDeviceId, localSessionId);
    },
    [activeDeviceId, captureListScroll, onNavigate, playNav],
  );

  // Closing the chat: the header button plays the list fade-in, the gesture
  // has already slid the panel away and must not animate again.
  const closeChat = useCallback(
    (withAnimation = true) => {
      if (withAnimation) playNav('pop');
      setMobileChat(false);
      setSelectedLocalSessionId('');
      onNavigate(activeDeviceId);
    },
    [activeDeviceId, onNavigate, playNav],
  );

  // Edge-swipe back with the finger: the panel follows the thumb, then either
  // finishes off-screen or springs back. Replaces the system snapshot gesture,
  // which flashed inside the WKWebView.
  const chatPanelRef = useRef<HTMLElement | null>(null);
  useEffect(() => {
    const panel = chatPanelRef.current;
    if (!panel || !mobileChat) return;
    let tracking = false;
    let horizontal = false;
    let dx = 0;
    let startX = 0;
    let startY = 0;
    let startedAt = 0;

    const settle = (to: number, then?: () => void) => {
      panel.style.transition = 'transform 0.22s cubic-bezier(0.22, 0.61, 0.36, 1)';
      panel.style.transform = `translate3d(${to}px, 0, 0)`;
      window.setTimeout(() => {
        panel.style.transition = '';
        panel.style.transform = '';
        then?.();
      }, 220);
    };

    const onStart = (event: TouchEvent) => {
      if (event.touches.length !== 1) return;
      const touch = event.touches[0];
      if (touch.clientX > 28) return;
      tracking = true;
      horizontal = false;
      dx = 0;
      startX = touch.clientX;
      startY = touch.clientY;
      startedAt = performance.now();
    };

    const onMove = (event: TouchEvent) => {
      if (!tracking) return;
      const touch = event.touches[0];
      const moveX = touch.clientX - startX;
      const moveY = touch.clientY - startY;
      if (!horizontal) {
        if (Math.abs(moveX) < 6 && Math.abs(moveY) < 6) return;
        if (Math.abs(moveX) <= Math.abs(moveY)) {
          tracking = false;
          return;
        }
        horizontal = true;
        panel.style.transition = 'none';
      }
      dx = Math.max(0, moveX);
      event.preventDefault();
      panel.style.transform = `translate3d(${dx}px, 0, 0)`;
    };

    const onEnd = () => {
      if (!tracking) return;
      tracking = false;
      if (!horizontal) return;
      const fast = dx / Math.max(performance.now() - startedAt, 1) > 0.5;
      if (dx > panel.offsetWidth * 0.28 || fast) {
        settle(panel.offsetWidth, () => closeChat(false));
      } else {
        settle(0);
      }
    };

    panel.addEventListener('touchstart', onStart, { passive: true });
    panel.addEventListener('touchmove', onMove, { passive: false });
    panel.addEventListener('touchend', onEnd);
    panel.addEventListener('touchcancel', onEnd);
    return () => {
      panel.removeEventListener('touchstart', onStart);
      panel.removeEventListener('touchmove', onMove);
      panel.removeEventListener('touchend', onEnd);
      panel.removeEventListener('touchcancel', onEnd);
    };
  }, [mobileChat, closeChat]);

  const activeDevice = devices.find(device => device.id === activeDeviceId);
  const query = search.trim().toLowerCase();

  // One new-terminal operation at a time. The directory was picked in the
  // folder browser, so progress lives next to the single "新会话" button.
  const [newSessionOpen, setNewSessionOpen] = useState(false);
  const [newSessionCwd, setNewSessionCwd] = useState<string | null>(null);
  const [newSessionState, setNewSessionState] = useState('');
  // A new-terminal operation is bound to the device it was started on;
  // switching devices mid-poll used to relaunch it on the wrong machine with
  // the old path.
  useEffect(() => {
    setNewSessionCwd(null);
    setNewSessionState('');
  }, [activeDeviceId]);
  useEffect(() => {
    if (!newSessionCwd) return;
    let cancelled = false;
    const tick = async (operationId: string | null, attempt = 0) => {
      if (cancelled) return;
      try {
        if (!operationId) {
          const started = await openNewSession(activeDeviceId, newSessionCwd);
          if (cancelled) return;
          if (!started.operationId) {
            setNewSessionState(started.detail || '无法启动新终端');
            window.setTimeout(() => !cancelled && setNewSessionCwd(null), 4000);
            return;
          }
          setNewSessionState('正在电脑上打开新终端…');
          void tick(started.operationId);
          return;
        }
        const operation = await fetchOperation(operationId);
        if (cancelled) return;
        if (operation.state === 'ready') {
          const target = operation.resolvedLocalSessionId;
          if (target) {
            refreshCatalog();
            selectSession(target);
            setNewSessionCwd(null);
            setNewSessionState('');
          } else {
            // Ready without an id should not happen; let the timeout poll out.
            setNewSessionState('终端已就绪，但未能确定新会话 ID');
            setNewSessionCwd(null);
          }
          return;
        }
        if (operation.state === 'failed') {
          setNewSessionState(operation.detail || '新终端启动失败');
          window.setTimeout(() => !cancelled && setNewSessionCwd(null), 6000);
          return;
        }
        if (operation.state === 'waiting_for_user') {
          setNewSessionState(operation.detail || '终端已打开，等待你在电脑上确认。');
        }
        if (attempt > 90) {
          setNewSessionState('等待新终端超时，请稍后在列表中刷新');
          setNewSessionCwd(null);
          return;
        }
        window.setTimeout(() => void tick(operationId, attempt + 1), 1500);
      } catch (error) {
        if (cancelled) return;
        setNewSessionState(error instanceof Error ? error.message : '启动新终端失败');
        window.setTimeout(() => !cancelled && setNewSessionCwd(null), 5000);
      }
    };
    void tick(null);
    return () => {
      cancelled = true;
    };
    // selectSession/refreshCatalog are stable per device; activeDeviceId is the
    // scope of the whole operation.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [newSessionCwd, activeDeviceId]);

  const startNewSession = useCallback(
    (cwd: string) => {
      if (!activeDevice?.online) {
        setSessionsError('设备离线，无法打开新终端');
        return;
      }
      if (newSessionCwd) return;
      setNewSessionOpen(false);
      setNewSessionState('正在电脑上打开新终端…');
      setNewSessionCwd(cwd);
    },
    [activeDevice?.online, newSessionCwd],
  );

  // One predicate for both filters: the project filter used to ignore
  // summaries, so a summary-only hit showed "没有匹配的项目" while the session
  // filter was written to match them.
  const sessionMatches = useCallback(
    (session: DeviceSessionSummary, q: string): boolean =>
      session.title.toLowerCase().includes(q) || (session.summary ?? '').toLowerCase().includes(q),
    [],
  );

  const filteredProjects = useMemo(() => {
    if (!query) return projects;
    return projects.filter(project => {
      if (project.name.toLowerCase().includes(query) || project.path.toLowerCase().includes(query)) {
        return true;
      }
      return (sessionsByProject[project.projectId] ?? []).some(session => sessionMatches(session, query));
    });
  }, [projects, query, sessionMatches, sessionsByProject]);

  const filteredSessions = useCallback(
    (project: DeviceProject): DeviceSessionSummary[] => {
      const sessions = sessionsByProject[project.projectId] ?? [];
      if (!query) return sessions;
      // A project matched by name/path: show all of its sessions instead of
      // filtering them out by the same query.
      if (project.name.toLowerCase().includes(query) || project.path.toLowerCase().includes(query)) {
        return sessions;
      }
      return sessions.filter(session => sessionMatches(session, query));
    },
    [query, sessionMatches, sessionsByProject],
  );

  return (
    <div className="rcs-page-enter relative flex min-h-0 flex-1 overflow-hidden">
      <aside
        data-chat-open={mobileChat}
        className={cn(
          'workspace-sidebar w-full shrink-0 flex-col border-r border-border/70 bg-surface-0 md:flex md:w-[21rem] flex',
          navDir === 'pop' && 'rcs-nav-pop',
          // The chat panel covers the list on phones; leaving the list in the
          // accessibility tree let VoiceOver swipe into invisible rows.
          mobileChat && 'max-md:hidden',
        )}
      >
        <div className="border-b border-border/70 px-3 pb-2.5 pt-1.5">
          <div className="flex items-center justify-between pb-1.5">
            <h1 className="px-1 text-[20px] font-semibold tracking-[-0.01em] text-text-primary">会话</h1>
            <div className="flex items-center">
              <DebugButton />
              <ThemeToggle />
              <button
                type="button"
                onClick={() => {
                  void refreshDevices(true);
                  refreshCatalog();
                }}
                title="刷新设备"
                aria-label="刷新设备"
                className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full text-text-muted transition-colors hover:bg-surface-2 hover:text-text-secondary active:bg-surface-2 active:text-text-secondary"
              >
                <RefreshCw className={cn('h-4 w-4', (loadingDevices || refreshingDevices) && 'animate-spin')} />
              </button>
            </div>
          </div>
          <div className="relative min-w-0">
            <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-text-muted" />
            <input
              value={search}
              onChange={event => setSearch(event.target.value)}
              placeholder="搜索项目或会话"
              aria-label="搜索项目或会话"
              className="h-10 w-full rounded-xl border border-transparent bg-surface-1 pl-9 pr-3 text-base text-text-primary placeholder:text-text-muted focus:border-brand/40 focus:outline-none sm:text-[15px]"
            />
          </div>
          {devices.length > 0 && (
            <div className="mt-2 flex gap-1.5 overflow-x-auto pb-0.5">
              {devices.map(device => (
                <button
                  key={device.id}
                  type="button"
                  aria-pressed={device.id === activeDeviceId}
                  aria-label={`${device.name}，${device.online ? '设备在线' : '设备离线'}`}
                  onClick={() => {
                    setActiveDeviceId(device.id);
                    setSelectedLocalSessionId('');
                    setMobileChat(false);
                    onNavigate(device.id);
                  }}
                  className={cn(
                    'flex min-h-10 shrink-0 items-center gap-1.5 rounded-full border px-3 py-1.5 text-xs transition-colors',
                    device.id === activeDeviceId
                      ? 'border-brand/40 bg-brand/10 text-text-primary'
                      : 'border-border bg-surface-2 text-text-secondary',
                  )}
                >
                  <span
                    className={cn(
                      'h-1.5 w-1.5 shrink-0 rounded-full',
                      device.online ? 'bg-status-success' : 'bg-text-muted/40',
                    )}
                  />
                  <span className="max-w-[10rem] truncate">{device.name}</span>
                  <span aria-hidden="true" className="text-[11px] text-text-muted">
                    {device.online ? '在线' : '离线'}
                  </span>
                </button>
              ))}
            </div>
          )}
          {devices.length === 0 && !loadingDevices && (
            <p className="mt-2 px-1 text-xs text-text-muted">{devicesError || '还没有设备连接'}</p>
          )}
          {devices.length > 0 && devicesError && <p className="mt-1 px-1 text-xs text-status-error">{devicesError}</p>}

          {/* One entry point for a new terminal: pick a folder on the PC, then
              start it there. The old per-project "+" only worked for folders
              that already had history. */}
          <button
            type="button"
            onClick={() => setNewSessionOpen(true)}
            disabled={!activeDevice || !activeDevice.online || Boolean(newSessionCwd)}
            className={cn(
              'mt-2 flex h-10 w-full items-center justify-center gap-1.5 rounded-xl border text-[14px] font-medium transition-colors disabled:opacity-50',
              'border-brand/35 bg-brand/10 text-brand-dark active:bg-brand/15 dark:text-brand',
            )}
          >
            {newSessionCwd ? (
              <RefreshCw className="h-4 w-4 animate-spin" />
            ) : (
              <Plus className="h-4 w-4" strokeWidth={2.25} />
            )}
            {newSessionCwd ? '正在打开新终端…' : '新会话'}
          </button>
          {!activeDevice?.online && activeDevice && (
            <p className="mt-1.5 px-1 text-[11px] text-text-muted">设备离线，无法新建会话</p>
          )}
          {newSessionState && (
            <p
              className={cn(
                'mt-1.5 px-1 text-[11px] leading-relaxed',
                newSessionCwd ? 'text-text-secondary' : 'text-status-error',
              )}
            >
              {newSessionState}
            </p>
          )}
        </div>

        <div ref={listScrollRef} className="flex-1 overflow-y-auto">
          {projectsError && (
            <div className="mx-3 mb-1 flex items-center gap-2 rounded-lg bg-destructive/10 px-3 py-2 text-xs text-status-error">
              <span className="min-w-0 flex-1">{projectsError}</span>
              <button
                type="button"
                onClick={() => void refreshCatalog()}
                className="shrink-0 font-medium text-brand-dark active:opacity-70 dark:text-brand"
              >
                重试
              </button>
            </div>
          )}
          <div className="list-safe pb-[calc(1rem+env(safe-area-inset-bottom))]">
            {filteredProjects.map(project => {
              const expanded = query
                ? !collapsedSearchProjects.has(project.projectId)
                : expandedProjectId === project.projectId;
              const sessions = filteredSessions(project);
              return (
                <div key={project.projectId}>
                  <div className="flex items-center">
                    <button
                      type="button"
                      onClick={() => toggleProject(project)}
                      aria-expanded={expanded}
                      className="flex min-h-12 min-w-0 flex-1 items-center gap-2.5 px-3 text-left text-[15px] text-text-primary transition-colors hover:bg-surface-1"
                      title={project.path}
                    >
                      {expanded ? (
                        <ChevronDown className="h-3.5 w-3.5 shrink-0 text-text-muted" />
                      ) : (
                        <ChevronRight className="h-3.5 w-3.5 shrink-0 text-text-muted" />
                      )}
                      <Folder className="h-4 w-4 shrink-0 text-text-muted" />
                      <span className="min-w-0 flex-1 truncate">{project.name}</span>
                    </button>
                  </div>
                  {expanded && (
                    <div className="pb-1">
                      {sessionsLoading && allSessions.length === 0 && (
                        <div className="space-y-1 px-8 py-1">
                          <div className="h-6 animate-pulse rounded bg-surface-1" />
                          <div className="h-6 animate-pulse rounded bg-surface-1" />
                        </div>
                      )}
                      {sessions.map(session => (
                        <button
                          key={session.localSessionId}
                          type="button"
                          onClick={() => selectSession(session.localSessionId)}
                          className={cn(
                            'flex min-h-12 w-full items-center gap-2 pl-10 pr-3 text-left transition-colors',
                            selectedLocalSessionId === session.localSessionId
                              ? 'bg-surface-1 text-text-primary'
                              : 'text-text-secondary hover:bg-surface-1',
                          )}
                        >
                          <span className="min-w-0 flex-1 truncate text-[14px]">{session.title}</span>
                          {/* Every row carries the dot: green when a terminal is attached right
                              now, muted otherwise. Hiding it when offline made the whole feature
                              invisible whenever nothing was running. */}
                          <span
                            className={cn(
                              'h-1.5 w-1.5 shrink-0 rounded-full',
                              liveTerminals[session.localSessionId] ? 'bg-status-success' : 'bg-text-muted/35',
                            )}
                            title={liveTerminals[session.localSessionId] ? '终端运行中' : '终端未运行'}
                          >
                            <span className="sr-only">
                              {liveTerminals[session.localSessionId] ? '终端运行中' : '终端未运行'}
                            </span>
                          </span>
                          <span className="shrink-0 text-[12px] text-text-muted">
                            {formatRelativeTime(session.updatedAt)}
                          </span>
                        </button>
                      ))}
                      {sessions.length === 0 && !sessionsLoading && !sessionsError && (
                        <p className="px-9 py-1 text-xs text-text-muted">没有匹配的会话</p>
                      )}
                      {sessionsError && (
                        <div className="mx-3 my-1 flex items-center gap-2 rounded-lg bg-destructive/10 px-3 py-2 text-xs text-status-error">
                          <span className="min-w-0 flex-1">{sessionsError}</span>
                          <button
                            type="button"
                            onClick={() => void refreshCatalog()}
                            className="shrink-0 font-medium text-brand-dark active:opacity-70 dark:text-brand"
                          >
                            重试
                          </button>
                        </div>
                      )}
                    </div>
                  )}
                </div>
              );
            })}
            {sessionsHasMore && (
              <div className="px-3 py-2">
                <button
                  type="button"
                  onClick={() => void loadMoreSessions()}
                  disabled={loadingMoreSessions}
                  className="w-full rounded-lg border border-border bg-surface-1 py-2 text-xs text-text-secondary transition-colors hover:bg-surface-2 disabled:opacity-60"
                >
                  {loadingMoreSessions ? '正在加载…' : '加载更早的会话'}
                </button>
                {/* Searching only covers loaded rows; say so instead of hiding
                    the rest of the corpus behind a silent dead end. */}
                {query && !loadingMoreSessions && (
                  <p className="pt-1 text-center text-[11px] text-text-muted">仅搜索已加载的会话</p>
                )}
              </div>
            )}
            {projectsLoading && projects.length === 0 && (
              <div className="space-y-1.5 px-4 py-2">
                <div className="h-7 animate-pulse rounded bg-surface-1" />
                <div className="h-7 animate-pulse rounded bg-surface-1" />
                <div className="h-7 animate-pulse rounded bg-surface-1" />
              </div>
            )}
            {projects.length === 0 && !projectsLoading && activeDevice && !projectsError && (
              <p className="px-4 py-2 text-xs text-text-muted">
                {activeDevice.online ? '这个设备还没有历史会话' : '设备离线，没有缓存目录'}
              </p>
            )}
            {projects.length > 0 && filteredProjects.length === 0 && (
              <p className="px-4 py-2 text-xs text-text-muted">没有匹配的项目</p>
            )}
          </div>
        </div>
      </aside>

      <main
        ref={chatPanelRef}
        data-chat-open={mobileChat}
        className={cn(
          'workspace-chat min-h-0 min-w-0 flex-1 flex-col bg-surface-0 max-md:absolute max-md:inset-0 max-md:z-10',
          mobileChat ? 'flex' : 'hidden md:flex',
          !mobileChat && 'max-md:pointer-events-none',
          navDir === 'push' && 'rcs-nav-push',
        )}
      >
        {selectedLocalSessionId && activeDeviceId ? (
          <Suspense
            fallback={
              <div className="flex flex-1 items-center justify-center text-sm text-text-muted">正在加载会话…</div>
            }
          >
            <DeviceSessionChat
              key={`${activeDeviceId}:${selectedLocalSessionId}`}
              deviceId={activeDeviceId}
              localSessionId={selectedLocalSessionId}
              deviceName={activeDevice?.name}
              onBack={() => closeChat(true)}
            />
          </Suspense>
        ) : (
          <div className="flex flex-1 items-center justify-center px-6 text-center">
            <div className="max-w-md">
              <div className="mx-auto mb-4 flex h-12 w-12 items-center justify-center rounded-2xl bg-surface-2">
                <Sparkles className="h-6 w-6 text-brand" />
              </div>
              <p className="font-display text-base font-medium text-text-primary">选择会话开始</p>
              <p className="mt-2 text-sm leading-relaxed text-text-muted">
                发送消息会在那台电脑上打开终端；终端没开也能先看历史。
              </p>
            </div>
          </div>
        )}
      </main>

      <DirectoryPicker
        open={newSessionOpen}
        deviceId={activeDeviceId}
        deviceName={activeDevice?.name}
        mode="directory"
        title="新会话"
        onClose={() => setNewSessionOpen(false)}
        onPickDirectory={startNewSession}
      />
    </div>
  );
}
