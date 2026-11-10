'use strict';
// One small script for all three pages; document.body.dataset.page picks
// which one runs.

const $ = (id) => document.getElementById(id);

async function getJSON(url) {
  const r = await fetch(url);
  if (!r.ok) throw new Error(url + ' ' + r.status);
  return r.json();
}

function el(tag, className, text) {
  const e = document.createElement(tag);
  if (className) e.className = className;
  if (text !== undefined) e.textContent = text;
  return e;
}

// Teaching info (service version, which Pod answered, SSE state) is only
// shown with ?debug=1 in the URL.
const DEBUG = new URLSearchParams(location.search).get('debug') === '1';

function initDebug(page) {
  if (!DEBUG) return;
  $('debug').hidden = false;
  getJSON('/api/whoami')
    .then((who) => {
      // On the board the version comes from the SSE hello event instead.
      if (page !== 'board') $('version').textContent = '服務版本：' + who.version;
      $('pod').textContent = 'Pod：' + (who.pod_name || who.pod_uid || '?');
    })
    .catch(() => { $('pod').textContent = 'Pod：查詢失敗'; });
}

// ---- order page ----

// Photo per menu id (img/*.webp, 800×800).
const PHOTOS = { 1: 'boba-milk-tea', 2: 'sunset-black-tea', 3: 'green-tea-trio', 4: 'winter-melon-lemon' };

function initOrder() {
  let menu = [];
  const qty = new Map(); // menu id -> cups chosen
  let pending = null; // {ref, body, items} kept while a submit is unresolved
  let sending = false;

  function quantities() {
    const items = [];
    for (const m of menu) {
      const q = qty.get(m.id) || 0;
      if (q > 0) items.push({ menu_item_id: m.id, quantity: q });
    }
    return items;
  }
  function notice(text, kind) {
    const n = $('notice');
    n.className = 'notice' + (kind ? ' ' + kind : '');
    n.textContent = text;
  }
  function updateTotal() {
    let total = 0;
    let cups = 0;
    for (const it of quantities()) {
      total += it.quantity * menu.find((m) => m.id === it.menu_item_id).price;
      cups += it.quantity;
    }
    $('total').textContent = total;
    $('cups').textContent = cups;
    // A pending order can always be sent again, even if the steppers changed.
    $('submit').disabled = sending || (!pending && cups === 0);
    for (const li of document.querySelectorAll('#menu li')) {
      const q = qty.get(Number(li.dataset.id)) || 0;
      li.querySelector('.qty').textContent = q;
      li.querySelector('.dec').disabled = li.classList.contains('sold-out') || q === 0;
      li.querySelector('.inc').disabled = li.classList.contains('sold-out') || q >= 20;
    }
  }
  function step(id, d) {
    qty.set(id, Math.max(0, Math.min(20, (qty.get(id) || 0) + d)));
    updateTotal();
  }
  async function loadMenu() {
    menu = await getJSON('/api/menu');
    const ul = $('menu');
    ul.textContent = '';
    menu.forEach((m, i) => {
      if (!m.available) qty.delete(m.id);
      const li = el('li', 'card');
      li.dataset.id = m.id;
      li.dataset.name = m.name;
      const img = el('img');
      img.src = '/img/' + (PHOTOS[m.id] || PHOTOS[1]) + '.webp';
      img.alt = m.name;
      img.width = 800;
      img.height = 800;
      img.decoding = 'async';
      if (i >= 3) img.loading = 'lazy'; // the first cards are above the fold
      li.appendChild(img);
      const info = el('div', 'info');
      info.appendChild(el('span', 'name', m.name));
      info.appendChild(el('span', 'price', 'NT$ ' + m.price));
      li.appendChild(info);
      const stepper = el('div', 'stepper');
      const dec = el('button', 'dec', '−');
      dec.type = 'button';
      dec.setAttribute('aria-label', '減少一杯 ' + m.name);
      dec.addEventListener('click', () => step(m.id, -1));
      const q = el('span', 'qty', '0');
      q.setAttribute('aria-live', 'polite');
      const inc = el('button', 'inc', '+');
      inc.type = 'button';
      inc.setAttribute('aria-label', '增加一杯 ' + m.name);
      inc.addEventListener('click', () => step(m.id, 1));
      stepper.append(dec, q, inc);
      li.appendChild(stepper);
      if (!m.available) {
        li.classList.add('sold-out');
        info.appendChild(el('span', 'sold-label', '今日售完'));
      }
      ul.appendChild(li);
    });
    updateTotal();
  }

  function showDone(number, items, total) {
    $('ticket').textContent = number;
    const lines = $('lines');
    lines.textContent = '';
    for (const it of items) {
      const m = menu.find((x) => x.id === it.menu_item_id);
      const li = el('li');
      li.append(el('span', '', (m ? m.name : '#' + it.menu_item_id) + ' × ' + it.quantity),
        el('span', '', m ? 'NT$ ' + m.price * it.quantity : ''));
      lines.appendChild(li);
    }
    $('paid').textContent = total;
    $('shop').hidden = true;
    $('cart').hidden = true;
    $('result').hidden = false;
    window.scrollTo(0, 0);
    $('again').focus();
  }
  function orderAgain() {
    $('result').hidden = true;
    $('shop').hidden = false;
    $('cart').hidden = false;
    notice('');
    loadMenu().catch(() => notice('菜單載入失敗', 'err'));
  }

  // One attempt; returns {status, data}. status 0 means network failure.
  async function attempt(p) {
    try {
      const r = await fetch('/orders', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-Koupa-Attempt': crypto.randomUUID() },
        body: p.body,
      });
      return { status: r.status, data: await r.json().catch(() => ({})) };
    } catch (e) {
      return { status: 0, data: {} };
    }
  }
  // Known API errors in Traditional Chinese; the raw text is for logs only.
  function describeError(res) {
    const raw = String(res.data.error || '');
    if (/sold out/i.test(raw)) return '有品項已售完，請重新選擇';
    if (res.status === 409) return '這筆訂單編號已被不同內容使用，請重新下單';
    if (res.status === 422) return '訂單內容有誤，請檢查品項與數量';
    return '請稍後再試（錯誤 ' + res.status + '）';
  }
  async function submit() {
    if (!pending) {
      const items = quantities();
      if (items.length === 0) {
        notice('請先選擇飲料', 'err');
        return;
      }
      // One client_ref per order; a retry reuses it and the same body.
      const ref = crypto.randomUUID();
      pending = { ref, body: JSON.stringify({ client_ref: ref, items }), items };
    }
    sending = true;
    updateTotal();
    notice('送出中…', 'info');
    let res = await attempt(pending);
    if (res.status === 0 || res.status >= 500) {
      res = await attempt(pending); // retry once, same client_ref and body
    }
    sending = false;
    if (res.status === 200 || res.status === 201) {
      const sent = pending.items;
      pending = null;
      notice('');
      showDone(res.data.number, sent, res.data.total);
      qty.clear();
      updateTotal();
    } else if (res.status === 409 || res.status === 422) {
      pending = null; // the order itself was rejected; a new one gets a new ref
      notice('無法送出：' + describeError(res), 'err');
      updateTotal();
      loadMenu().catch(() => {});
    } else {
      // Still unknown: keep `pending` so pressing again reuses the same client_ref.
      notice('連線失敗，請再按一次「確認點餐」', 'err');
      updateTotal();
    }
  }
  $('submit').addEventListener('click', submit);
  $('again').addEventListener('click', orderAgain);
  loadMenu().catch(() => notice('菜單載入失敗', 'err'));
}

