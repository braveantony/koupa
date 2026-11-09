> [← 目錄](README.md) ｜ [上一篇](01-overview.md) ｜ [下一篇](03-app-and-data.md)

# 2. 系統架構

```
主機（叢集外）
  podman run rush-hour
     │  HTTP
     ▼
Cilium Gateway（IP 由 Cilium 的 LoadBalancer IP 池分配）
     │  依 HTTPRoute 找到後端 Service
     ▼
Service koupa（代表「哪些 Pod 是後端」）
     ├──► koupa Pod 1 ┐
     ├──► koupa Pod 2 ├─ 3 個 replica，用 topology spread 分散在 3 個 worker
     └──► koupa Pod 3 ┘
              │
              ▼
        PostgreSQL（CloudNativePG 單一實例）
          ・存菜單和訂單
          ・用 LISTEN/NOTIFY 提醒所有 Pod「有新的叫號」
```

HTTPRoute 是 Gateway API 裡描述「哪個路徑轉給哪個 Service」的設定。圖中的 Service 表示「後端是哪些 Pod」的邏輯關係；Cilium Gateway 實際上會依 Service 目前可用的 Pod，直接把流量送到 Pod，實際封包路徑以環境驗證為準。

| 項目 | 決定 | 理由 |
|---|---|---|
| 程式語言 | Go | 映像檔小、啟動快；處理關機訊號的程式短，放得進投影片 |
| 映像檔 | distroless（不含 shell 的極簡映像檔），約 20 MB | 小、安全。`preStop` 用 Kubernetes 內建的 `sleep` 動作，不需要 shell |
| replica 數量 | 3 個，用 `topologySpreadConstraints`（Pod 分散規則）讓 3 個 Pod 盡量各在一個 worker | 只有 1 個 Pod 時，升級就是直接停機。有 3 個，升級時新舊 Pod 才會同時存在 |
| 升級方式 | Kubernetes 預設的 rolling update（逐步用新版 Pod 換掉舊版 Pod），不另外調參數 | 步驟 0 就是「什麼都沒改」的真實樣子。預設設定允許更新期間暫時多出 1 個新 Pod，它落在哪個 worker 不做限制；實際的排程和 Pod 數量以量測紀錄為準 |
| 資料庫 | CloudNativePG（簡稱 CNPG，在 Kubernetes 上管理 PostgreSQL 的 operator）單一實例，資料放 `local-path` | 資料庫不是本階段的重點。單一實例故障時服務會中斷，本階段接受這個限制 |
| 長連線 | SSE（Server-Sent Events，伺服器單向推送資料給瀏覽器的 HTTP 長連線） | 看板只需要接收；瀏覽器內建自動重連，重連時會帶上最後收到的事件編號 |
| 入口 | 01 就走 Cilium Gateway | 三個階段用同一個入口；入口會不會切斷長連線，本身就是該教的內容 |
| 連線協定 | HTTP，不設網域名稱，直接用 IP 連 | 01 不處理憑證，避免分心 |
| namespace | `koupa` | 和 App 名稱一致 |

---

> [← 目錄](README.md) ｜ [上一篇](01-overview.md) ｜ [下一篇](03-app-and-data.md)
