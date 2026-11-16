import { useEffect, useState } from 'react';
import { SlidersHorizontal } from 'lucide-react';
import { apiFetchProviderQuota, apiFetchProviders } from '../../api/client';
import { updateSessionSettings, type SessionSettings, type SessionSettingsValues } from '../../api/devices';
import { ComposerSettings } from './ComposerSettings';
import { composerModelLabel } from '../../lib/composer-model-label';

const permissions = [
  { id: 'default', label: '默认' },
  { id: 'acceptEdits', label: '自动接受编辑' },
  { id: 'plan', label: '计划模式' },
  { id: 'auto', label: '自动' },
];

export function OfflineSessionSettings({
  deviceId,
  sessionId,
  settings,
  onChange,
}: {
  deviceId: string;
  sessionId: string;
  settings: SessionSettings | null | undefined;
  onChange: (settings: SessionSettings) => void;
}) {
  const [open, setOpen] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  const [failed, setFailed] = useState(false);
  const [models, setModels] = useState<
    { id: string; label: string; quota?: { label: string; remaining: number; resetAt?: number }[] }[]
  >([]);
  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    setLoading(true);
    setFailed(false);
    void apiFetchProviders()
      .then(async ({ providers }) => {
        const rows = providers.flatMap(provider =>
          provider.models.map(model => ({
            id: `rcs-provider:${provider.id}:${encodeURIComponent(model)}`,
            label: `${provider.name} / ${provider.modelDetails?.find(detail => detail.model === model)?.displayName ?? model}`,
          })),
        );
        if (!cancelled) setModels(rows);
        await Promise.all(
          providers.map(async provider => {
            try {
              const { windows } = await apiFetchProviderQuota(provider.id);
              if (!cancelled && windows.length)
                setModels(previous =>
                  previous.map(row =>
                    row.id.startsWith(`rcs-provider:${provider.id}:`) ? { ...row, quota: windows } : row,
                  ),
                );
            } catch {
              /* Providers without quota support remain selectable. */
            }
          }),
        );
      })
      .catch(() => {
        if (!cancelled) setFailed(true);
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [open]);

  const values = settings?.values ?? {};
  const model = values.model ?? '';
  const label = composerModelLabel(model, model);
  const change = async (patch: SessionSettingsValues) => {
    setPending(true);
    setError('');
    try {
      const result = await updateSessionSettings(deviceId, sessionId, patch);
      onChange(result.settings);
      if (result.settings.error) setError(`已保存，终端未能应用：${result.settings.error}`);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : '保存设置失败');
    } finally {
      setPending(false);
    }
  };
  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        aria-label="会话设置"
        className="flex h-8 min-w-0 items-center gap-1 rounded-full px-1.5 text-text-secondary active:bg-surface-1"
      >
        <SlidersHorizontal className="h-4 w-4 shrink-0" />
        <span className="truncate text-xs">{label || '会话设置'}</span>
      </button>
      <ComposerSettings
        open={open}
        onClose={() => setOpen(false)}
        pending={pending}
        controlError={
          error ||
          (settings?.error
            ? `等待终端应用：${settings.error}`
            : settings?.pending.length
              ? '已保存，终端启动后生效'
              : '')
        }
        effort={values.effort ?? ''}
        ultracode={values.ultracode ?? false}
        ultracodeUsable={true}
        permissionMode={values.permissionMode ?? ''}
        permissionOptions={permissions}
        modelId={model}
        currentModelName={label}
        currentModelIsLocal={Boolean(model) && !model.startsWith('rcs-provider:')}
        providerSwitchAvailable={true}
        providersLoading={loading}
        providersError={failed}
        models={models}
        onChangeEffort={effort => change({ effort })}
        onToggleUltracode={() => change({ ultracode: !values.ultracode })}
        onChangePermissionMode={permissionMode => change({ permissionMode })}
        onChangeModel={next => change({ model: next })}
      />
    </>
  );
}
