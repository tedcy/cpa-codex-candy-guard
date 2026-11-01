import { test, expect } from '@playwright/test';

const uiPath = '/v0/resource/plugins/cpa-codex-candy-guard/ui';
const statePath = '/v0/management/plugins/cpa-codex-candy-guard/state';
const keyStore = 'cpa-codex-candy-guard.key';
const otherKeyStore = 'cpa-codex-candy-eval.key';
const panelStore = 'cli-proxy-auth';
const fixtureKey = 'synthetic-guard-key';
const fixtureState = {
  settings: { model: '', per_credential_concurrency: 3, credential_concurrency: 2, interval_minutes: 10, schedule_enabled: false, scope: 'all_enabled', credential_ids: [] },
  credentials: [], models: ['gpt-6.1-sol'], rounds: [], running: false, ready: true
};

async function seed(page, { sessionKey, panel, format = 'plain' } = {}) {
  await page.addInitScript(({ sessionKey, panel, format, keyStore, otherKeyStore, panelStore }) => {
    sessionStorage.setItem(otherKeyStore, 'synthetic-other-plugin-key');
    if (sessionKey !== undefined) sessionStorage.setItem(keyStore, sessionKey);
    if (panel === undefined) return;
    let raw = typeof panel === 'string' ? panel : JSON.stringify(panel);
    if (format !== 'plain') {
      const scope = location.host + (format === 'v1' ? '|' + navigator.userAgent : '');
      const mask = new TextEncoder().encode('cli-proxy-api-webui::secure-storage|' + scope);
      const bytes = new TextEncoder().encode(raw).map((value, i) => value ^ mask[i % mask.length]);
      raw = `enc::${format}::` + btoa(String.fromCharCode(...bytes));
    }
    localStorage.setItem(panelStore, raw);
  }, { sessionKey, panel, format, keyStore, otherKeyStore, panelStore });
}

async function mockState(page, expectedKey = fixtureKey) {
  const requests = [];
  await page.route('**' + statePath, async route => {
    const authorization = route.request().headers().authorization;
    requests.push(authorization);
    await route.fulfill({ status: authorization === 'Bearer ' + expectedKey ? 200 : 401, json: fixtureState });
  });
  return requests;
}

async function login(page, value = fixtureKey) {
  await page.locator('#key').fill(value);
  await page.locator('#login button').click();
}

async function connected(page) {
  await expect(page.locator('#connection')).toHaveText('已连接');
  await expect(page.locator('#login')).toBeHidden();
}

async function cachedKey(page) {
  return page.evaluate(keyStore => sessionStorage.getItem(keyStore), keyStore);
}

for (const format of ['plain', 'v1', 'v2']) {
  test(`prefers the panel key from ${format} storage`, async ({ page }) => {
    const panelKey = 'synthetic-panel-key';
    await seed(page, { sessionKey: fixtureKey, panel: { state: { managementKey: ` ${panelKey} ` } }, format });
    const requests = await mockState(page, panelKey);
    await page.goto(uiPath);
    await connected(page);
    expect(requests).toEqual(['Bearer ' + panelKey]);
    expect(await cachedKey(page)).toBe(fixtureKey);
    expect(await page.evaluate(otherKeyStore => sessionStorage.getItem(otherKeyStore), otherKeyStore)).toBe('synthetic-other-plugin-key');
  });
}

test('manual login survives reload and returning to the same tab only', async ({ page, context }) => {
  await seed(page);
  await mockState(page);
  await page.goto(uiPath);
  await expect(page.locator('#login')).toBeVisible();
  await login(page, ` ${fixtureKey} `);
  await connected(page);
  await expect.poll(() => cachedKey(page)).toBe(fixtureKey);
  expect(await page.evaluate(() => localStorage.length)).toBe(0);
  expect(await page.locator('#key').inputValue()).toBe('');
  await page.reload();
  await connected(page);
  await page.goto('about:blank');
  await page.goto(uiPath);
  await connected(page);
  const freshTab = await context.newPage();
  await freshTab.goto(uiPath);
  await expect(freshTab.locator('#login')).toBeVisible();
  await freshTab.close();
});

for (const panel of ['broken-json', { state: { managementKey: 123 } }]) {
  test(`falls back to its session key for ${typeof panel === 'string' ? 'invalid JSON' : 'a non-string panel key'}`, async ({ page }) => {
    await seed(page, { sessionKey: fixtureKey, panel });
    const requests = await mockState(page);
    await page.goto(uiPath);
    await connected(page);
    expect(requests).toEqual(['Bearer ' + fixtureKey]);
  });
}

test('does not read the other plugin session key', async ({ page }) => {
  await page.clock.install();
  await seed(page);
  const requests = await mockState(page);
  await page.goto(uiPath);
  await expect(page.locator('#login')).toBeVisible();
  await page.clock.runFor(12000);
  expect(requests).toEqual([]);
});

test('only caches a manually entered key after verification succeeds', async ({ page }) => {
  let status = 401;
  await page.route('**' + statePath, route => route.fulfill({ status, json: status === 200 ? fixtureState : { error: 'synthetic failure' } }));
  await page.goto(uiPath);
  await login(page);
  await expect(page.locator('#notice')).toHaveText('管理密钥无效或未登录');
  expect(await cachedKey(page)).toBeNull();
  status = 500;
  await login(page);
  await expect(page.locator('#notice')).toHaveText('synthetic failure');
  expect(await cachedKey(page)).toBeNull();
  status = 200;
  await login(page);
  await connected(page);
  await expect.poll(() => cachedKey(page)).toBe(fixtureKey);
  expect(await page.evaluate(() => localStorage.length)).toBe(0);
});

