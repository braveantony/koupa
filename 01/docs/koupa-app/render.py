#!/usr/bin/env python3
"""Render the Koupa documentation diagrams using only Python's standard library.

Run from any working directory. All output is written beside this script.
Coordinates are explicit so routing and label clearance remain reviewable.
"""
from html import escape
from pathlib import Path
import xml.etree.ElementTree as ET

FONT = 'Noto Sans TC, PingFang TC, Microsoft JhengHei, WenQuanYi Zen Hei, sans-serif'
COLORS = {'app': '#eaf3ff', 'db': '#eaf5ef', 'entry': '#fff2d9',
          'host': '#f0edf8', 'neutral': '#f5f7fa'}
INK = '#26364a'


class Diagram:
    def __init__(self, title, height, description, width=1240):
        self.height = height
        self.parts = [f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" viewBox="0 0 {width} {height}" role="img" aria-labelledby="title desc" font-family="{FONT}">',
                      f'<title id="title">{escape(title)}</title>',
                      f'<desc id="desc">{escape(description)}</desc>',
                      '<defs><marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M 0 0 L 10 5 L 0 10 z" fill="#52657b"/></marker></defs>',
                      f'<rect width="{width}" height="{height}" fill="white"/>']
        self.text(32, 49, title, 26, bold=True)

    def text(self, x, y, value, size=16, bold=False, anchor='start'):
        self.parts.append(f'<text x="{x}" y="{y}" font-size="{size}" fill="{INK}" text-anchor="{anchor}"' + (' font-weight="700"' if bold else '') + f'>{escape(value)}</text>')

    def rect(self, x, y, w, h, fill, dashed=False, radius=12):
        self.parts.append(f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="{radius}" fill="{fill}" stroke="#9baabd" stroke-width="1.5"' + (' stroke-dasharray="7 5"' if dashed else '') + '/>')

    def group(self, x, y, w, h, title, fill):
        self.rect(x, y, w, h, fill)
        self.text(x+18, y+31, title, 20, True)

    def box(self, x, y, w, h, title, tag, lines, color='app', dashed=False):
        self.rect(x, y, w, h, COLORS[color], dashed)
        self.text(x+14, y+24, tag, 15)
        title_size = 18 if sum(1 if ord(c) > 127 else 0.62 for c in title) * 20 > w - 28 else 20
        self.text(x+14, y+55, title, title_size, True)
        for i, line in enumerate(lines):
            self.text(x+14, y+85+i*27, line)

    def line(self, points, style='normal', arrow=True, both=False):
        dash = {'sse': '12 5 2 5', 'dashed': '7 5', 'notify': '2 5'}.get(style)
        attrs = f'fill="none" stroke="#52657b" stroke-width="{3.5 if style == "main" else 1.8}" stroke-linejoin="round"'
        if dash:
            attrs += f' stroke-dasharray="{dash}"'
        if arrow:
            attrs += ' marker-end="url(#arrow)"'
        if both:
            attrs += ' marker-start="url(#arrow)"'
        self.parts.append('<polyline points="' + ' '.join(f'{x},{y}' for x,y in points) + f'" {attrs}/>')

    def save(self, name):
        svg = '\n'.join(self.parts + ['</svg>', ''])
        ET.fromstring(svg)
        Path(__file__).resolve().with_name(name).write_text(svg, encoding='utf-8')


def app():
    d = Diagram('Koupa 酷杯 App（不含 Kubernetes）', 980,
                '點餐頁與廚房頁以短連線呼叫 Go App；App 透過 SSE 推送叫號看板，並以 SQL 與 PostgreSQL 溝通。', width=1580)
    d.box(30, 140, 460, 155, '點餐頁  /', '瀏覽器頁面',
          ['選飲料、送出訂單', 'POST /orders', '回應：號碼牌'], 'host')
    d.box(30, 340, 460, 255, '廚房頁  /kitchen', '瀏覽器頁面',
          ['每 2 秒更新「製作中」的訂單：', 'GET /api/orders', '按「完成」：POST /orders/{id}/ready', 'v2 才有「售完」切換：', 'POST /api/menu/{id}/available'], 'host')
    d.box(30, 640, 460, 165, '叫號看板  /board', '瀏覽器頁面',
          ['GET /events（SSE 長連線）', '伺服器持續推送：', '「請 0058 號取餐」'], 'host')
    d.box(640, 180, 280, 415, 'koupa App', 'process',
          ['Go 程式 · 埠 8080', '收單時發號碼牌', '出餐時分配叫號編號', '推送叫號給看板', 'v1 基本功能', 'v2 多「售完標示」'])
    d.box(1320, 180, 230, 195, 'PostgreSQL', '資料庫',
          ['menu_items', 'orders', 'order_items', 'idempotency_keys'], 'db')
    d.line([(490, 230), (640, 230)], 'main')
    d.text(565, 211, 'POST /orders', 16, anchor='middle')
    d.text(565, 261, '短連線', 15, anchor='middle')
    d.line([(490, 460), (640, 460)])
    d.text(565, 440, '短連線', 16, anchor='middle')
    d.line([(640, 550), (610, 550), (610, 680), (490, 680)], 'sse')
    d.text(640, 650, 'SSE 推送', 16)
    d.text(640, 675, '瀏覽器發起連線，App 推送事件', 15)
    # Direct, parallel database links preserve the horizontal ordering path.
    d.line([(920, 230), (1320, 230)], 'main', both=True)
    d.text(1120, 211, 'SQL：存訂單、分配叫號編號 ready_seq', 16, anchor='middle')
    d.line([(1320, 340), (920, 340)], 'notify')
    d.text(1120, 321, 'LISTEN/NOTIFY：有新叫號', 16, anchor='middle')
    d.group(960, 820, 590, 135, '圖例', '#fafbfd')
    d.line([(980, 875), (1035, 875)])
    d.text(1050, 881, '短連線', 15)
    d.line([(1240, 875), (1295, 875)], 'sse')
    d.text(1310, 881, 'SSE 長連線', 15)
    d.line([(980, 920), (1035, 920)], 'notify')
    d.text(1050, 926, '資料庫通知', 15)
    d.line([(1240, 920), (1295, 920)], 'main')
    d.text(1310, 926, '粗線＝主要路徑', 15)
    d.save('app.svg')


def deployed():
    d = Diagram('部署到 Kubernetes 之後', 1480,
                '主機上的瀏覽器與 Rush Hour 經 Gateway、Service 呼叫分散於三個 worker 的 App Pod。資料庫與 CNPG operator 固定於 infra worker。')
    d.group(20, 100, 280, 495, '主機（叢集外）', '#f8f6fc')
    d.group(320, 100, 900, 1200, 'Kubernetes 叢集（tk8s）', '#f8fafc')
    d.group(340, 155, 860, 915, 'namespace：koupa', '#ffffff')
    d.group(340, 1100, 860, 175, 'namespace：cnpg-system', '#f1f7f3')
    d.box(40, 175, 240, 150, '瀏覽器', '主機端',
          ['點餐頁 /', '廚房頁 /kitchen', '叫號看板 /board'], 'host')
    d.box(40, 385, 240, 195, 'Rush Hour', 'container（podman）',
          ['量測工具：模擬客人、', '廚房和 3 台看板', '每秒送出 20 張訂單'], 'host', True)
    d.box(370, 220, 350, 175, 'Gateway koupa', 'Gateway',
          ['Cilium Gateway · 埠 80', 'LoadBalancer IP', '系統入口；照 HTTPRoute 轉送'], 'entry')
    d.box(770, 220, 400, 115, 'HTTPRoute koupa', 'HTTPRoute',
          ['/ → Service koupa:80'], 'entry')
    d.box(370, 460, 350, 110, 'Service koupa', 'Service',
          ['ClusterIP · 80 → 8080'], 'entry')
    for x, number in [(370, 1), (650, 2), (930, 3)]:
        d.box(x, 670, 240, 145, f'koupa Pod {number}', 'Pod',
              ['koupa:v1 → 升級到 v2', f'所在節點：worker {chr(64 + number)}'])
    d.text(910, 568, 'topology spread：穩定狀態下，', 16, anchor='middle')
    d.text(910, 595, '3 個 worker 各一個同版本 Pod', 16, anchor='middle')
    d.box(650, 895, 520, 150, 'PostgreSQL koupa-db-1', '資料庫 · CNPG 單一實例',
          ['Service koupa-db-rw:5432', '固定於 infra worker（worker A）', '節點標籤：koupa.dev/infra=true'], 'db')
    d.box(370, 895, 255, 150, 'koupa-migrate-000004', 'Job · migration',
          ['每次部署執行一次', '只套用還沒執行過的變更', '完成就結束'], 'neutral', True)
    d.box(650, 1145, 520, 110, 'CNPG operator', 'operator',
          ['管理 PostgreSQL · infra worker（worker A）'], 'db')
    d.line([(970, 335), (970, 365), (720, 365)], 'dashed')
    d.text(790, 354, '套用到 Gateway', 16)
    # Host traffic joins in the gap between the two host boxes.
    d.line([(160, 325), (160, 350), (370, 350)], 'main')
    d.text(40, 376, 'HTTP：http://<Gateway IP>/', 15)
    d.line([(280, 475), (310, 475), (310, 350)], 'main', arrow=False)
    d.line([(545, 395), (545, 460)], 'main')
    d.line([(545, 570), (545, 610)], 'main', arrow=False)
    d.line([(490, 610), (1050, 610)], 'main', arrow=False)
    for x in (490, 770, 1050):
        d.line([(x, 610), (x, 670)], 'main')
        d.line([(x, 815), (x, 855)], arrow=False)
    d.line([(490, 855), (1050, 855)], arrow=False)
    d.line([(910, 855), (910, 895)])
    d.text(925, 879, 'SQL', 15)
    d.line([(497, 895), (497, 885), (680, 885), (680, 895)], 'dashed')
    d.text(570, 878, '套用變更', 15)
    d.line([(910, 1145), (910, 1045)], 'dashed')
    d.text(930, 1093, '管理', 15)
    d.group(340, 1320, 860, 150, '圖例', '#fafbfd')
    d.line([(360, 1375), (410, 1375)], 'main')
    d.text(425, 1381, '使用者主要路徑', 15)
    d.rect(610, 1361, 35, 25, '#fff', True, 4)
    d.text(655, 1381, '虛線框＝container／一次性 Job', 15)
    d.text(360, 1410, '角落標籤＝資源種類；小字「所在節點」＝worker；虛線箭頭＝套用到 Gateway／管理／套用變更', 15)
    d.text(360, 1435, 'worker A／B／C 為示意，實際名稱用 kubectl get pods -o wide 查', 15)
    d.save('deployed.svg')


if __name__ == '__main__':
    app()
    deployed()
