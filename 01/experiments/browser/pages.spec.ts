import { test, expect, Page } from '@playwright/test';
import { spawn } from 'node:child_process';
import { createServer } from 'node:net';

const NUMBER = /\b\d{4}\b/;

async function placeOrder(page: Page): Promise<string> {
  await page.goto('/');
  await page.locator('#menu li[data-name="滾動珍珠奶茶"] input').fill('2');
  await expect(page.locator('#total')).toHaveText('130');
  await page.locator('#submit').click();
  const result = page.locator('#result');
  await expect(result).toContainText(NUMBER);
  return (await result.innerText()).match(NUMBER)![0];
}

test('order, kitchen done, board calls the number', async ({ browser, page }) => {
  const board = await browser.newPage();
  await board.goto('/board');
  await expect(board.locator('#version')).toContainText('服務版本：v');

  const number = await placeOrder(page);

  const kitchen = await browser.newPage();
  await kitchen.goto('/kitchen');
  const row = kitchen.locator(`#orders li[data-number="${number}"]`);
  await expect(row).toBeVisible();
  await expect(row).toContainText('滾動珍珠奶茶 x2');
  await row.getByRole('button', { name: '完成' }).click();
  await expect(row).toHaveCount(0);

  await expect(board.locator('#latest')).toContainText(`請 ${number} 號取餐 🧋`);
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
  await page.locator('#menu li[data-name="容器冬瓜茶"] input').fill('1');
  await page.locator('#submit').click();
  await expect(page.locator('#result')).toContainText(NUMBER);
  expect(bodies).toHaveLength(2);
  expect(bodies[1]).toBe(bodies[0]);
  expect(JSON.parse(bodies[0]).client_ref).toMatch(/^[0-9a-f-]{36}$/);
});

test('v2: sold-out item is disabled on the order page', async ({ page, request }) => {
  const menu = await (await request.get('/api/menu')).json();
  const tea = menu.find((m: any) => m.name === 'Pod 沫紅茶');
  const kitchen = await page.context().newPage();
  await kitchen.goto('/kitchen');
  const toggle = kitchen.locator(`#menu li[data-id="${tea.id}"] button`);
  await toggle.click();
  try {
    await page.goto('/');
    await expect(page.locator(`#menu li[data-id="${tea.id}"] input`)).toBeDisabled();
    await expect(page.locator(`#menu li[data-id="${tea.id}"]`)).toHaveClass(/sold-out/);
  } finally {
    await request.post(`/api/menu/${tea.id}/available`, { data: { available: true } });
  }
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
    await page.goto(`${base}/board`);
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