// ---- kitchen page ----

function initKitchen() {
  const msg = (t) => { $('msg').textContent = t; };
  let shown = ''; // last rendered order list, to skip identical redraws
  async function refreshOrders() {
    try {
      const orders = await getJSON('/api/orders?status=pending');
      $('count').textContent = orders.length;
      $('no-orders').hidden = orders.length > 0;
      const key = JSON.stringify(orders.map((o) => [o.order_id, o.total, o.items]));
      if (key === shown) return;
      shown = key;
      const ul = $('orders');
      ul.textContent = '';
      for (const o of orders) {
        const li = el('li', 'ticket-card');
        li.dataset.orderId = o.order_id;
        li.dataset.number = o.number;
        const meta = el('div', 'meta');
        meta.append(el('span', '', '號碼牌'), el('span', '', 'NT$ ' + o.total));
        li.append(meta, el('span', 'number num', o.number));
        const items = el('ul', 'items');
        for (const it of o.items || []) {
          const row = el('li');
          row.append(el('span', '', it.name), document.createTextNode(' '), el('span', 'q', '× ' + it.quantity));
          items.appendChild(row);
        }
        li.appendChild(items);
        const b = el('button', 'done-btn', '完成');
        b.type = 'button';
        b.setAttribute('aria-label', '完成 ' + o.number + ' 號');
        b.addEventListener('click', async () => {
          b.disabled = true;
          try {
            const r = await fetch('/orders/' + o.order_id + '/ready', { method: 'POST' });
            if (!r.ok) throw new Error(String(r.status));
            msg('');
          } catch (e) {
            msg('操作失敗，請再試一次');
            shown = ''; // redraw so the button is usable again
          }
          refreshOrders();
        });
        li.appendChild(b);
        ul.appendChild(li);
      }
    } catch (e) {
      msg('更新訂單失敗，稍後自動重試');
    }
  }
  async function loadMenu() {
    const who = await getJSON('/api/whoami');
    if (who.version !== 'v2') return; // v1 has no sold-out feature
    $('supply').hidden = false;
    const menu = await getJSON('/api/menu');
    const ul = $('menu');
    ul.textContent = '';
    for (const m of menu) {
      const li = el('li');
      li.dataset.id = m.id;
      const state = el('span', 'state');
      li.append(el('span', 'name', m.name), state);
      const b = el('button');
      b.type = 'button';
      const show = (available) => {
        b.textContent = available ? '設為售完' : '恢復供應';
        state.textContent = available ? '供應中' : '今日售完';
        li.classList.toggle('sold-out', !available);
        b.dataset.available = String(available);
      };
      show(m.available);
      b.addEventListener('click', async () => {
        const next = b.dataset.available !== 'true';
        try {
          const r = await fetch('/api/menu/' + m.id + '/available', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ available: next }),
          });
          if (!r.ok) throw new Error(String(r.status));
          show(next);
        } catch (e) {
          msg('設定售完失敗');
        }
      });
      li.appendChild(b);
      ul.appendChild(li);
    }
  }
  refreshOrders();
  setInterval(refreshOrders, 2000);
  loadMenu().catch(() => msg('菜單載入失敗'));
}

