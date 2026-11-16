import { useCallback, useEffect, useRef, useState } from 'react';
import { Plus, X } from 'lucide-react';
import {
  apiDeleteProvider,
  apiFetchProviders,
  apiFetchProviderSecret,
  apiFetchProviderQuota,
  apiSaveProvider,
  type ProviderInfo,
  type ProviderInput,
} from '../../api/client';
import {
  flushClientLogs,
  getUploadState,
  setUploadOverride,
  subscribeUploadSetting,
} from '../../lib/client-log-upload';
import { ModalSheet } from './ModalSheet';
import { ProviderQuota, type QuotaState } from './ProviderQuota';

type Draft = {
  name: string;
  protocol: ProviderInfo['protocol'];
  baseUrl: string;
  authMode: 'api-key' | 'bearer';
  apiKey: string;
  models: { model: string; displayName: string; context1m: boolean }[];
};

/**
 * One-click switch for client log upload. On = follow the server setting
 * (default on); off = never upload from this device. The status line tells
 * the two "off" causes apart (local switch vs server RCS_CLIENT_LOG_UPLOAD=0).
 */
function LogUploadSection() {
  const [state, setState] = useState(getUploadState());
  useEffect(() => subscribeUploadSetting(() => setState(getUploadState())), []);
  const status = !state.enabled
    ? state.localOverride === false
      ? '已在本机关闭'
      : '服务器已关闭上报'
    : state.serverEnabled === false
      ? '已开启（等待服务器允许）'
      : `上报中 · 已发送 ${state.shippedTotal} 条`;
  return (
    <section className="mt-6 border-t border-border pt-4">
      <div className="flex items-center justify-between gap-3">
        <div>
          <div className="text-sm font-medium">诊断日志</div>
          <p className="mt-0.5 text-xs text-text-muted">{status}</p>
        </div>
        <div className="flex items-center gap-3">
          {state.enabled && (
            <button
              type="button"
              onClick={() => void flushClientLogs('manual')}
              className="min-h-9 rounded-lg border border-border px-3 text-xs text-text-secondary active:bg-surface-1"
            >
              立即上报
            </button>
          )}
          <button
            type="button"
            role="switch"
            aria-checked={state.enabled}
            aria-label="日志上报"
            onClick={() => setUploadOverride(!state.enabled ? null : false)}
            className={`flex h-7 w-12 shrink-0 items-center rounded-full px-0.5 transition-colors ${
              state.enabled ? 'bg-brand' : 'bg-surface-2'
            }`}
          >
            <span
              className={`h-6 w-6 rounded-full bg-white shadow transition-transform ${
                state.enabled ? 'translate-x-5' : 'translate-x-0'
              }`}
            />
          </button>
        </div>
      </div>
      <p className="mt-2 text-xs leading-5 text-text-muted">
        记录本机操作日志（消息收发、连接状态、错误）并定期发送到服务器，用于排查手机端问题。服务器可通过
        RCS_CLIENT_LOG_UPLOAD=0 全局关闭。
      </p>
    </section>
  );
}
const emptyDraft = (): Draft => ({
  name: '',
  protocol: 'openai',
  baseUrl: '',
  authMode: 'api-key',
  apiKey: '',
  models: [{ model: '', displayName: '', context1m: false }],
});
const field =
  'mt-1 w-full rounded-xl border border-border bg-surface-1 px-3 py-2.5 text-base text-text-primary outline-none focus:border-brand';

