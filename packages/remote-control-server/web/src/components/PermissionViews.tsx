import { useRef, useState } from 'react';
import type { Question } from '../types';
import type { PermissionOption } from '../lib/types';
import { esc, cn, truncate } from '../lib/utils';
import { TriangleAlert, Check } from 'lucide-react';
import { buildQuestionAnswers } from '../lib/question-answers';

// ============================================================
// PermissionPromptView — simple approve/reject for tool use
// ============================================================

export function PermissionPromptView({
  requestId,
  toolName,
  toolInput,
  description,
  options,
  busy,
  onApprove,
  onReject,
  onRespond,
}: {
  requestId: string;
  toolName: string;
  toolInput: unknown;
  description: string;
  /** The CLI's own option list (allow once/always, reject once/always…). */
  options?: PermissionOption[];
  /** The parent knows an answer is already on the wire for this card. */
  busy?: boolean;
  onApprove: () => void | Promise<void>;
  onReject: () => void | Promise<void>;
  onRespond?: (optionId: string, kind: string) => void | Promise<void>;
}) {
  // Both buttons must never fire: the first tap is authoritative and a second
  // one used to send a contradictory "allow" after "reject".
  const [pending, setPending] = useState<string | null>(null);
  const pendingRef = useRef(false);
  const [actionError, setActionError] = useState('');
  const run = (key: string, action: () => void | Promise<void>) => {
    if (pendingRef.current || busy) return;
    pendingRef.current = true;
    setPending(key);
    setActionError('');
    void Promise.resolve()
      .then(action)
      .catch(error => setActionError(error instanceof Error ? error.message : '处理失败，请重试'))
      .finally(() => {
        pendingRef.current = false;
        setPending(null);
      });
  };
  const blocked = pending !== null || Boolean(busy);
  const inputStr = typeof toolInput === 'string' ? toolInput : JSON.stringify(toolInput, null, 2);
  const hasOptions = Boolean(onRespond && options && options.length > 0);

  return (
    <div className="rounded-xl border border-warning-border/30 bg-surface-1 p-4">
      <div className="mb-2 flex items-center gap-2">
        <span className="flex h-5 w-5 items-center justify-center rounded-full bg-warning-border/15 text-warning-text">
          <TriangleAlert className="h-3 w-3" />
        </span>
        <span className="text-sm font-semibold text-warning-text">权限请求</span>
      </div>
      {description && <div className="mb-2 text-[14px] leading-relaxed text-text-secondary">{esc(description)}</div>}
      <div className="mb-2 font-mono text-xs font-bold text-text-primary">{esc(toolName)}</div>
      {toolName !== 'AskUserQuestion' && (
        <pre className="mb-3 max-h-40 overflow-auto rounded-lg bg-surface-2 p-2 text-xs text-text-secondary font-mono">
          {truncate(inputStr, 500)}
        </pre>
      )}
      {actionError && (
        <div role="alert" className="mb-2 text-sm text-status-error">
          {actionError}
        </div>
      )}
      {hasOptions ? (
        // Answer with the CLI's real option ids: "always allow" was impossible
        // from this card before, so every approval had to be repeated.
        <div className="flex flex-wrap gap-2">
          {options!.map(option => {
            const rejected = option.kind === 'reject_once' || option.kind === 'reject_always';
            return (
              <button
                key={option.optionId}
                type="button"
                onClick={() => run(option.optionId, () => onRespond!(option.optionId, option.kind))}
                disabled={blocked}
                className={cn(
                  'min-h-11 rounded-lg px-4 text-sm font-medium transition-colors active:scale-[0.98] disabled:opacity-60',
                  rejected
                    ? 'border border-status-error/30 text-status-error hover:bg-status-error/10'
                    : 'bg-brand text-white hover:bg-brand-light',
                )}
              >
                {pending === option.optionId || busy ? '处理中…' : option.name || option.kind}
              </button>
            );
          })}
        </div>
      ) : (
        <div className="flex gap-2">
          <button
            type="button"
            onClick={() => run('approve', onApprove)}
            disabled={blocked}
            className="min-h-11 min-w-24 rounded-lg bg-brand px-4 text-sm font-medium text-white transition-colors hover:bg-brand-light active:scale-[0.98] active:bg-brand-light disabled:opacity-60"
          >
            {pending === 'approve' || busy ? '处理中…' : '允许'}
          </button>
          <button
            type="button"
            onClick={() => run('reject', onReject)}
            disabled={blocked}
            className="min-h-11 min-w-24 rounded-lg border border-status-error/30 px-4 text-sm font-medium text-status-error transition-colors hover:bg-status-error/10 active:scale-[0.98] active:bg-status-error/15 disabled:opacity-60"
          >
            {pending === 'reject' || busy ? '处理中…' : '拒绝'}
          </button>
        </div>
      )}
    </div>
  );
}