// ---- board page ----

function initBoard() {
  const MAX_RECENT = 5; // the latest number plus 4 earlier ones
  const MAX_MAKING = 30; // the screen has room for about this many
  window.__events = []; // names of the events seen, for tests and debugging
  const recent = [];
  const status = (t) => { $('status').textContent = t; };

  function renderMaking(numbers) {
    const ul = $('making');
    ul.textContent = '';
    for (const n of numbers.slice(0, MAX_MAKING)) {
      const li = el('li', 'num', n);
      li.dataset.number = n;
      ul.appendChild(li);
    }
    if (numbers.length > MAX_MAKING) ul.appendChild(el('li', '', '…'));
    $('no-making').hidden = numbers.length > 0;
  }
  // Same 2 s polling as the kitchen page.
  async function refreshMaking() {
    try {
      const orders = await getJSON('/api/orders?status=pending');
      renderMaking(orders.map((o) => o.number));
    } catch (e) {
      // keep the last list; the next poll tries again
    }
  }
  refreshMaking();
  setInterval(refreshMaking, 2000);

  const es = new EventSource('/events');
  es.addEventListener('open', () => { status('SSE 已連線'); });
  es.addEventListener('error', () => {
    window.__events.push('error');
    status('SSE 連線中斷，瀏覽器正在重新連線…');
  });
  es.addEventListener('hello', (e) => {
    window.__events.push('hello');
    $('version').textContent = '服務版本：' + JSON.parse(e.data).version;
  });
  es.addEventListener('ready', (e) => {
    window.__events.push('ready');
    const d = JSON.parse(e.data);
    const latest = $('latest');
    latest.textContent = '';
    latest.append(el('span', 'big num', d.number), el('span', 'caption', '請 ' + d.number + ' 號取餐 🧋'));
    recent.unshift(d.number);
    recent.length = Math.min(recent.length, MAX_RECENT);
    const ul = $('list');
    ul.textContent = '';
    for (const n of recent.slice(1)) ul.appendChild(el('li', 'num', n));
    $('recent').hidden = recent.length < 2;
    // The called number leaves 製作中 now rather than at the next poll.
    const li = document.querySelector('#making li[data-number="' + d.number + '"]');
    if (li) li.remove();
    $('no-making').hidden = $('making').children.length > 0;
  });
  // The server is going away; just record it and let the browser reconnect.
  es.addEventListener('shutdown', () => {
    window.__events.push('shutdown');
    console.log('koupa: server sent shutdown');
    status('SSE：伺服器即將重啟，等待瀏覽器重新連線…');
  });
}

const page = document.body.dataset.page;
const init = { order: initOrder, kitchen: initKitchen, board: initBoard }[page];
if (init) {
  initDebug(page);
  init();
}
