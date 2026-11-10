import { test, expect, Page, APIRequestContext } from '@playwright/test';
import { randomUUID } from 'node:crypto';
import { spawn } from 'node:child_process';
import { createServer } from 'node:net';

const NUMBER = /\b\d{4}\b/;

async function placeOrder(page: Page): Promise<string> {
  await page.goto('/');
  const plus = page.locator('#menu li[data-name="波霸奶茶"] button.inc');
  await plus.click();
  await plus.click();
  await expect(page.locator('#total')).toHaveText('130');
  await expect(page.locator('#cups')).toHaveText('2');
  await page.locator('#submit').click();
  const result = page.locator('#result');
  await expect(result).toBeVisible();
  await expect(result).toContainText('波霸奶茶 × 2');
  await expect(result).toContainText('做好會在店內的叫號螢幕叫號');
  const ticket = page.locator('#ticket');
  await expect(ticket).toHaveText(NUMBER);
  return (await ticket.innerText()).match(NUMBER)![0];
}

test('order, kitchen done, board calls the number', async ({ browser, page }) => {
  const board = await browser.newPage();
  await board.goto('/board?debug=1');
  await expect(board.locator('#version')).toContainText('服務版本：v');

  const number = await placeOrder(page);
  await expect(board.locator(`#making li[data-number="${number}"]`)).toBeVisible();

  const kitchen = await browser.newPage();
  await kitchen.goto('/kitchen');
  const row = kitchen.locator(`#orders li[data-number="${number}"]`);
  await expect(row).toBeVisible();
  await expect(row).toContainText('波霸奶茶 × 2');
  await row.getByRole('button', { name: '完成' }).click();
  await expect(row).toHaveCount(0);

  await expect(board.locator('#latest')).toContainText(`請 ${number} 號取餐 🧋`);
  await expect(board.locator('#latest .big')).toHaveText(number);
  await expect(board.locator(`#making li[data-number="${number}"]`)).toHaveCount(0);
});

test('order page retries once with the same client_ref and body', async ({ page }) => {
  const bodies: string[] = [];
  let first = true;
  await page.route('**/orders', async (route) => {
    if (route.request().method() !== 'POST') return route.continue();
    bodies.push(route.request().postData() ?? '');
    if (first) {
      first = false;
      return route.abort('connectionreset');
    }
    return route.continue();
  });
  await page.goto('/');
  await page.locator('#menu li[data-name="冬瓜檸檬"] button.inc').click();
  await page.locator('#submit').click();
  await expect(page.locator('#ticket')).toHaveText(NUMBER);
  expect(bodies).toHaveLength(2);
  expect(bodies[1]).toBe(bodies[0]);
  expect(JSON.parse(bodies[0]).client_ref).toMatch(/^[0-9a-f-]{36}$/);
});

test('v2: sold-out item is disabled on the order page', async ({ page, request }) => {
  const menu = await (await request.get('/api/menu')).json();
  const tea = menu.find((m: any) => m.name === '晚霞紅茶');
  const kitchen = await page.context().newPage();
  await kitchen.goto('/kitchen');
  const toggle = kitchen.locator(`#menu li[data-id="${tea.id}"] button`);
  await toggle.click();
  try {
    await page.goto('/');
    const card = page.locator(`#menu li[data-id="${tea.id}"]`);
    await expect(card.locator('button.inc')).toBeDisabled();
    await expect(card.locator('button.dec')).toBeDisabled();
    await expect(card).toHaveClass(/sold-out/);
    await expect(card).toContainText('今日售完');
  } finally {
    await request.post(`/api/menu/${tea.id}/available`, { data: { available: true } });
  }
});