export function ProviderManager({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [items, setItems] = useState<ProviderInfo[]>([]);
  const [editing, setEditing] = useState<string | null | undefined>(undefined);
  const [draft, setDraft] = useState<Draft>(emptyDraft);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [keyTouched, setKeyTouched] = useState(false);
  const [originalEndpoint, setOriginalEndpoint] = useState('');
  const [quotas, setQuotas] = useState<Record<string, QuotaState>>({});
  const editGeneration = useRef(0);
  const quotaGeneration = useRef(0);
  const quotaRequests = useRef<Record<string, number>>({});
  const loadQuota = useCallback(async (id: string, generation: number) => {
    const request = (quotaRequests.current[id] ?? 0) + 1;
    quotaRequests.current[id] = request;
    setQuotas(previous => ({ ...previous, [id]: { status: 'loading', windows: previous[id]?.windows ?? [] } }));
    try {
      const { windows } = await apiFetchProviderQuota(id);
      if (generation === quotaGeneration.current && quotaRequests.current[id] === request) {
        setQuotas(previous => ({ ...previous, [id]: { status: 'ready', windows } }));
      }
    } catch {
      if (generation === quotaGeneration.current && quotaRequests.current[id] === request) {
        setQuotas(previous => ({ ...previous, [id]: { status: 'error', windows: [] } }));
      }
    }
  }, []);
  const refresh = useCallback(async () => {
    const generation = ++quotaGeneration.current;
    try {
      const { providers } = await apiFetchProviders();
      if (generation !== quotaGeneration.current) return;
      setItems(providers);
      setQuotas({});
      providers.forEach(provider => void loadQuota(provider.id, generation));
      setError('');
    } catch (e) {
      if (generation === quotaGeneration.current) setError(e instanceof Error ? e.message : '无法读取供应商');
    }
  }, [loadQuota]);
  useEffect(() => {
    if (open) {
      void refresh();
      return () => {
        quotaGeneration.current++;
      };
    } else {
      quotaGeneration.current++;
      editGeneration.current++;
      setBusy(false);
      setDraft(emptyDraft());
      setEditing(undefined);
    }
  }, [open, refresh]);
  const edit = async (item?: ProviderInfo) => {
    const generation = ++editGeneration.current;
    setError('');
    setKeyTouched(false);
    setOriginalEndpoint(item ? `${item.protocol}\n${item.baseUrl}` : '');
    setEditing(item?.id ?? null);
    setDraft(
      item
        ? {
            name: item.name,
            protocol: item.protocol,
            baseUrl: item.baseUrl,
            authMode: item.authMode ?? 'api-key',
            apiKey: '',
            models: item.models.map(
              model =>
                item.modelDetails?.find(detail => detail.model === model) ?? {
                  model,
                  displayName: model,
                  context1m: false,
                },
            ),
          }
        : emptyDraft(),
    );
    if (item) {
      setBusy(true);
      try {
        const secret = await apiFetchProviderSecret(item.id);
        if (generation === editGeneration.current) setDraft(previous => ({ ...previous, apiKey: secret.apiKey }));
      } catch (e) {
        if (generation === editGeneration.current) setError(e instanceof Error ? e.message : '无法读取密钥');
      } finally {
        if (generation === editGeneration.current) setBusy(false);
      }
    }
  };
  const save = async () => {
    const details = draft.models.map(detail => ({
      ...detail,
      model: detail.model.trim(),
      displayName: detail.displayName.trim(),
    }));
    const models = details.map(detail => detail.model);
    if (
      !draft.name.trim() ||
      !draft.baseUrl.trim() ||
      !models.length ||
      details.some(detail => !detail.model || !detail.displayName) ||
      new Set(models).size !== models.length ||
      (!editing && !draft.apiKey.trim())
    ) {
      setError('请填写名称、接口地址、模型，以及新供应商的 API Key。');
      return;
    }
    if (editing && `${draft.protocol}\n${draft.baseUrl.trim()}` !== originalEndpoint && !keyTouched) {
      setError('修改接口地址或协议时，请重新填写 API Key。');
      return;
    }
    const payload: ProviderInput = {
      name: draft.name.trim(),
      protocol: draft.protocol,
      baseUrl: draft.baseUrl.trim(),
      authMode: draft.authMode,
      models,
      modelDetails: details,
      ...((!editing || keyTouched) && draft.apiKey ? { apiKey: draft.apiKey } : {}),
    };
    setBusy(true);
    setError('');
    try {
      await apiSaveProvider(payload, editing ?? undefined);
      setEditing(undefined);
      setDraft(emptyDraft());
      await refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message : '保存失败');
    } finally {
      setBusy(false);
    }
  };
  const remove = async (id: string) => {
    if (!window.confirm('删除此供应商？已运行的请求不会立即中断。')) return;
    setBusy(true);
    setError('');
    try {
      await apiDeleteProvider(id);
      await refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message : '删除失败');
    } finally {
      setBusy(false);
    }
  };
  return (
    <ModalSheet
      open={open}
      onClose={onClose}
      label="供应商管理"
      className="flex max-h-[calc(var(--app-height,100dvh)*0.9)] flex-col overflow-hidden rounded-t-[20px] border border-border bg-surface-0 px-4 pb-[calc(1rem+env(safe-area-inset-bottom))] pt-4 shadow-2xl sm:max-w-lg sm:rounded-2xl"
    >
      <div className="mb-4 flex items-center justify-between">
        <h2 className="text-lg font-semibold">供应商管理</h2>
        <button type="button" onClick={onClose} aria-label="关闭" className="rounded-full p-2 text-text-muted">
          <X size={18} />
        </button>
      </div>
      <div className="min-h-0 overflow-y-auto overscroll-contain hide-scrollbar">
        {error && (
          <p role="alert" className="mb-3 rounded-xl bg-destructive/10 p-3 text-sm text-destructive">
            {error}
          </p>
        )}
        {editing === undefined ? (
          <>
            <p className="mb-4 text-sm text-text-muted">
              配置保存在服务器，CLI 切换时获取并直接连接供应商。修改接口地址或协议时需重新填写密钥。
            </p>
            <button
              type="button"
              onClick={() => edit()}
              className="mb-3 flex min-h-11 items-center gap-2 rounded-xl bg-brand px-4 text-sm font-medium text-white"
            >
              <Plus size={16} />
              添加供应商
            </button>
            {items.length === 0 && <p className="py-5 text-sm text-text-muted">还没有供应商。</p>}
            <div className="space-y-2">
              {items.map(item => (
                <div key={item.id} className="rounded-xl border border-border bg-surface-1 p-3">
                  <div className="font-medium">{item.name}</div>
                  <div className="mt-1 break-all text-xs text-text-muted">
                    {item.protocol} · {item.baseUrl}
                  </div>
                  <div className="mt-1 text-xs text-text-muted">
                    {item.models.join('、')} · {item.hasApiKey ? '密钥已配置' : '未配置密钥'}
                  </div>
                  <ProviderQuota
                    state={quotas[item.id]}
                    onRefresh={() => void loadQuota(item.id, quotaGeneration.current)}
                  />
                  <div className="mt-3 flex gap-4 text-sm">
                    <button type="button" onClick={() => edit(item)} className="text-brand">
                      编辑
                    </button>
                    <button
                      type="button"
                      disabled={busy}
                      onClick={() => void remove(item.id)}
                      className="text-destructive"
                    >
                      删除
                    </button>
                  </div>
                </div>
              ))}
            </div>
            <LogUploadSection />
          </>
        ) : (
          <form
            onSubmit={event => {
              event.preventDefault();
              void save();
            }}
            className="space-y-3"
          >
            <label className="block text-sm">
              名称
              <input
                className={field}
                value={draft.name}
                onChange={e => setDraft({ ...draft, name: e.target.value })}
                autoComplete="off"
              />
            </label>
            <label className="block text-sm">
              接口协议
              <select
                className={field}
                value={draft.protocol}
                onChange={e => setDraft({ ...draft, protocol: e.target.value as Draft['protocol'] })}
              >
                <option value="openai">OpenAI 兼容</option>
                <option value="anthropic">Anthropic 兼容</option>
                <option value="gemini">Gemini</option>
                <option value="grok">Grok</option>
              </select>
            </label>
            <label className="block text-sm">
              接口地址
              <input
                className={field}
                value={draft.baseUrl}
                onChange={e => setDraft({ ...draft, baseUrl: e.target.value })}
                placeholder="https://example.com/v1"
                autoCapitalize="none"
                autoCorrect="off"
              />
            </label>
            {draft.protocol === 'anthropic' && (
              <label className="block text-sm">
                密钥格式
                <select
                  className={field}
                  value={draft.authMode}
                  onChange={e => setDraft({ ...draft, authMode: e.target.value as Draft['authMode'] })}
                >
                  <option value="api-key">x-api-key</option>
                  <option value="bearer">Bearer Token</option>
                </select>
              </label>
            )}
            {draft.protocol === 'anthropic' &&
              draft.baseUrl.includes('tokenhub.tencentmaas.com/plan/') &&
              draft.authMode !== 'bearer' && (
                <p className="rounded-xl bg-brand/10 px-3 py-2 text-xs leading-5 text-text-secondary">
                  腾讯官方建议使用 Bearer Token，并需使用 Token Plan 专属 API Key。
                </p>
              )}
            <label className="block text-sm">
              API Key
              <input
                className={field}
                type="text"
                value={draft.apiKey}
                onChange={e => {
                  setKeyTouched(true);
                  setDraft({ ...draft, apiKey: e.target.value });
                }}
                placeholder={editing ? '留空则保持原密钥' : '输入供应商密钥'}
                autoComplete="new-password"
              />
            </label>
            <fieldset className="space-y-2">
              <legend className="text-sm font-medium">模型配置</legend>
              {draft.models.map((model, index) => (
                <div key={index} className="space-y-2 rounded-xl border border-border bg-surface-1 p-3">
                  <div className="grid gap-2 sm:grid-cols-2">
                    <label className="text-xs text-text-muted">
                      显示名称
                      <input
                        className={field}
                        value={model.displayName}
                        onChange={e =>
                          setDraft(previous => ({
                            ...previous,
                            models: previous.models.map((row, i) =>
                              i === index ? { ...row, displayName: e.target.value } : row,
                            ),
                          }))
                        }
                      />
                    </label>
                    <label className="text-xs text-text-muted">
                      实际请求模型
                      <input
                        className={field}
                        value={model.model}
                        onChange={e =>
                          setDraft(previous => ({
                            ...previous,
                            models: previous.models.map((row, i) =>
                              i === index ? { ...row, model: e.target.value } : row,
                            ),
                          }))
                        }
                        autoCapitalize="none"
                        autoCorrect="off"
                      />
                    </label>
                  </div>
                  <div className="flex items-center justify-between gap-2">
                    <label className="flex min-h-9 items-center gap-2 text-sm">
                      <input
                        type="checkbox"
                        checked={model.context1m}
                        onChange={e =>
                          setDraft(previous => ({
                            ...previous,
                            models: previous.models.map((row, i) =>
                              i === index ? { ...row, context1m: e.target.checked } : row,
                            ),
                          }))
                        }
                      />
                      声明支持 1M 上下文
                    </label>
                    {draft.models.length > 1 && (
                      <button
                        type="button"
                        className="text-sm text-destructive"
                        onClick={() =>
                          setDraft(previous => ({ ...previous, models: previous.models.filter((_, i) => i !== index) }))
                        }
                      >
                        删除
                      </button>
                    )}
                  </div>
                </div>
              ))}
              <button
                type="button"
                className="min-h-10 text-sm text-brand"
                onClick={() =>
                  setDraft(previous => ({
                    ...previous,
                    models: [...previous.models, { model: '', displayName: '', context1m: false }],
                  }))
                }
              >
                ＋ 添加模型
              </button>
            </fieldset>
            <div className="flex gap-3 pb-1">
              <button
                type="submit"
                disabled={busy}
                className="min-h-11 rounded-xl bg-brand px-5 text-sm text-white disabled:opacity-50"
              >
                {busy ? '保存中…' : '保存'}
              </button>
              <button
                type="button"
                disabled={busy}
                onClick={() => {
                  editGeneration.current++;
                  setBusy(false);
                  setEditing(undefined);
                  setDraft(emptyDraft());
                  setError('');
                }}
                className="min-h-11 rounded-xl bg-surface-1 px-5 text-sm"
              >
                取消
              </button>
            </div>
          </form>
        )}
      </div>
    </ModalSheet>
  );
}
