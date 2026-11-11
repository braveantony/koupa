#!/usr/bin/env python3
"""Seven course timelines; standard library only, explicit coordinates.

Run from any directory. Writes only the seven SVGs beside this script.
Source references are retained with each diagram below. Numbers describe tkha
observations, not Kubernetes guarantees. Horizontal distances are not to scale.
"""
from html import escape
from pathlib import Path
import xml.etree.ElementTree as ET

FONT = 'Noto Sans TC, PingFang TC, Microsoft JhengHei, WenQuanYi Zen Hei, sans-serif'
COLORS = {'app': '#eaf3ff', 'db': '#eaf5ef', 'entry': '#fff2d9',
          'host': '#f0edf8', 'neutral': '#f5f7fa'}
INK = '#26364a'
GREEN = '#24764c'
RED = '#ad3945'


class Diagram:
    def __init__(self, title, subtitle, change, ticks, lanes, height=850):
        self.height = height
        self.parts = [f'<svg xmlns="http://www.w3.org/2000/svg" width="1240" height="{height}" viewBox="0 0 1240 {height}" role="img" aria-labelledby="title desc" font-family="{FONT}">',
                      f'<title id="title">{escape(title)}</title>',
                      f'<desc id="desc">{escape(subtitle)}。時間由左往右，非等比例。</desc>',
                      '<defs><marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M 0 0 L 10 5 L 0 10 z" fill="#52657b"/></marker>',
                      '<pattern id="hatch" width="9" height="9" patternUnits="userSpaceOnUse"><rect width="9" height="9" fill="#fff0f0"/><path d="M-2,2 L2,-2 M0,9 L9,0 M7,11 L11,7" stroke="#e6a9ae" stroke-width="1"/></pattern></defs>',
                      f'<rect width="1240" height="{height}" fill="white"/>']
        self.text(32, 49, title, 28, True)
        self.text(32, 86, subtitle, 21)
        self.rect(32, 108, 1176, 48, COLORS['app'])
        self.text(50, 140, change, 22, True)
        self.text(32, 190, '時間 →', 19, True)
        self.line(210, 202, 1190)
        for x, label in ticks:
            self.text(x, 185, label, 18, anchor='middle')
        self.text(32, 220, '非等比例', 16)
        for i, label in enumerate(lanes):
            y = 250 + i * 116
            self.rect(24, y, 1192, 104, COLORS['neutral'])
            self.text(42, y + 58, label, 22, True)
        self.legend(height - 52)

    def text(self, x, y, value, size=20, bold=False, anchor='start', color=INK):
        self.parts.append(f'<text x="{x}" y="{y}" font-size="{size}" fill="{color}" text-anchor="{anchor}"' + (' font-weight="700"' if bold else '') + f'>{escape(value)}</text>')

    def rect(self, x, y, w, h, fill):
        self.parts.append(f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="8" fill="{fill}"/>')

    def line(self, x, y, end, color='#52657b', arrow=True):
        self.parts.append(f'<path d="M{x},{y} H{end}" fill="none" stroke="{color}" stroke-width="4"' + (' marker-end="url(#arrow)"' if arrow else '') + '/>')

    def span(self, lane, x, end, label, detail='', kind='normal'):
        y = 250 + lane * 116
        if kind == 'problem':
            self.rect(x, y + 8, end - x, 34, 'url(#hatch)')
        elif kind == 'change':
            self.rect(x, y + 8, end - x, 88, COLORS['app'])
        self.text(x + 10, y + 34, label, 21, True,
                  color=RED if kind == 'problem' else INK)
        self.line(x + 10, y + 49, end - 12,
                  RED if kind == 'problem' else '#52657b')
        if detail:
            self.text(x + 10, y + 80, detail, 18)

    def note(self, y, value, good=False):
        self.text(42, y, ('✓ ' if good else '') + value, 20, good,
                  color=GREEN if good else INK)

    def legend(self, y):
        self.line(42, y, 100)
        self.text(115, y + 7, '主要過程', 18)
        self.rect(260, y - 16, 32, 28, COLORS['app'])
        self.text(303, y + 7, '本步改動', 18)
        self.rect(470, y - 16, 32, 28, 'url(#hatch)')
        self.text(513, y + 7, '問題時窗', 18)
        self.text(730, y + 7, '✓ 相較前一步消失的問題', 18, color=GREEN)

    def save(self, name):
        svg = '\n'.join(self.parts + ['</svg>', ''])
        ET.fromstring(svg)
        Path(__file__).resolve().with_name(name).write_text(svg, encoding='utf-8')
        print(name)


LANES = ['入口（Gateway）', '舊 Pod', '新 Pod', '看板']


def idle():
    # design-v2/04-step-mechanisms.md:84-88; measurements/2026-11-10.md:77-78
    # board/sse.go:47-48; web/app.js:459-463. Browser 301→304: brief only.
    d = Diagram('小實驗：看板沒叫號，也能保持連線嗎？',
                '解決：閒置斷線　還剩：心跳不能修好升級時的斷線',
                'SSE_HEARTBEAT=15s：每 15 秒送一行 : hb',
                [(240, '0 秒'), (490, '15、30…秒'), (810, '300 秒'), (1120, '420 秒')],
                ['沒有心跳', '有心跳'], 660)
    d.span(0, 220, 770, 'hello → 一直沒有資料', '入口等了 300 秒')
    d.span(0, 785, 1195, '入口切斷：閒置逾時', 'response_flags=SI；curl 結束碼 18', 'problem')
    d.span(1, 220, 770, 'hello → : hb → : hb → …', '每 15 秒一行；沒有叫號也有資料', 'change')
    d.span(1, 785, 1195, '420 秒仍連著', 'curl 的 --max-time 到期，結束碼 28')
    d.note(508, '瀏覽器：「SSE 連線中斷…」')
    d.note(542, '瀏覽器約 3～5 秒後自動重連（Chromium 3 秒、Firefox 5 秒）')
    d.note(578, '有心跳後：300 秒閒置斷線消失', True)
    d.save('idle-lab.svg')


def baseline():
    # README:36,261; mechanisms:92,96,113-117,123-128,152;
    # observations.md:95-96. Startup and deletion share a schematic axis.
    d = Diagram('步驟 0：什麼都不做', '解決：（基準，還沒有任何修正）　還剩：送出失敗、漏號、重送可能重複',
                '基準：沒有 readinessProbe，也沒有 preStop 或程式收尾',
                [(345, '新 Pod 啟動'), (650, '開始關閉舊 Pod'), (1050, '看板重連')], LANES)
    d.span(0, 220, 570, '太早送給新 Pod', '送出失敗：尚未 listen', 'problem')
    d.span(0, 590, 950, '仍可能送給舊 Pod', '送出失敗：入口更新還沒到', 'problem')
    d.span(1, 590, 1195, 'SIGTERM → 立刻結束', '通知入口和通知程式關機同時進行，彼此不等待')
    d.span(2, 220, 570, '一啟動就 Ready', '部分 Pod 約 5 秒後才 listen', 'problem')
    d.span(2, 590, 1195, '開始接受連線（listen）→ 能接單')
    d.span(3, 220, 570, '原本連著舊 Pod')
    d.span(3, 590, 950, '約 3～5 秒：漏號', '瀏覽器預設的重連等待', 'problem')
    d.span(3, 970, 1195, '連到另一 Pod', '從「現在」收')
    d.note(760, '入口改送別的 Pod 只影響之後的訂單；已經連著的看板要等程式結束才斷。')
    d.save('step-0.svg')


def readiness():
    # step-1/kustomization.yaml:25-32; README:259-267;
    # step-1.md:「過程中看什麼」 (seconds since orders started).
    d = Diagram('步驟 1：新 Pod 準備好才接訂單',
                '解決：新 Pod 太早接單　還剩：舊 Pod 的送出失敗、漏號',
                'readinessProbe：每 2 秒檢查 /healthz',
                [(330, '新 Pod 啟動'), (650, '/healthz 通過'), (1050, '舊 Pod 關閉後')], LANES, 970)
    d.span(0, 220, 560, '等待 Ready', '不送給尚未準備好的新 Pod', 'change')
    d.span(0, 580, 760, '送給新 Pod')
    d.span(0, 780, 1195, '送出失敗仍在', '舊 Pod 端尚未修正', 'problem')
    d.span(1, 780, 1195, 'SIGTERM → 結束', '通知入口和關機仍同時進行')
    d.span(2, 220, 560, '啟動 → 等待就緒', '/healthz 還沒通過', 'change')
    d.span(2, 580, 1195, 'Ready → 接單', 'HTTP、資料庫、叫號通知都準備好了', 'change')
    d.span(3, 220, 760, '連著舊 Pod')
    d.span(3, 780, 1000, '斷線：漏號', '重連仍從「現在」收', 'problem')
    d.span(3, 1020, 1195, '再連到舊 Pod', '幾秒後再斷一次')
    d.note(754, '新 Pod 還沒開始接受連線（listen）就接單的失敗消失', True)
    d.note(800, '看板實測（從開始送單算起）：第 18 秒斷 → 第 21 秒連到另一個舊 Pod；')
    d.note(834, '　　　　　　　　　　　　　第 26 秒再斷 → 第 29 秒連到新 Pod')
    d.note(872, '送出失敗減少，但不是 0；readinessProbe 沒有縮短看板斷線。')
    d.save('step-1.svg')


def draining(step):
    # README:271-297; step-2 YAML:1-10,37-39; step-3 YAML:19-26;
    # board/handler.go:140-171,213-245; board/sse.go:40-44;
    # observations.md:252,272-273,359-360,448-449.
    titles = {2: '被關掉前先等 8 秒', 3: '先通知看板，再收尾', 4: '重連後，把漏掉的叫號補回來'}
    subs = {2: '解決：實測送出失敗 0　還剩：漏號每台約 80 個、重送可能重複',
            3: '解決：斷線時間縮短　還剩：漏號每台約 22 個、重送可能重複',
            4: '解決：實測漏號 0　還剩：結果未知時，重送可能重複下單'}
    changes = {2: 'preStop：sleep 8 秒，延後 SIGTERM',
               3: 'GRACEFUL_SHUTDOWN=1；SHUTDOWN_TIMEOUT=15s',
               4: 'RESUME_FROM_LAST_ID=1：用 Last-Event-ID 接著收'}
    d = Diagram(f'步驟 {step}：{titles[step]}', subs[step], changes[step],
                [(290, '終止開始：0 秒'), (620, '8 秒：SIGTERM'),
                 (1030, '約 11～13 秒：重連' if step == 2 else '約 9 秒：重連')], LANES, 970)
    d.span(0, 220, 570, '入口得知舊 Pod 要下線', 'SIGTERM 之前就不再送單給它')
    d.span(0, 590, 1195, '新訂單送給其他 Ready Pod', '已經連著的看板不受影響')
    d.span(1, 220, 570, 'preStop 等 8 秒', '程式仍運作、看板仍連著', 'change' if step == 2 else 'normal')
    if step == 2:
        d.span(1, 590, 1195, 'SIGTERM → 直接結束', '看板連線在這時被切斷')
    else:
        d.span(1, 590, 1195, '送 shutdown；等處理中的請求完成', '最多等 15 秒，之後結束', 'change' if step == 3 else 'normal')
    d.span(2, 220, 880, '已通過 readinessProbe → 可以接單')
    d.span(2, 900, 1195, '先補號，再即時推送' if step == 4 else '接受看板重連',
           '補送最後 id 之後的叫號' if step == 4 else '', 'change' if step == 4 else 'normal')
    d.span(3, 220, 570, '持續收到叫號', '仍在原本的舊 Pod')
    d.span(3, 590, 880, '約 3～5 秒：漏號' if step == 2 else ('約 1 秒：暫停接收' if step == 4 else '約 1 秒：漏號'),
           '瀏覽器預設的重連等待' if step == 2 else ('自動重連' if step == 3 else '帶回 Last-Event-ID'), 'problem' if step < 4 else 'change')
    d.span(3, 900, 1195, '補回斷線期間叫號' if step == 4 else '從「現在」開始收',
           '實測漏號 0' if step == 4 else '缺的叫號不會補回', 'change' if step == 4 else 'normal')
    if step == 2:
        d.note(754, '實測送出失敗降到 0；漏號仍約 80 個／台', True)
        d.note(798, '實測：入口最晚約 1.5 秒後就不再送單給舊 Pod；')
        d.note(834, '保守推估上限 2.127 秒，進位成 3 秒再加 5 秒緩衝 = 8 秒。')
        d.note(872, '看板：斷線後約 3～5 秒重新連上（依瀏覽器）。')
    elif step == 3:
        d.note(754, '3～5 秒的斷線縮短為約 1 秒（App 用 retry: 指定 1 秒）', True)
        d.note(798, 'preStop 8 ＋ SHUTDOWN_TIMEOUT 15 ＋ 緩衝 5 ＜ 關機寬限 40 秒')
        d.note(834, '看板實測：收到 shutdown／斷線後約 1 秒重連。')
        d.note(872, '漏號約 22 個是每次升級、每台看板的量測值，不是保證。')
    else:
        d.note(754, '斷線期間的漏號消失；斷線本身仍會發生', True)
        d.note(798, 'Last-Event-ID = 最後收到的 ready_seq（叫號流水號），不是取餐號碼。')
        d.note(834, '實測：重連前收到 362 個 → 下一秒 404 個；先補送，再接即時叫號。')
        d.note(872, '自動重連會帶上 Last-Event-ID；重新整理網頁不會帶，所以從「現在」收。')
    d.save(f'step-{step}.svg')


def idempotency():
    # README:301-305; mechanisms:161-170; store/idempotency.go:83-93;
    # step-5/kustomization.yaml:19-20. Critical timing is an illustration.
    d = Diagram('步驟 5：重送時，認出是同一張單',
                '解決：同一張單重送不重複建立　還剩：斷線時客人仍可能不知道結果',
                'IDEMPOTENCY_KEY=1：相同 client_ref ＋ 相同訂單內容',
                [(320, '第一次送出'), (690, '舊 Pod 關閉／回應中斷'), (1060, '重送後')],
                ['客人', '入口（Gateway）', 'Pod', '資料庫'], 970)
    d.span(0, 220, 520, '送出訂單', '帶 client_ref')
    d.span(0, 540, 850, '沒收到回應：結果未知', '訂單其實已寫入', 'problem')
    d.span(0, 870, 1195, '重送 → 拿回原號碼牌', '同一 client_ref、同一內容', 'change')
    d.span(1, 220, 520, '轉送第一次請求')
    d.span(1, 870, 1195, '重送轉給可用 Pod')
    d.span(2, 220, 520, '舊 Pod：建立訂單')
    d.span(2, 540, 850, '寫入成功後，回應中斷', '關鍵時機示意')
    d.span(2, 870, 1195, '查到原訂單 → 回傳', '不再新增', 'change')
    d.span(3, 220, 520, '存訂單與 client_ref')
    d.span(3, 540, 850, '原訂單保留')
    d.span(3, 870, 1195, '只有一張訂單', '用 client_ref 找到原訂單', 'change')
    d.note(754, '重複訂單消失：重送回傳原訂單，不變成兩張', True)
    d.text(58, 810, '沒有這個開關時（步驟 4 以前），這次重送會變成兩張訂單。', 20, True, color=RED)
    d.note(864, '預設負載下很少碰到，講師用關鍵時機實驗示範。')
    d.save('step-5.svg')


if __name__ == '__main__':
    idle()
    baseline()
    readiness()
    for step in (2, 3, 4):
        draining(step)
    idempotency()
