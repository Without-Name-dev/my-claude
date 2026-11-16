import { expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { formatQuotaReset, ProviderQuota } from '../components/device/ProviderQuota';

test('displays each returned quota and its reset countdown', () => {
  const html = renderToStaticMarkup(
    createElement(ProviderQuota, {
      state: {
        status: 'ready',
        windows: [
          { label: '5 小时', remaining: 84, resetAt: Date.now() + 60_000 },
          { label: '每周', remaining: 31 },
        ],
      },
      onRefresh: () => {},
    }),
  );
  expect(html).toContain('5 小时剩余');
  expect(html).toContain('84%');
  expect(html).toContain('每周剩余');
  expect(html).toContain('31%');
  expect(html).toContain('后重置');
  expect(formatQuotaReset(0, 1)).toBe('0分钟');
  expect(formatQuotaReset(25 * 60 * 60 * 1000, 0)).toBe('1天1小时');
});

test('distinguishes loading, unavailable and retryable errors', () => {
  const render = (status: 'loading' | 'ready' | 'error') =>
    renderToStaticMarkup(
      createElement(ProviderQuota, {
        state: { status, windows: [] },
        onRefresh: () => {},
      }),
    );
  expect(render('loading')).toContain('正在查询额度');
  expect(render('ready')).toContain('暂无可用额度数据');
  expect(render('error')).toContain('额度查询失败');
  expect(render('error')).toContain('刷新额度');
});