test('order page shows a photo per drink; debug info only with ?debug=1', async ({ page }) => {
  await page.goto('/');
  const cards = page.locator('#menu li');
  await expect(cards).toHaveCount(4);
  for (const name of ['波霸奶茶', '晚霞紅茶', '綠茶三劍客', '冬瓜檸檬']) {
    const img = page.locator(`#menu li[data-name="${name}"] img`);
    await expect(img).toHaveAttribute('alt', name);
    await img.scrollIntoViewIfNeeded();
    await expect.poll(() => img.evaluate((e: HTMLImageElement) => e.complete && e.naturalWidth)).toBe(800);
  }
  await expect(page.locator('#debug')).toBeHidden();
  await page.goto('/?debug=1');
  await expect(page.locator('#debug')).toBeVisible();
  await expect(page.locator('#version')).toContainText('服務版本：v');
  await expect(page.locator('#pod')).toContainText('Pod：');
});

async function apiOrder(request: APIRequestContext, menuId = 1): Promise<{ order_id: number; number: string }> {
  const r = await request.post('/orders', {
    data: { client_ref: randomUUID(), items: [{ menu_item_id: menuId, quantity: 1 }] },
  });
  expect(r.ok()).toBeTruthy();
  return r.json();
}

test('order page locks the steppers while an order outcome is unknown', async ({ page }) => {
  const bodies: string[] = [];
  let fail = true;
  await page.route('**/orders', async (route) => {
    if (route.request().method() !== 'POST') return route.continue();
    bodies.push(route.request().postData() ?? '');
    return fail ? route.abort('connectionreset') : route.continue();
  });
  await page.goto('/');
  const card = page.locator('#menu li[data-name="波霸奶茶"]');
  await card.locator('button.inc').click();
  await page.locator('#submit').click();
  // Both attempts failed: the outcome is unknown and the order stays pending.
  await expect(page.locator('#notice')).toContainText('同一張訂單（波霸奶茶 × 1）');
  expect(bodies).toHaveLength(2);
  await expect(card.locator('button.inc')).toBeDisabled();
  await expect(card.locator('button.dec')).toBeDisabled();
  await expect(page.locator('#menu li[data-name="綠茶三劍客"] button.inc')).toBeDisabled();
  await expect(page.locator('#submit')).toBeEnabled();
  await expect(page.locator('#total')).toHaveText('65');
  fail = false;
  await page.locator('#submit').click();
  await expect(page.locator('#ticket')).toHaveText(NUMBER);
  expect(bodies).toHaveLength(3);
  expect(bodies[2]).toBe(bodies[0]);
  await page.locator('#again').click();
  await expect(card.locator('button.inc')).toBeEnabled();
});

test('kitchen sends 完成 once even if the list redraws while it is in flight', async ({ page, request }) => {
  const a = await apiOrder(request);
  let posts = 0;
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  await page.route('**/orders/*/ready', async (route) => {
    posts++;
    await gate;
    await route.continue();
  });
  await page.goto('/kitchen');
  const row = page.locator(`#orders li[data-order-id="${a.order_id}"]`);
  const done = row.getByRole('button', { name: '完成' });
  await done.click();
  // A new order changes the list, so the next poll redraws every card.
  const b = await apiOrder(request);
  await expect(page.locator(`#orders li[data-order-id="${b.order_id}"]`)).toBeVisible();
  await expect(done).toBeDisabled();
  await done.click({ force: true }).catch(() => {});
  release();
  await expect(row).toHaveCount(0);
  expect(posts).toBe(1);
});