// ============================================================
// AskUserPanelView — multi-question interactive panel
// ============================================================

export function AskUserPanelView({
  questions,
  description,
  busy,
  onSubmit,
  onSkip,
}: {
  requestId: string;
  questions: Question[];
  description: string;
  /** An answer POST is on the wire; taps must not fire a second one. */
  busy?: boolean;
  onSubmit: (answers: Record<string, unknown>) => void;
  onSkip: () => void;
}) {
  const [answers, setAnswers] = useState<Record<string, number | number[]>>({});
  const [otherTexts, setOtherTexts] = useState<Record<string, string>>({});
  const [activeTab, setActiveTab] = useState(0);
  const mappedAnswers = buildQuestionAnswers(questions, answers, otherTexts);

  const handleSelect = (qIdx: number, oIdx: number, multiSelect: boolean) => {
    if (multiSelect) {
      setAnswers(previous => {
        const selected = previous[qIdx];
        const current = Array.isArray(selected) ? selected : [];
        const next = current.includes(oIdx) ? current.filter(i => i !== oIdx) : [...current, oIdx];
        return { ...previous, [qIdx]: next };
      });
    } else {
      setAnswers(previous => ({ ...previous, [qIdx]: oIdx }));
      setOtherTexts(previous => ({ ...previous, [qIdx]: '' }));
    }
  };

  const handleOtherChange = (qIdx: number, value: string, multiSelect: boolean) => {
    setOtherTexts(previous => ({ ...previous, [qIdx]: value }));
    if (!multiSelect && value.trim()) {
      setAnswers(previous => {
        const next = { ...previous };
        delete next[qIdx];
        return next;
      });
    }
  };

  const handleSubmit = () => {
    if (busy || Object.keys(mappedAnswers).length === 0) return;
    onSubmit(mappedAnswers);
  };

  // Single question — simple layout
  if (questions.length <= 1) {
    const q = questions[0] || { question: description, options: [], multiSelect: false };
    const multiSelect = q.multiSelect || false;

    return (
      <div className="rounded-xl border border-brand/30 bg-surface-1 p-4">
        <div className="mb-3 text-sm font-semibold text-text-primary">
          {esc(q.question || description || 'Claude 需要一个回答')}
        </div>
        {description && q.question && description !== q.question && (
          <div className="mb-3 text-sm text-text-secondary">{esc(description)}</div>
        )}
        <div className="space-y-2">
          {(q.options || []).map((opt, j) => {
            const isSelected = multiSelect ? ((answers[0] as number[]) || []).includes(j) : answers[0] === j;
            return (
              <button
                type="button"
                key={j}
                onClick={() => handleSelect(0, j, multiSelect)}
                aria-pressed={isSelected}
                className={cn(
                  'min-h-11 w-full rounded-lg border px-4 py-2.5 text-left text-sm transition-colors',
                  isSelected
                    ? 'border-brand bg-brand/10 text-text-primary'
                    : 'border-border bg-surface-2 text-text-secondary hover:border-border-light',
                )}
              >
                <div className="font-medium">{esc(opt.label)}</div>
                {opt.description && <div className="mt-0.5 text-xs text-text-muted">{esc(opt.description)}</div>}
              </button>
            );
          })}
          <div className="flex gap-2">
            <input
              type="text"
              aria-label="其他回答"
              value={otherTexts[0] || ''}
              onChange={e => handleOtherChange(0, e.target.value, multiSelect)}
              placeholder="其他…"
              className="flex-1 rounded-lg border border-border bg-surface-2 px-3 py-2 text-sm text-text-primary placeholder:text-text-muted focus:border-brand focus:outline-none"
            />
          </div>
        </div>
        <div className="mt-4 flex gap-2">
          <button
            type="button"
            onClick={handleSubmit}
            disabled={Boolean(busy) || Object.keys(mappedAnswers).length === 0}
            className="min-h-11 rounded-lg bg-brand px-4 text-sm font-medium text-white transition-colors hover:bg-brand-light active:bg-brand-light disabled:opacity-60"
          >
            {busy ? '处理中…' : '提交'}
          </button>
          <button
            type="button"
            onClick={onSkip}
            disabled={Boolean(busy)}
            className="min-h-11 rounded-lg border border-border px-4 text-sm text-text-secondary transition-colors hover:bg-surface-2 active:bg-surface-2 disabled:opacity-60"
          >
            跳过
          </button>
        </div>
      </div>
    );
  }

  // Multiple questions — tab layout
  const currentQ = questions[activeTab];

  return (
    <div className="rounded-xl border border-brand/30 bg-surface-1 p-4">
      <div className="mb-3 text-sm font-semibold text-text-primary">{esc(description || '问题')}</div>
      <div className="mb-3 flex gap-1 overflow-x-auto">
        {questions.map((q, i) => (
          <button
            type="button"
            key={i}
            onClick={() => setActiveTab(i)}
            aria-pressed={activeTab === i}
            className={cn(
              'rounded-md px-3 py-1.5 text-xs whitespace-nowrap transition-colors',
              activeTab === i ? 'bg-brand/20 text-brand-dark dark:text-brand' : 'text-text-muted hover:bg-surface-2',
            )}
          >
            {q.header || `Q${i + 1}`}
          </button>
        ))}
      </div>

      {currentQ && (
        <QuestionTab
          question={currentQ}
          qIdx={activeTab}
          answers={answers}
          otherTexts={otherTexts}
          onSelect={handleSelect}
          onOtherTextChange={(qIdx, text) => handleOtherChange(qIdx, text, Boolean(currentQ.multiSelect))}
        />
      )}

      <div className="mt-4 flex items-center justify-between">
        <span className="text-xs text-text-muted">
          {activeTab + 1} / {questions.length}
        </span>
        <div className="flex gap-2">
          <button
            type="button"
            onClick={handleSubmit}
            disabled={Boolean(busy) || Object.keys(mappedAnswers).length === 0}
            className="min-h-11 rounded-lg bg-brand px-4 text-sm font-medium text-white transition-colors hover:bg-brand-light active:bg-brand-light disabled:opacity-60"
          >
            {busy ? '处理中…' : '全部提交'}
          </button>
          <button
            type="button"
            onClick={onSkip}
            disabled={Boolean(busy)}
            className="min-h-11 rounded-lg border border-border px-4 text-sm text-text-secondary transition-colors hover:bg-surface-2 active:bg-surface-2 disabled:opacity-60"
          >
            跳过
          </button>
        </div>
      </div>
    </div>
  );
}

