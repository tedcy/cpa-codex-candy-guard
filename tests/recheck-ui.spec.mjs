import { test, expect } from '@playwright/test';

const ui = '/v0/resource/plugins/cpa-codex-candy-guard/ui';
const base = '/v0/management/plugins/cpa-codex-candy-guard';

async function mockGuard(page) {
  const state = {
    settings: { model: '', per_credential_concurrency: 3, credential_concurrency: 2, interval_minutes: 10, recheck_enabled: false, recheck_delay_minutes: 10, schedule_enabled: false, scope: 'all_enabled', credential_ids: [] },
    credentials: [0, 1].map(id => ({ id: String(id), name: `codex-synthetic-account-${id}.json`, disabled: false })),
    models: ['gpt-6.1-sol'], rounds: [], running: false, stopping: false, ready: true
  };
  let runs = 0;
  await page.route('**' + base + '/**', async route => {
    const request = route.request();
    const path = new URL(request.url()).pathname.slice(base.length);
    if (path === '/settings' && request.method() === 'PUT') {
      state.settings = request.postDataJSON();
      return route.fulfill({ json: { saved: true } });
    }
    if (path === '/run') {
      runs++;
      state.running = true;
      state.next_run = new Date(Date.now() + state.settings.recheck_delay_minutes * 60000).toISOString();
      state.rounds.push({
        id: String(runs), settings: structuredClone(state.settings), started: new Date().toISOString(), trigger: 'manual', status: 'running',
        items: state.credentials.flatMap(c => [
          { credential_id: c.id, name: c.name, check_index: 1, model: state.settings.model, status: 'completed', prediction: 'gpt-6-astra', record_id: 'first-' + c.id },
          { credential_id: c.id, name: c.name, check_index: 2, model: state.settings.model, status: 'queued', due_at: state.next_run }
        ])
      });
      return route.fulfill({ json: { round_id: String(runs) } });
    }
    if (path === '/stop') {
      state.running = false;
      delete state.next_run;
      const round = state.rounds.at(-1);
      round.status = 'stopped';
      for (const item of round.items) if (item.status === 'queued') item.status = 'cancelled';
      return route.fulfill({ json: { stopping: true } });
    }
    if (path === '/state') return route.fulfill({ json: state });
    return route.fulfill({ status: 404, json: { error: 'unexpected mock route' } });
  });
  return { state, runs: () => runs };
}

async function layoutFits(page) {
  const layout = await page.evaluate(() => ({
    width: innerWidth, scroll: document.documentElement.scrollWidth,
    controls: [...document.querySelectorAll('.controls > *')].filter(e => e.getClientRects().length).map(e => {
      const r = e.getBoundingClientRect();
      return { left: r.left, right: r.right, top: r.top, bottom: r.bottom };
    })
  }));
  expect(layout.scroll).toBeLessThanOrEqual(layout.width);
  for (const r of layout.controls) {
    expect(r.left).toBeGreaterThanOrEqual(0);
    expect(r.right).toBeLessThanOrEqual(layout.width);
  }
  for (let i = 0; i < layout.controls.length; i++) for (let j = i + 1; j < layout.controls.length; j++) {
    const a = layout.controls[i], b = layout.controls[j];
    expect(a.right <= b.left || b.right <= a.left || a.bottom <= b.top || b.bottom <= a.top).toBe(true);
  }
}

