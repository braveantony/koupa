import { test, expect } from '@playwright/test';
import * as fs from 'node:fs';

// Plan A14 協定: a real Chromium EventSource on the board page, recorded
// event by event, so protocol.sh can compare it with Rush Hour's boards and
// the raw curl stream.
//
// Environment (set by protocol.sh):
//   BOARD_URL  the board page, e.g. http://<gateway>/board or a port-forward
//   OUT_FILE   JSON written at the end: {connections, events}
//   STOP_FILE  the test ends when this file exists (or MAX_SECONDS passes)
//
// The page's own script is not changed: an init script wraps
// window.EventSource and adds listeners (open, error, hello, ready,
// shutdown) before the page adds its own, recording lastEventId and data.
// The Pod of each connection comes from the X-Koupa-Pod-UID header of the
// /events responses (EventSource cannot read headers), in order: the k-th
// 200 response is connection k (the k-th open).

const BOARD_URL = process.env.BOARD_URL ?? '';
const OUT_FILE = process.env.OUT_FILE ?? '/out/browser.json';
const STOP_FILE = process.env.STOP_FILE ?? '/out/stop';
const MAX_SECONDS = Number(process.env.MAX_SECONDS ?? '600');

test('board EventSource recording', async ({ page }) => {
  expect(BOARD_URL, 'BOARD_URL').not.toBe('');
  const responses: { t: number; status: number; pod_uid: string | null; url: string }[] = [];
  page.on('response', (r) => {
    if (new URL(r.url()).pathname === '/events') {
      responses.push({ t: Date.now(), status: r.status(), pod_uid: r.headers()['x-koupa-pod-uid'] ?? null, url: r.url() });
    }
  });
  await page.addInitScript(() => {
    const Orig = window.EventSource;
    const log: any[] = [];
    (window as any).__a14 = log;
    let conn = 0;
    class Recorded extends Orig {
      constructor(url: string | URL, init?: EventSourceInit) {
        super(url, init);
        this.addEventListener('open', () => {
          conn++;
          log.push({ k: 'open', conn, t: Date.now() });
        });
        this.addEventListener('error', () => log.push({ k: 'error', conn, t: Date.now(), ready_state: this.readyState }));
        for (const name of ['hello', 'ready', 'shutdown']) {
          this.addEventListener(name, (e: Event) => {
            const m = e as MessageEvent;
            log.push({ k: name, conn, t: Date.now(), id: m.lastEventId, data: m.data });
          });
        }
      }
    }
    (window as any).EventSource = Recorded;
  });
  await page.goto(BOARD_URL);
  // Started: protocol.sh waits for this before it starts the load.
  await expect.poll(() => page.evaluate(() => (window as any).__a14.some((e: any) => e.k === 'hello')), { timeout: 30_000 }).toBe(true);
  fs.writeFileSync(OUT_FILE + '.started', String(Date.now()));
  const deadline = Date.now() + MAX_SECONDS * 1000;
  let events: any[] = [];
  while (!fs.existsSync(STOP_FILE) && Date.now() < deadline) {
    await page.waitForTimeout(250);
    events = await page.evaluate(() => (window as any).__a14);
    // Progress for protocol.sh (it waits for the shutdown event).
    fs.writeFileSync(OUT_FILE + '.partial', JSON.stringify({ responses, events }));
  }
  events = await page.evaluate(() => (window as any).__a14);
  fs.writeFileSync(OUT_FILE, JSON.stringify({ board_url: BOARD_URL, user_agent: await page.evaluate(() => navigator.userAgent), responses, events }, null, 1));
  expect(events.filter((e) => e.k === 'hello').length).toBeGreaterThan(0);
});