function QuestionTab({
  question,
  qIdx,
  answers,
  otherTexts,
  onSelect,
  onOtherTextChange,
}: {
  question: Question;
  qIdx: number;
  answers: Record<string, number | number[]>;
  otherTexts: Record<string, string>;
  onSelect: (qIdx: number, oIdx: number, multiSelect: boolean) => void;
  onOtherTextChange: (qIdx: number, text: string) => void;
}) {
  const multiSelect = question.multiSelect || false;

  return (
    <div>
      <div className="mb-2 text-[14px] leading-relaxed text-text-secondary">{esc(question.question)}</div>
      <div className="space-y-2">
        {(question.options || []).map((opt, j) => {
          const isSelected = multiSelect ? ((answers[qIdx] as number[]) || []).includes(j) : answers[qIdx] === j;
          return (
            <button
              type="button"
              key={j}
              onClick={() => onSelect(qIdx, j, multiSelect)}
              aria-pressed={isSelected}
              className={cn(
                'min-h-11 w-full rounded-lg border px-4 py-2.5 text-left text-sm transition-colors',
                isSelected
                  ? 'border-brand bg-brand/10 text-text-primary'
                  : 'border-border bg-surface-2 text-text-secondary hover:border-border-light',
              )}
            >
              <div className="font-medium">{esc(opt.label)}</div>
              {opt.description && <div className="mt-0.5 text-xs text-text-muted">{esc(opt.description)}</div>}
            </button>
          );
        })}
        <div className="flex gap-2">
          <input
            type="text"
            aria-label="其他回答"
            value={otherTexts[qIdx] || ''}
            onChange={e => onOtherTextChange(qIdx, e.target.value)}
            placeholder="其他…"
            className="flex-1 rounded-lg border border-border bg-surface-2 px-3 py-2 text-sm text-text-primary placeholder:text-text-muted focus:border-brand focus:outline-none"
          />
        </div>
      </div>
    </div>
  );
}

// ============================================================
// PlanPanelView — plan approval with feedback
// ============================================================

