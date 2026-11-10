'use strict';
// One small script for all three pages; document.body.dataset.page picks
// which one runs.

const $ = (id) => document.getElementById(id);

async function getJSON(url) {
  const r = await fetch(url);
  if (!r.ok) throw new Error(url + ' ' + r.status);
  return r.json();
}

// ---- order page ----

function initOrder() {
  let menu = [];
  let pending = null; // {ref, body} kept while a submit is unresolved

  function quantities() {
    const items = [];
    for (const input of document.querySelectorAll('#menu input')) {
      const q = parseInt(input.value, 10) || 0;
      if (q > 0) items.push({ menu_item_id: Number(input.dataset.id), quantity: q });
    }
    return items;
  }
  function updateTotal() {
    let total = 0;
    for (const it of quantities()) total += it.quantity * menu.find((m) => m.id === it.menu_item_id).price;
    $('total').textContent = total;
  }
  async function loadMenu() {
    menu = await getJSON('/api/menu');
    const ul = $('menu');
    ul.textContent = '';
    for (const m of menu) {
      const li = document.createElement('li');
      li.dataset.id = m.id;
      li.dataset.name = m.name;
      const label = document.createElement('label');
      label.textContent = m.name + '　' + m.price + ' 元　';
      const input = document.createElement('input');
      input.type = 'number';
      input.min = '0';
      input.max = '20';
      input.value = '0';
      input.dataset.id = m.id;
      input.addEventListener('input', updateTotal);
      label.appendChild(input);
      li.appendChild(label);
      if (!m.available) {
        li.classList.add('sold-out');
        input.disabled = true;
        li.appendChild(document.createTextNode('（售完）'));
      }
      ul.appendChild(li);
    }
    updateTotal();
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
    const result = $('result');
    result.className = '';
    if (!pending) {
      const items = quantities();
      if (items.length === 0) {
        result.className = 'err';
        result.textContent = '請先選擇飲料';
        return;
      }
      // One client_ref per order; a retry reuses it and the same body.
      const ref = crypto.randomUUID();
      pending = { ref, body: JSON.stringify({ client_ref: ref, items }) };
    }
    $('submit').disabled = true;
    result.textContent = '送出中…';
    let res = await attempt(pending);
    if (res.status === 0 || res.status >= 500) {
      res = await attempt(pending); // retry once, same client_ref and body
    }
    $('submit').disabled = false;
    if (res.status === 200 || res.status === 201) {
      pending = null;
      result.textContent = '訂單成立，您的號碼牌：' + res.data.number + '（合計 ' + res.data.total + ' 元）';
      for (const input of document.querySelectorAll('#menu input')) input.value = '0';
      updateTotal();
    } else if (res.status === 409 || res.status === 422) {
      pending = null; // the order itself was rejected; a new one gets a new ref
      result.className = 'err';
      result.textContent = '無法送出：' + describeError(res);
      loadMenu().catch(() => {});
    } else {
      // Still unknown: keep `pending` so pressing again reuses the same client_ref.
      result.className = 'err';
      result.textContent = '連線失敗，請再按一次送出';
    }
  }
  $('submit').addEventListener('click', submit);
  loadMenu().catch(() => { $('result').textContent = '菜單載入失敗'; });
}

// ---- kitchen page ----

function initKitchen() {
  const msg = (t) => { $('msg').textContent = t; };
  async function refreshOrders() {
    try {
      const orders = await getJSON('/api/orders?status=pending');
      const ul = $('orders');
      ul.textContent = '';
      for (const o of orders) {
        const li = document.createElement('li');
        li.dataset.orderId = o.order_id;
        li.dataset.number = o.number;
        li.appendChild(document.createTextNode('號碼牌 ' + o.number + '　' + o.total + ' 元　'));
        const items = document.createElement('span');
        items.className = 'items';
        items.textContent = (o.items || []).map((it) => it.name + ' x' + it.quantity).join('、') + '　';
        li.appendChild(items);
        const b = document.createElement('button');
        b.textContent = '完成';
        b.addEventListener('click', async () => {
          b.disabled = true;
          try {
            const r = await fetch('/orders/' + o.order_id + '/ready', { method: 'POST' });
            if (!r.ok) throw new Error(String(r.status));
            msg('');
          } catch (e) {
            msg('操作失敗，請再試一次');
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
    $('menu-title').hidden = false;
    const menu = await getJSON('/api/menu');
    const ul = $('menu');
    ul.textContent = '';
    for (const m of menu) {
      const li = document.createElement('li');
      li.dataset.id = m.id;
      li.appendChild(document.createTextNode(m.name + '　'));
      const b = document.createElement('button');
      const show = (available) => {
        b.textContent = available ? '設為售完' : '恢復供應';
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
  const MAX_RECENT = 5;
  window.__events = []; // names of the events seen, for tests and debugging
  const recent = [];
  const es = new EventSource('/events');
  es.addEventListener('open', () => { $('status').textContent = '已連線'; });
  es.addEventListener('error', () => {
    window.__events.push('error');
    $('status').textContent = '連線中斷，瀏覽器正在重新連線…';
  });
  es.addEventListener('hello', (e) => {
    window.__events.push('hello');
    $('version').textContent = '服務版本：' + JSON.parse(e.data).version;
  });
  es.addEventListener('ready', (e) => {
    window.__events.push('ready');
    const d = JSON.parse(e.data);
    $('latest').textContent = '請 ' + d.number + ' 號取餐 🧋';
    recent.unshift(d.number);
    recent.length = Math.min(recent.length, MAX_RECENT);
    const ul = $('list');
    ul.textContent = '';
    for (const n of recent) {
      const li = document.createElement('li');
      li.textContent = n;
      ul.appendChild(li);
    }
  });
  // The server is going away; just record it and let the browser reconnect.
  es.addEventListener('shutdown', () => {
    window.__events.push('shutdown');
    console.log('koupa: server sent shutdown');
    $('status').textContent = '伺服器即將重啟，等待瀏覽器重新連線…';
  });
}

const init = { order: initOrder, kitchen: initKitchen, board: initBoard }[document.body.dataset.page];
if (init) init();
