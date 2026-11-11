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

  await expect(board.locator('#latest')).toContainText(`請 ${number} 號取餐`);
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

test('order page works without crypto.randomUUID (plain HTTP, not localhost)', async ({ page }) => {
  // Over http://<Gateway IP> the page is not a secure context and
  // crypto.randomUUID does not exist. Simulate that here.
  await page.addInitScript(() => {
    delete (Crypto.prototype as any).randomUUID;
    delete (crypto as any).randomUUID;
  });
  const errors: string[] = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const bodies: string[] = [];
  const attempts: string[] = [];
  let first = true;
  await page.route('**/orders', async (route) => {
    if (route.request().method() !== 'POST') return route.continue();
    bodies.push(route.request().postData() ?? '');
    attempts.push(route.request().headers()['x-koupa-attempt'] ?? '');
    if (first) {
      first = false;
      return route.abort('connectionreset');
    }
    return route.continue();
  });
  await page.goto('/');
  expect(await page.evaluate(() => typeof (crypto as any).randomUUID)).toBe('undefined');
  await page.locator('#menu li[data-name="綠茶三劍客"] button.inc').click();
  await page.locator('#submit').click();
  await expect(page.locator('#ticket')).toHaveText(NUMBER);
  expect(errors).toEqual([]);
  expect(bodies).toHaveLength(2);
  expect(bodies[1]).toBe(bodies[0]);
  const v4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
  expect(JSON.parse(bodies[0]).client_ref).toMatch(v4);
  for (const a of attempts) expect(a).toMatch(v4);
  expect(attempts[1]).not.toBe(attempts[0]);
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

// The kitchen's sold-out panel follows the MAJOR of the build version (the
// semver image tag), not an exact "v2".
test('kitchen shows the sold-out panel from MAJOR version 2 on', async ({ page }) => {
  for (const [version, shown] of [['v2.2.0', true], ['v2', true], ['v3.0.0', true],
    ['v1.2.0', false], ['v1', false], ['dev', false]] as const) {
    let menuAsked = 0;
    await page.unrouteAll();
    await page.route('**/api/whoami', (r) =>
      r.fulfill({ json: { pod_uid: 'pw-uid', pod_name: 'pw-pod', version } }));
    await page.route('**/api/menu', (r) => { menuAsked++; return r.continue(); });
    await page.goto('/kitchen?debug=1');
    await expect(page.locator('#version')).toHaveText('服務版本：' + version);
    if (shown) {
      await expect(page.locator('#supply'), version).toBeVisible();
      await expect(page.locator('#menu li')).toHaveCount(4);
    } else {
      await page.waitForTimeout(300);
      await expect(page.locator('#supply'), version).toBeHidden();
      expect(menuAsked, version + ': /api/menu asked').toBe(0);
    }
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
  await expect(page.locator('#notice')).toContainText('會重新送出同一張訂單（波霸奶茶 × 1）');
  // Only step 5 (IDEMPOTENCY_KEY) dedupes on the server; the page must not promise it.
  await expect(page.locator('#notice')).not.toContainText('不會重複');
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

// The board's debug strip names the Pod holding its SSE stream: it comes
// from each hello, so a one-off /api/whoami answer (maybe from another Pod)
// never shows there.
test('board debug strip takes the Pod from hello, not from whoami', async ({ page, request }) => {
  const real = (await (await request.get('/api/whoami')).json()).pod_name;
  expect(real).toBeTruthy();
  await page.route('**/api/whoami', (r) =>
    r.fulfill({ json: { pod_uid: 'stale-uid', pod_name: 'stale-pod', version: 'v0.0.0' } }));
  await page.goto('/board?debug=1');
  await expect(page.locator('#pod')).toHaveText('Pod：' + real);
  await page.waitForTimeout(500);
  await expect(page.locator('#pod')).toHaveText('Pod：' + real);
  await expect(page.locator('#version')).not.toContainText('v0.0.0');
});

test('board debug strip follows the Pod after the SSE stream reconnects', async ({ page }) => {
  const hello = (pod: string, version: string) =>
    'retry: 100\n\n' +
    `event: hello\nid: 0\ndata: {"version":"${version}","pod_name":"${pod}","pod_uid":"uid-${pod}"}\n\n`;
  let release!: () => void;
  const gate = new Promise<void>((r) => { release = r; });
  let n = 0;
  await page.route('**/events', async (route) => {
    // The first stream ends right after hello; the browser reconnects and
    // the next one lands on "another Pod".
    const first = n++ === 0;
    if (!first) await gate;
    await route.fulfill({
      status: 200,
      headers: { 'Content-Type': 'text/event-stream' },
      body: first ? hello('pod-a', 'v2.2.1') : hello('pod-b', 'v2.2.2'),
    });
  });
  await page.goto('/board?debug=1');
  await expect(page.locator('#pod')).toHaveText('Pod：pod-a');
  await expect(page.locator('#version')).toHaveText('服務版本：v2.2.1');
  release();
  await expect(page.locator('#pod')).toHaveText('Pod：pod-b');
  await expect(page.locator('#version')).toHaveText('服務版本：v2.2.2');
});

// Board 製作中 on a fixed-height screen: every pending order is either a
// fully visible strip or counted in the line 「還有 N 張」 (#making-more),
// which is itself fully visible.
const fakeOrders = (n: number) =>
  Array.from({ length: n }, (_, i) => ({ order_id: 900000 + i, number: String(5001 + i), total: 50, items: [] }));

async function makingState(page: Page) {
  return page.evaluate(() => {
    // Fractional clip box: the padding boxes of the list and the section.
    const clip = (e: Element) => {
      const r = e.getBoundingClientRect();
      const cs = getComputedStyle(e);
      return { top: r.top + parseFloat(cs.borderTopWidth), bottom: r.bottom - parseFloat(cs.borderBottomWidth) };
    };
    const ul = document.getElementById('making')!;
    const a = clip(ul);
    const b = clip(ul.parentElement!);
    const top = Math.max(a.top, b.top);
    const bottom = Math.min(a.bottom, b.bottom);
    const inView = (r: DOMRect) => r.top >= 0 && r.left >= 0 && r.bottom <= window.innerHeight && r.right <= window.innerWidth;
    const lis = Array.from(ul.children) as HTMLElement[];
    const more = document.getElementById('making-more')!;
    const mr = more.getBoundingClientRect();
    return {
      shown: lis.map((li) => li.dataset.number!),
      more: more.hidden ? null : more.textContent,
      moreVisible: !more.hidden && mr.height > 0 && inView(mr) && mr.top >= b.top && mr.bottom <= b.bottom,
      unclipped: lis.every((li) => {
        const r = li.getBoundingClientRect();
        return r.top >= top && r.bottom <= bottom;
      }),
      allVisible: lis.every((li) => {
        const r = li.getBoundingClientRect();
        return r.top >= top && r.bottom <= bottom && inView(r);
      }),
    };
  });
}

// The invariant, polled: shown strips are the first k orders, all fully
// visible; the rest (if any) are counted on a fully visible line.
async function expectAccounted(page: Page, total: number) {
  // Let a pending resize refit (one animation frame) run first.
  await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
  await expect
    .poll(async () => {
      const st = await makingState(page);
      const prefix = st.shown.every((n, i) => n === String(5001 + i));
      const counted = total - st.shown.length;
      const ok = prefix && st.allVisible &&
        (counted === 0 ? st.more === null : st.more === `還有 ${counted} 張` && st.moreVisible);
      return ok ? 'ok' : JSON.stringify(st);
    }, { timeout: 6000 })
    .toBe('ok');
  return makingState(page);
}

for (const [w, h] of [[800, 600], [1280, 720], [1920, 1080], [800, 360], [800, 220], [800, 200]]) {
  test(`board ${w}x${h}: no pending order is clipped out of sight`, async ({ page }) => {
    await page.setViewportSize({ width: w, height: h });
    let n = 200;
    await page.route('**/api/orders?status=pending*', (route) => route.fulfill({ json: fakeOrders(n) }));
    await page.goto('/board');
    const many = await expectAccounted(page, 200);
    expect(many.more).not.toBeNull();
    // Boundary: exactly the strips that fit next to the count line -> all
    // of them shown and no count line; one more -> still all accounted for.
    const k = many.shown.length;
    if (k > 0) {
      n = k;
      const full = await expectAccounted(page, k);
      expect(full.shown).toHaveLength(k);
      expect(full.more).toBeNull();
    }
    n = k + 1;
    await expectAccounted(page, k + 1);
  });
}

test('board 800x200: no strip fits, the count line alone counts every order', async ({ page }) => {
  await page.setViewportSize({ width: 800, height: 200 });
  await page.route('**/api/orders?status=pending*', (route) => route.fulfill({ json: fakeOrders(200) }));
  await page.goto('/board');
  const st = await expectAccounted(page, 200);
  expect(st.shown).toEqual([]);
  expect(st.more).toBe('還有 200 張');
});

test('board 800x150: too short to fit, the page scrolls and nothing is clipped', async ({ page }) => {
  await page.setViewportSize({ width: 800, height: 150 });
  await page.route('**/api/orders?status=pending*', (route) => route.fulfill({ json: fakeOrders(200) }));
  await page.goto('/board');
  const more = page.locator('#making-more');
  await expect(more).toHaveText('還有 140 張'); // 60 strips listed, the rest counted
  await expect(page.locator('#making li')).toHaveCount(60);
  expect(await page.evaluate(() => {
    const ul = document.getElementById('making')!;
    return ul.scrollHeight <= ul.clientHeight && document.documentElement.scrollHeight > innerHeight;
  })).toBe(true);
  await more.scrollIntoViewIfNeeded();
  await expect(more).toBeInViewport({ ratio: 1 });
});

// Fractional pixels: sizes come from vh, so sweeping the height moves the
// strips by fractions of a pixel against the clip edge.
test('board: the fit holds at every height from 600 to 640 px', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 600 });
  await page.route('**/api/orders?status=pending*', (route) => route.fulfill({ json: fakeOrders(200) }));
  await page.goto('/board');
  for (let h = 600; h <= 640; h++) {
    await page.setViewportSize({ width: 1280, height: h });
    await expectAccounted(page, 200);
  }
});

test('board: the fit holds at a fractional device pixel ratio', async ({ browser, baseURL }) => {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 600 }, deviceScaleFactor: 1.25, baseURL });
  const page = await ctx.newPage();
  await page.route('**/api/orders?status=pending*', (route) => route.fulfill({ json: fakeOrders(200) }));
  await page.goto('/board');
  for (let h = 600; h <= 620; h++) {
    await page.setViewportSize({ width: 1280, height: h });
    await expectAccounted(page, 200);
  }
  await ctx.close();
});