export function PlanPanelView({
  planContent,
  description,
  busy,
  onSubmit,
}: {
  requestId: string;
  planContent: string;
  description: string;
  /** An answer POST is on the wire; taps must not fire a second one. */
  busy?: boolean;
  onSubmit: (value: string, feedback?: string) => void;
}) {
  const [selected, setSelected] = useState<string | null>(null);
  const [feedback, setFeedback] = useState('');
  const isEmpty = !planContent || !planContent.trim();

  const handleSubmit = () => {
    if (!selected || busy) return;
    onSubmit(selected, selected === 'no' ? feedback : undefined);
  };

  return (
    <div className="rounded-xl border border-brand/30 bg-surface-1 p-4">
      <div className="mb-3 flex items-center gap-2">
        <span className="flex h-5 w-5 items-center justify-center rounded-full bg-brand/15 text-brand">
          <Check className="h-3 w-3" strokeWidth={2.5} />
        </span>
        <span className="text-sm font-semibold text-text-primary">{isEmpty ? '退出计划模式？' : '准备开始编写？'}</span>
      </div>
      {!isEmpty && (
        <div
          // 卡片本身是 surface-1：浅色下 tool-card(#f7f7f9) 和它几乎同色，
          // 计划正文区域看起来“没有背景”；surface-2 在两种主题里都可见。
          className="mb-4 max-h-64 overflow-auto rounded-lg bg-surface-2 p-4 text-sm text-text-secondary"
          // biome-ignore lint/security/noDangerouslySetInnerHtml: content is sanitized by formatPlanContent
          dangerouslySetInnerHTML={{ __html: formatPlanContent(planContent) }}
        />
      )}
      <div className="space-y-2">
        {isEmpty ? (
          <>
            <PlanOption selected={selected === 'yes-default'} onClick={() => setSelected('yes-default')} label="是" />
            <PlanOption selected={selected === 'no'} onClick={() => setSelected('no')} label="否" />
          </>
        ) : (
          <>
            <PlanOption
              selected={selected === 'yes-accept-edits'}
              onClick={() => setSelected('yes-accept-edits')}
              label="是，自动接受编辑"
              desc="批准计划并自动接受文件修改"
            />
            <PlanOption
              selected={selected === 'yes-default'}
              onClick={() => setSelected('yes-default')}
              label="是，逐条确认编辑"
              desc="批准计划，但每次修改都需要确认"
            />
            <PlanOption
              selected={selected === 'no'}
              onClick={() => setSelected('no')}
              label="否，继续完善计划"
              desc="补充你的意见，继续调整计划"
            />
          </>
        )}
      </div>
      {selected === 'no' && (
        <textarea
          value={feedback}
          onChange={e => setFeedback(e.target.value)}
          placeholder="告诉 Claude 需要改什么…"
          className="mt-3 w-full rounded-lg border border-border bg-surface-2 px-3 py-2 text-sm text-text-primary placeholder:text-text-muted focus:border-brand focus:outline-none"
          rows={3}
        />
      )}
      <div className="mt-4">
        <button
          type="button"
          onClick={handleSubmit}
          disabled={!selected || Boolean(busy)}
          className="min-h-11 rounded-lg bg-brand px-4 py-2 text-sm font-medium text-white hover:bg-brand-light active:bg-brand-light disabled:opacity-50 transition-colors"
        >
          {busy ? '处理中…' : '提交'}
        </button>
      </div>
    </div>
  );
}

function PlanOption({
  selected,
  onClick,
  label,
  desc,
}: {
  selected: boolean;
  onClick: () => void;
  label: string;
  desc?: string;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        'min-h-11 w-full rounded-lg border px-4 py-2.5 text-left text-sm transition-colors',
        selected
          ? 'border-brand bg-brand/10 text-text-primary'
          : 'border-border bg-surface-2 text-text-secondary hover:border-border-light',
      )}
    >
      <div className="flex items-center gap-2">
        <span
          className={cn(
            'flex h-4 w-4 items-center justify-center rounded-full border text-[10px] transition-colors',
            selected ? 'border-brand bg-brand text-white' : 'border-border',
          )}
        >
          {selected && '\u2713'}
        </span>
        <span className="font-medium">{label}</span>
      </div>
      {desc && <div className="mt-0.5 pl-6 text-xs text-text-muted">{desc}</div>}
    </button>
  );
}

function formatPlanContent(content: string): string {
  // esc() is for React text nodes, not HTML. This string is inserted as HTML,
  // so escape untrusted plan content before adding our own formatting tags.
  let html = content
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
  html = html.replace(
    /```(\w*)\n?([\s\S]*?)```/g,
    (_, _l, code) =>
      `<pre class="my-2 overflow-x-auto rounded-lg bg-surface-1 p-3 font-mono text-xs">${code.trim()}</pre>`,
  );
  html = html.replace(/`([^`]+)`/g, '<code class="rounded bg-surface-1 px-1.5 py-0.5 font-mono text-xs">$1</code>');
  html = html.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
  return html;
}
