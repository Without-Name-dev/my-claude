import { useCallback, useEffect, useMemo, useState } from 'react';
import { ChevronDown, ChevronRight, Folder, RefreshCw, Search, Sparkles } from 'lucide-react';
import { cn } from '../lib/utils';
import { formatRelativeTime } from '../lib/device-history';
import {
  fetchDevices,
  fetchProjects,
  fetchSessions,
  type DeviceInfo,
  type DeviceProject,
  type DeviceSessionSummary,
} from '../api/devices';
import { DeviceSessionChat } from '../components/device/DeviceSessionChat';
import { DebugButton } from '../components/DebugPanel';
import { ThemeToggle } from '../components/ThemeToggle';

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
  const [expandedProjectId, setExpandedProjectId] = useState<string | null>(null);
  // Load every session for the device once and filter locally: expanding a
  // project used to trigger a multi-second disk scan on the PC.
  const [allSessions, setAllSessions] = useState<DeviceSessionSummary[]>([]);
  const [sessionsError, setSessionsError] = useState('');
  const [sessionsLoading, setSessionsLoading] = useState(false);
  const [selectedLocalSessionId, setSelectedLocalSessionId] = useState(initialLocalSessionId || '');
  const [mobileChat, setMobileChat] = useState(Boolean(initialLocalSessionId));
  const [search, setSearch] = useState('');

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
    const timer = setInterval(() => void refreshDevices(), 10_000);
    return () => clearInterval(timer);
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

  useEffect(() => {
    if (!activeDeviceId) {
      setProjects([]);
      setAllSessions([]);
      setProjectsLoading(false);
      return;
    }
    let cancelled = false;
    setProjectsError('');
    setSessionsError('');
    setProjectsLoading(true);
    setExpandedProjectId(null);
    setAllSessions([]);
    setSessionsLoading(true);
    void (async () => {
      // Projects and the session index are independent scans on the PC; run
      // them together so the sidebar becomes interactive as soon as either
      // is ready.
      const [projectResult, sessionResult] = await Promise.allSettled([
        fetchProjects(activeDeviceId),
        fetchSessions(activeDeviceId, undefined, 500),
      ]);
      if (cancelled) return;
      if (projectResult.status === 'fulfilled') {
        setProjects(projectResult.value.projects);
      } else {
        setProjects([]);
        setProjectsError(projectResult.reason instanceof Error ? projectResult.reason.message : '无法读取项目');
      }
      if (sessionResult.status === 'fulfilled') {
        setAllSessions(sessionResult.value.sessions);
      } else {
        setSessionsError(sessionResult.reason instanceof Error ? sessionResult.reason.message : '无法读取会话');
      }
      setProjectsLoading(false);
      setSessionsLoading(false);
    })();
    return () => {
      cancelled = true;
    };
  }, [activeDeviceId]);

  const sessionsByProject = useMemo(() => {
    const grouped: Record<string, DeviceSessionSummary[]> = {};
    for (const session of allSessions) {
      (grouped[session.projectId] ??= []).push(session);
    }
    for (const list of Object.values(grouped)) {
      list.sort((a, b) => b.updatedAt - a.updatedAt);
    }
    return grouped;
  }, [allSessions]);

  const toggleProject = useCallback((project: DeviceProject) => {
    setExpandedProjectId(current => (current === project.projectId ? null : project.projectId));
    setSessionsError('');
  }, []);

  const selectSession = useCallback(
    (localSessionId: string) => {
      setSelectedLocalSessionId(localSessionId);
      setMobileChat(true);
      onNavigate(activeDeviceId, localSessionId);
    },
    [activeDeviceId, onNavigate],
  );

  const activeDevice = devices.find(device => device.id === activeDeviceId);
  const query = search.trim().toLowerCase();

  const filteredProjects = useMemo(() => {
    if (!query) return projects;
    return projects.filter(project => {
      if (project.name.toLowerCase().includes(query) || project.path.toLowerCase().includes(query)) {
        return true;
      }
      return (sessionsByProject[project.projectId] ?? []).some(session => session.title.toLowerCase().includes(query));
    });
  }, [projects, query, sessionsByProject]);

  const filteredSessions = useCallback(
    (project: DeviceProject): DeviceSessionSummary[] => {
      const sessions = sessionsByProject[project.projectId] ?? [];
      if (!query) return sessions;
      // A project matched by name/path: show all of its sessions instead of
      // filtering them out by the same query.
      if (project.name.toLowerCase().includes(query) || project.path.toLowerCase().includes(query)) {
        return sessions;
      }
      return sessions.filter(
        session => session.title.toLowerCase().includes(query) || (session.summary ?? '').toLowerCase().includes(query),
      );
    },
    [query, sessionsByProject],
  );

  return (
    <div className="rcs-page-enter flex min-h-0 flex-1 overflow-hidden">
      <aside
        className={cn(
          'w-full shrink-0 flex-col border-r border-border bg-surface-1 md:flex md:w-[21rem]',
          mobileChat ? 'hidden' : 'flex',
        )}
      >
        <div className="border-b border-border px-3 pb-2 pt-2.5">
          <div className="flex items-center gap-1">
            <div className="relative min-w-0 flex-1">
              <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-text-muted" />
              <input
                value={search}
                onChange={event => setSearch(event.target.value)}
                placeholder="搜索项目或会话"
                aria-label="搜索项目或会话"
                className="h-10 w-full rounded-lg border border-border bg-surface-2 pl-9 pr-3 text-base text-text-primary placeholder:text-text-muted focus:border-brand focus:outline-none sm:text-sm"
              />
            </div>
            <DebugButton />
            <ThemeToggle />
            <button
              type="button"
              onClick={() => void refreshDevices(true)}
              title="刷新设备"
              aria-label="刷新设备"
              className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg text-text-muted transition-colors hover:bg-surface-2 hover:text-text-secondary"
            >
              <RefreshCw className={cn('h-4 w-4', (loadingDevices || refreshingDevices) && 'animate-spin')} />
            </button>
          </div>
          {devices.length > 0 && (
            <div className="mt-2 flex gap-1.5 overflow-x-auto pb-0.5">
              {devices.map(device => (
                <button
                  key={device.id}
                  type="button"
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
                </button>
              ))}
            </div>
          )}
          {devices.length === 0 && !loadingDevices && (
            <p className="mt-2 px-1 text-xs text-text-muted">{devicesError || '还没有设备连接'}</p>
          )}
          {devices.length > 0 && devicesError && (
            <p className="mt-1 px-1 text-[11px] text-status-error">{devicesError}</p>
          )}
        </div>

        <div className="flex-1 overflow-y-auto">
          {projectsError && <p className="px-4 py-2 text-xs text-status-error">{projectsError}</p>}
          <div className="pb-[calc(1rem+env(safe-area-inset-bottom))]">
            {filteredProjects.map(project => {
              const expanded = expandedProjectId === project.projectId;
              const sessions = filteredSessions(project);
              return (
                <div key={project.projectId}>
                  <button
                    type="button"
                    onClick={() => toggleProject(project)}
                    className="flex w-full items-center gap-2 px-3 py-2.5 text-left text-sm text-text-secondary transition-colors hover:bg-surface-2/60"
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
                  {expanded && (
                    <div className="pb-1">
                      {sessionsLoading && allSessions.length === 0 && (
                        <div className="space-y-1 px-8 py-1">
                          <div className="h-6 animate-pulse rounded bg-surface-2" />
                          <div className="h-6 animate-pulse rounded bg-surface-2" />
                        </div>
                      )}
                      {sessions.map(session => (
                        <button
                          key={session.localSessionId}
                          type="button"
                          onClick={() => selectSession(session.localSessionId)}
                          className={cn(
                            'flex w-full items-center gap-2 py-2.5 pl-9 pr-3 text-left transition-colors',
                            selectedLocalSessionId === session.localSessionId
                              ? 'bg-surface-2 text-text-primary'
                              : 'text-text-secondary hover:bg-surface-2/60',
                          )}
                        >
                          <span className="min-w-0 flex-1 truncate text-sm">{session.title}</span>
                          <span className="shrink-0 text-[10px] text-text-muted">
                            {formatRelativeTime(session.updatedAt)}
                          </span>
                        </button>
                      ))}
                      {sessions.length === 0 && !sessionsLoading && (
                        <p className="px-9 py-1 text-xs text-text-muted">没有匹配的会话</p>
                      )}
                      {sessionsError && <p className="px-9 py-1 text-xs text-status-error">{sessionsError}</p>}
                    </div>
                  )}
                </div>
              );
            })}
            {projectsLoading && projects.length === 0 && (
              <div className="space-y-1.5 px-4 py-2">
                <div className="h-7 animate-pulse rounded bg-surface-2" />
                <div className="h-7 animate-pulse rounded bg-surface-2" />
                <div className="h-7 animate-pulse rounded bg-surface-2" />
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

      <main className={cn('min-h-0 min-w-0 flex-1 flex-col bg-surface-0', mobileChat ? 'flex' : 'hidden md:flex')}>
        {selectedLocalSessionId && activeDeviceId ? (
          <DeviceSessionChat
            key={`${activeDeviceId}:${selectedLocalSessionId}`}
            deviceId={activeDeviceId}
            localSessionId={selectedLocalSessionId}
            deviceName={activeDevice?.name}
            onBack={() => {
              setMobileChat(false);
              setSelectedLocalSessionId('');
              onNavigate(activeDeviceId);
            }}
          />
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
    </div>
  );
}