test('board: a strip clipped by less than 1 px is counted, not listed', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 720 });
  await page.route('**/api/orders?status=pending*', (route) => route.fulfill({ json: fakeOrders(4) }));
  await page.goto('/board');
  await expect(page.locator('#making li')).toHaveCount(4); // 2 rows of 2
  // Make the list box 0.3 px shorter than its strips, then refit.
  const h = await page.evaluate(() => {
    const ul = document.getElementById('making')!;
    const cs = getComputedStyle(ul);
    const last = ul.lastElementChild!.getBoundingClientRect();
    // overflow clips at the padding edge: end it 0.3 px above the strips' bottom
    return last.bottom - ul.getBoundingClientRect().top + parseFloat(cs.borderBottomWidth) - 0.3;
  });
  await page.addStyleTag({ content: `#making { flex: none !important; height: ${h}px !important; }` });
  await page.evaluate(() => window.dispatchEvent(new Event('resize')));
  await expect.poll(async () => (await makingState(page)).more).toBe('還有 2 張');
  const st = await expectAccounted(page, 4);
  expect(st.shown).toEqual(['5001', '5002']);
});

// Below 200 px tall the board scrolls: nothing is clipped, 60 strips are
// listed and the rest counted.
async function expectScrolling(page: Page, total: number) {
  await expect
    .poll(async () => {
      const st = await makingState(page);
      const ok = st.unclipped && st.shown.length === Math.min(60, total) &&
        (total > 60 ? st.more === `還有 ${total - 60} 張` : st.more === null);
      return ok ? 'ok' : JSON.stringify(st);
    }, { timeout: 6000 })
    .toBe('ok');
}