for (const viewport of [{ name: 'desktop', width: 1240, height: 860 }, { name: 'mobile', width: 390, height: 844 }]) {
  test(`recheck mode configuration and two-check history on ${viewport.name}`, async ({ page }) => {
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    await page.setViewportSize(viewport);
    const mock = await mockGuard(page);
    await page.goto(ui);
    await page.locator('#key').fill('synthetic-ui-key');
    await page.locator('#login button').click();
    await expect(page.locator('#connection')).toHaveText('已连接');
    await expect(page.getByRole('radio', { name: '常规', exact: true })).toBeChecked();
    await page.getByRole('radio', { name: '二次检查', exact: true }).check();
    await expect(page.locator('#recheck-delay')).toHaveValue('10');
    await expect(page.locator('#schedule-interval-field')).toBeHidden();
    await page.locator('#recheck-delay').fill('2');
    await page.locator('#model').selectOption('gpt-6.1-sol');
    await page.getByRole('button', { name: '保存设置', exact: true }).click();
    await expect(page.locator('#notice')).toHaveText('设置已保存');
    expect(mock.runs()).toBe(0);
    expect(mock.state.settings.recheck_enabled).toBe(true);
    expect(mock.state.settings.schedule_enabled).toBe(false);
    expect(mock.state.settings.recheck_delay_minutes).toBe(2);
    await page.reload();
    await expect(page.getByRole('radio', { name: '二次检查', exact: true })).toBeChecked();
    await expect(page.locator('#recheck-delay')).toHaveValue('2');
    await layoutFits(page);
    await page.getByRole('button', { name: '立即检测', exact: true }).click();
    await expect(page.locator('#round-state')).toHaveText('巡检进行中');
    await expect(page.locator('#next')).toContainText('下次复查');
    await expect(page.locator('#accounts tr').first().locator('td').nth(3)).toHaveText('完成');
    await expect(page.locator('#accounts tr').first().locator('td').nth(4)).toContainText('排队');
    await expect(page.locator('#accounts tr').first().locator('td').nth(4)).toContainText('到 ');
    await expect(page.getByRole('button', { name: '立即检测', exact: true })).toBeDisabled();
    expect(mock.runs()).toBe(1);
    await page.screenshot({ path: `test-output/recheck-${viewport.name}-accounts.png`, fullPage: true });
    await page.getByRole('tab', { name: '运行记录', exact: true }).click();
    await page.locator('#history summary').click();
    await expect(page.locator('#history tbody tr')).toHaveCount(4);
    await expect(page.locator('#history tbody tr td:first-child').filter({ hasText: /^首检$/ })).toHaveCount(2);
    await expect(page.locator('#history tbody tr td:first-child').filter({ hasText: /^二次检查$/ })).toHaveCount(2);
    await layoutFits(page);
    await page.screenshot({ path: `test-output/recheck-${viewport.name}-history.png`, fullPage: true });
    await page.getByRole('radio', { name: '定时', exact: true }).check();
    await page.getByRole('button', { name: '保存设置', exact: true }).click();
    await expect(page.locator('#notice')).toHaveText('设置已保存');
    expect(mock.state.settings.recheck_enabled).toBe(false);
    expect(mock.state.settings.schedule_enabled).toBe(true);
    await expect(page.locator('#next')).toContainText('下次复查');
    await page.getByRole('button', { name: '停止', exact: true }).click();
    await expect(page.locator('#round-state')).toHaveText('待命');
    await expect(page.locator('#history summary .badge')).toHaveText('已停止');
    await expect(page.getByRole('button', { name: '停止', exact: true })).toBeDisabled();
    expect(errors).toEqual([]);
  });
}

test('legacy history without a check index is displayed as a first check', async ({ page }) => {
  const mock = await mockGuard(page);
  mock.state.rounds.push({
    id: 'legacy', settings: mock.state.settings, started: new Date().toISOString(), trigger: 'manual', status: 'completed',
    items: [{ credential_id: '0', name: 'legacy-account.json', model: 'gpt-6.1-sol', status: 'completed', prediction: 'gpt-6-astra' }]
  });
  await page.goto(ui);
  await page.locator('#key').fill('synthetic-ui-key');
  await page.locator('#login button').click();
  await expect(page.locator('#connection')).toHaveText('已连接');
  await expect(page.locator('#accounts tr').first().locator('td').nth(3)).toHaveText('完成');
  await expect(page.locator('#accounts tr').first().locator('td').nth(4)).toHaveText('—');
  await page.getByRole('tab', { name: '运行记录', exact: true }).click();
  await page.locator('#history summary').click();
  await expect(page.locator('#history tbody tr td:first-child')).toHaveText('首检');
});
