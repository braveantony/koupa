import { test, expect, Page } from '@playwright/test';
import { spawn } from 'node:child_process';

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
  await row.getByRole('button', { name: '完成' }).click();
  await expect(row).toHaveCount(0);

  await expect(board.locator('#latest')).toContainText(`請 ${number} 號取餐 🧋`);
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

test('board EventSource fires shutdown when the app gets SIGTERM', async ({ page }) => {
  const port = 18081;
  const app = spawn(process.env.KOUPA_BIN ?? '../../bin/koupa', [], {
    env: {
      ...process.env,
      LISTEN_ADDR: `127.0.0.1:${port}`,
      POD_UID: 'pw-shutdown',
      POD_NAME: 'pw-shutdown',
      GRACEFUL_SHUTDOWN: '1',
    },
    stdio: 'ignore',
  });
  try {
    const base = `http://127.0.0.1:${port}`;
    await expect
      .poll(async () => (await fetch(`${base}/healthz`).catch(() => null))?.status, { timeout: 15_000 })
      .toBe(200);
    await page.goto(`${base}/board`);
    await expect(page.locator('#version')).toContainText('服務版本：v');
    app.kill('SIGTERM');
    await expect
      .poll(() => page.evaluate(() => (window as any).__events.filter((e: string) => e === 'shutdown').length), {
        timeout: 15_000,
      })
      .toBeGreaterThan(0);
    await expect(page.locator('#status')).toContainText('重新連線');
  } finally {
    app.kill('SIGKILL');
  }
});