test('board 800x199: scroll layout, nothing clipped', async ({ page }) => {
  await page.setViewportSize({ width: 800, height: 199 });
  await page.route('**/api/orders?status=pending*', (route) => route.fulfill({ json: fakeOrders(200) }));
  await page.goto('/board');
  await expectScrolling(page, 200);
  await page.locator('#making-more').scrollIntoViewIfNeeded();
  await expect(page.locator('#making-more')).toBeInViewport({ ratio: 1 });
});

test('board: resize across the 200 px switch (250 -> 199 -> 250)', async ({ page }) => {
  await page.setViewportSize({ width: 800, height: 250 });
  await page.route('**/api/orders?status=pending*', (route) => route.fulfill({ json: fakeOrders(200) }));
  await page.goto('/board');
  const before = await expectAccounted(page, 200);
  await page.setViewportSize({ width: 800, height: 199 });
  await expectScrolling(page, 200);
  await page.evaluate(() => window.scrollTo(0, 0));
  await page.setViewportSize({ width: 800, height: 250 });
  await expect.poll(async () => (await makingState(page)).shown.length).toBe(before.shown.length);
  await expectAccounted(page, 200);
});

test('board: the 製作中 fit holds across resizes (large -> small -> large)', async ({ page }) => {
  await page.setViewportSize({ width: 1920, height: 1080 });
  await page.route('**/api/orders?status=pending*', (route) => route.fulfill({ json: fakeOrders(200) }));
  await page.goto('/board');
  const large = await expectAccounted(page, 200);
  await page.setViewportSize({ width: 800, height: 260 });
  await expect.poll(async () => (await makingState(page)).shown.length).toBeLessThan(large.shown.length);
  await expectAccounted(page, 200);
  await page.setViewportSize({ width: 1920, height: 1080 });
  // The refit runs on the next animation frame; wait for it, then check.
  await expect.poll(async () => (await makingState(page)).shown.length).toBe(large.shown.length);
  await expectAccounted(page, 200);
});

test('board: stale filtering still applies when 製作中 overflows', async ({ page, request }) => {
  await page.setViewportSize({ width: 800, height: 600 });
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
    const real = await resp.json();
    const all = [...real, ...fakeOrders(100)]; // real orders first, so o would be in view
    if (hold && real.some((x: any) => x.order_id === o.order_id)) {
      hold = false;
      captured();
      await gate; // the SSE ready event arrives while this response is held
    }
    await route.fulfill({ json: all });
  });
  await gotSnapshot;
  await request.post(`/orders/${o.order_id}/ready`);
  await expect(page.locator('#latest .big')).toHaveText(o.number);
  release();
  const stale = page.locator(`#making li[data-number="${o.number}"]`);
  for (let i = 0; i < 30; i++) {
    expect(await stale.count()).toBe(0);
    const st = await makingState(page);
    expect(st.allVisible).toBe(true);
    expect(st.moreVisible).toBe(true);
    await page.waitForTimeout(100);
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
    await page.goto(`${base}/board?debug=1`);
    await expect(page.locator('#version')).toContainText('服務版本：v');
    await expect(page.locator('#pod')).toHaveText('Pod：' + uid);
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