test('a current 401 clears only its session key and stops polling', async ({ page }) => {
  await page.clock.install();
  await seed(page, { sessionKey: fixtureKey, panel: { state: { rememberPassword: false } } });
  let requests = 0, unauthorized = false;
  await page.route('**' + statePath, route => {
    requests++;
    return route.fulfill({ status: unauthorized ? 401 : 200, json: fixtureState });
  });
  await page.goto(uiPath);
  await connected(page);
  const before = await page.evaluate(panelStore => localStorage.getItem(panelStore), panelStore);
  unauthorized = true;
  await page.locator('#refresh').click();
  await expect(page.locator('#notice')).toHaveText('管理密钥无效或未登录');
  await expect(page.locator('#login')).toBeVisible();
  expect(await cachedKey(page)).toBeNull();
  const count = requests;
  await page.clock.runFor(12000);
  expect(requests).toBe(count);
  expect(await page.evaluate(panelStore => localStorage.getItem(panelStore), panelStore)).toBe(before);
  expect(await page.evaluate(otherKeyStore => sessionStorage.getItem(otherKeyStore), otherKeyStore)).toBe('synthetic-other-plugin-key');
});

for (const newKey of ['synthetic-new-key', fixtureKey]) {
  test(`an older 401 cannot clear a newer login with ${newKey === fixtureKey ? 'the same' : 'a different'} key`, async ({ page }) => {
    await page.clock.install();
    await seed(page, { sessionKey: fixtureKey });
    let phase = 'initial', oldRequests = 0, release;
    const delayed = new Promise(resolve => { release = resolve; });
    await page.route('**' + statePath, async route => {
      if (phase === 'old') {
        if (++oldRequests === 1) await delayed;
        return route.fulfill({ status: 401, json: { error: 'unauthorized' } });
      }
      return route.fulfill({ json: fixtureState });
    });
    try {
      await page.goto(uiPath);
      await connected(page);
      phase = 'old';
      await page.locator('#refresh').click();
      await expect.poll(() => oldRequests).toBe(1);
      await page.locator('#refresh').click();
      await expect(page.locator('#login')).toBeVisible();
      phase = 'new';
      await login(page, newKey);
      await connected(page);
      await expect.poll(() => cachedKey(page)).toBe(newKey);
      const oldResponse = page.waitForResponse(response => response.url().endsWith(statePath) && response.status() === 401);
      release();
      await (await oldResponse).finished();
      await page.clock.runFor(4000);
      await connected(page);
      expect(await cachedKey(page)).toBe(newKey);
    } finally { release(); }
  });
}

test('an older successful response cannot hide a current login prompt', async ({ page }) => {
  await page.clock.install();
  await seed(page, { sessionKey: fixtureKey });
  let requests = 0, release;
  const delayed = new Promise(resolve => { release = resolve; });
  await page.route('**' + statePath, async route => {
    const request = ++requests;
    if (request === 1) await delayed;
    return route.fulfill({ status: request === 1 ? 200 : 401, json: fixtureState });
  });
  try {
    await page.goto(uiPath);
    await expect.poll(() => requests).toBe(1);
    await page.locator('#refresh').click();
    await expect(page.locator('#notice')).toHaveText('管理密钥无效或未登录');
    const oldResponse = page.waitForResponse(response => response.url().endsWith(statePath));
    release();
    await (await oldResponse).finished();
    await page.clock.runFor(4000);
    await expect(page.locator('#login')).toBeVisible();
    expect(await cachedKey(page)).toBeNull();
    expect(requests).toBe(2);
  } finally { release(); }
});

for (const failure of ['500', 'failed', 'timedout']) {
  test(`a ${failure} response preserves the temporary login`, async ({ page }) => {
    await seed(page, { sessionKey: fixtureKey });
    let fail = false;
    await page.route('**' + statePath, route => {
      if (!fail) return route.fulfill({ json: fixtureState });
      return failure === '500' ? route.fulfill({ status: 500, json: { error: 'synthetic failure' } }) : route.abort(failure);
    });
    await page.goto(uiPath);
    await connected(page);
    fail = true;
    await page.locator('#refresh').click();
    await expect(page.locator('#connection')).toHaveText('未连接');
    await expect(page.locator('#login')).toBeHidden();
    expect(await cachedKey(page)).toBe(fixtureKey);
    fail = false;
    await page.locator('#refresh').click();
    await connected(page);
  });
}

test('blocked browser storage falls back to memory without crashing', async ({ page }) => {
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.addInitScript(() => {
    for (const name of ['localStorage', 'sessionStorage']) {
      Object.defineProperty(window, name, { get() { throw new DOMException('Storage blocked', 'SecurityError'); } });
    }
  });
  await mockState(page);
  await page.goto(uiPath);
  await expect(page.locator('#login')).toBeVisible();
  await login(page);
  await connected(page);
  await page.locator('#refresh').click();
  await connected(page);
  await page.route('**' + statePath, route => route.fulfill({ status: 401, json: { error: 'unauthorized' } }));
  await page.locator('#refresh').click();
  await expect(page.locator('#login')).toBeVisible();
  await page.reload();
  await expect(page.locator('#login')).toBeVisible();
  expect(errors).toEqual([]);
});