test('board: a poll that started before a call does not bring the number back', async ({ page, request }) => {
  await page.goto('/board');
  await page.waitForFunction(() => (window as any).__events.includes('hello'));
  const o = await apiOrder(request);
  let captured!: () => void;
  const gotSnapshot = new Promise<void>((r) => (captured = r));
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  let hold = true;
  await page.route('**/api/orders?status=pending*', async (route) => {
    const resp = await route.fetch();
    const body = await resp.text();
    if (hold && body.includes(`"order_id":${o.order_id}`)) {
      hold = false;
      captured();
      await gate; // the SSE ready event arrives while this response is held
    }
    await route.fulfill({ response: resp, body });
  });
  await gotSnapshot;
  await request.post(`/orders/${o.order_id}/ready`);
  await expect(page.locator('#latest .big')).toHaveText(o.number);
  release();
  const stale = page.locator(`#making li[data-number="${o.number}"]`);
  // Sample (no auto-retry) while the held response renders and the next
  // poll runs: the called number must never be back under 製作中.
  for (let i = 0; i < 30; i++) {
    expect(await stale.count()).toBe(0);
    await page.waitForTimeout(100);
  }
});

test('board ignores a ready event whose id is not newer than the last one', async ({ page }) => {
  const ev = (seq: number, n: string) =>
    `event: ready\nid: ${seq}\ndata: {"number":"${n}","order_id":${9000 + seq}}\n\n`;
  const body =
    'retry: 60000\n\n' +
    'event: hello\nid: 0\ndata: {"version":"v2"}\n\n' +
    ev(4, '0104') + ev(5, '0105') + ev(5, '0105') + ev(3, '0103');
  await page.route('**/events', (route) =>
    route.fulfill({ status: 200, headers: { 'Content-Type': 'text/event-stream' }, body }),
  );
  await page.goto('/board');
  await expect
    .poll(() => page.evaluate(() => (window as any).__events.filter((e: string) => e === 'ready-ignored').length))
    .toBe(2);
  await expect(page.locator('#latest .big')).toHaveText('0105');
  await expect(page.locator('#list li')).toHaveText(['0104']);
});

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = createServer();
    srv.listen(0, '127.0.0.1', () => {
      const { port } = srv.address() as { port: number };
      srv.close(() => resolve(port));
    });
    srv.on('error', reject);
  });
}

test('board EventSource fires shutdown when the app gets SIGTERM', async ({ page }) => {
  const port = await freePort();
  const uid = `pw-shutdown-${process.pid}-${port}`;
  const app = spawn(process.env.KOUPA_BIN ?? '../../bin/koupa', [], {
    env: {
      ...process.env,
      LISTEN_ADDR: `127.0.0.1:${port}`,
      POD_UID: uid,
      POD_NAME: uid,
      GRACEFUL_SHUTDOWN: '1',
    },
    stdio: 'ignore',
  });
  // A spawn error (e.g. no binary) emits 'error' and maybe never 'exit':
  // resolve on either, so neither the test nor the cleanup waits forever.
  let spawnError: Error | undefined;
  const exited = new Promise<number | null>((resolve) => {
    app.on('exit', (code) => resolve(code));
    app.on('error', (e) => {
      spawnError = e;
      resolve(null);
    });
  });
  try {
    const base = `http://127.0.0.1:${port}`;
    // Ready only when OUR child answers: the pod uid proves it bound this port.
    await expect
      .poll(
        async () => {
          if (spawnError) throw new Error(`cannot start the app: ${spawnError.message}`);
          if (app.exitCode !== null) throw new Error(`app exited (${app.exitCode}) before it was ready`);
          const r = await fetch(`${base}/api/whoami`).catch(() => null);
          return r?.ok ? (await r.json()).pod_uid : null;
        },
        { timeout: 15_000 },
      )
      .toBe(uid);
    await page.goto(`${base}/board?debug=1`);
    await expect(page.locator('#version')).toContainText('服務版本：v');
    app.kill('SIGTERM');
    await expect
      .poll(() => page.evaluate(() => (window as any).__events.filter((e: string) => e === 'shutdown').length), {
        timeout: 15_000,
      })
      .toBeGreaterThan(0);
    await expect(page.locator('#status')).toContainText('重新連線');
    expect(await exited).toBe(0);
    expect(spawnError).toBeUndefined();
  } finally {
    if (app.exitCode === null && !spawnError) app.kill('SIGKILL');
    await exited;
  }
});
