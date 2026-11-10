> [← 目錄](README.md) ｜ [上一篇](02-architecture.md) ｜ [下一篇](04-step-mechanisms.md)

# 3. App 功能與資料

## 3.1 三個頁面

| 頁面 | 連線 | 行為 |
|---|---|---|
| 點餐頁 | 短連線 | 一張單可以點多種飲料、每種可以點多杯，顯示合計金額。送出 `POST /orders` 後拿到號碼牌 |
| 廚房頁 | 短連線 | 定期更新「製作中」的訂單清單，按「完成」送出 `POST /orders/{id}/ready` |
| 叫號看板 | 長連線（SSE） | 整張單都完成才叫號，畫面顯示「請 0058 號取餐 🧋」 |

App 另外提供兩個量測專用的 API，頁面不會用到：

- `/api/audit`：查詢這次量測的訂單和叫號，讓 Rush Hour 對帳（見 [5.3](05-measurement.md#53-成績單的定義)）；
- `/api/whoami`：回傳處理這個請求的 Pod UID，用來校正 `preStop` 秒數（見 [5.4](05-measurement.md#54-因果時間線)）。

每個回應都帶一個 header，標明是哪個 Pod 處理的，方便對帳和畫時間線。

## 3.2 v1 與 v2

v2 比 v1 多一個真的功能：**售完標示**。

- 廚房頁可以把某個品項設成「售完」，例如珍珠煮完了；
- 點餐頁會把售完的品項變成灰色、不能點；
- 送出含有售完品項的訂單時，伺服器會拒絕。

這個功能只用到 `menu_items` 現有的 `available` 欄位，不需要變更資料表結構（那是 02 的主題）。

下單和設定售完同時發生時，以資料庫鎖的先後為準：v2 建立訂單時會先鎖住訂單裡的菜單品項，確認都可以賣才建立訂單；設定售完要更新同一筆菜單資料，所以一定排在它前面或後面。鎖的順序見[實作計畫 A7](../plan.md)。

看板的網頁只在一開始載入一次，SSE 重連時網頁不會重新載入。所以伺服器在每條 SSE 連線一開始送出的 `hello` 事件裡，會附上自己的版本。網址加上 `?debug=1` 時，看板底部的資訊列顯示「服務版本：v2」；平常不顯示。

**升級期間新舊版並存**：rolling update 期間，客人可能被分到還沒換掉的 v1 Pod。v1 不檢查售完，所以售完的品項還是點得到。這是新舊版同時運作的真實狀況，在講師手冊裡當作補充說明，替 02 鋪路；成績單不計算這一項。

## 3.3 識別碼對照

文件裡有四種「編號」，用途不同：

| 名稱 | 識別什麼 | 誰看得到 |
|---|---|---|
| `orders.id` | 一張訂單 | 系統內部 |
| `number` | 號碼牌，0001～9999 循環 | 客人、看板 |
| `ready_seq` | 一次叫號事件（訂單完成） | SSE 的事件編號 |
| `client_ref` | 客人的一次下單動作（重送時不變） | 點餐頁和伺服器 |

號碼牌用 4 位數，因為 Rush Hour 每秒送出 20 張訂單，3 位數約 50 秒就用完一輪，一次量測裡就會出現兩張同號的單。4 位數約 8 分鐘才循環一次，預期下一輪開始時，上一輪的訂單都已經完成，但仍要實測確認。號碼由資料庫的 sequence（依序配發數字的機制）產生，3 個 Pod 同時收單也不會在同一輪拿到同一號；不同輪之間本來就會重複，所以對帳一律用 `orders.id` 和 `ready_seq`，不用號碼牌。

## 3.4 資料表

```
menu_items        id │ name │ price │ available

orders            id │ number │ status（製作中／完成）│ total │ created_at
                  │ ready_seq │ ready_at │ ready_attempt │ client_ref

order_items       order_id │ menu_item_id │ quantity │ unit_price

idempotency_keys  client_ref（唯一）│ order_id │ request_hash │ created_at
```

- `orders.client_ref` 不論開關有沒有打開都會記錄，但**不設唯一限制**，所以步驟 0～4 會留下重複訂單，Rush Hour 才量得到。
- `ready_attempt` 記錄是哪一次出餐嘗試（嘗試編號，見 [5.2](05-measurement.md#52-rush-hour-的角色)）配發了 `ready_seq`，和配號寫在同一個交易裡，所以對帳時可以用它判斷是哪個嘗試編號配了號；入口重試或證據不足時，仍依實作計畫標為「未知」。
- `idempotency_keys` 只在防重複開關打開時使用（見 [4.9](04-step-mechanisms.md#49-步驟-5重送不重複下單)）。
- 02 會把甜度、冰塊加在 `order_items` 上，因為同一張單裡每一杯可以不同。

## 3.5 建表方式

用 golang-migrate（資料庫 migration 工具：照編號依序執行 SQL 檔，並記錄執行到哪一號）建表，以 Kubernetes Job 執行。

```
01/migrations/
  000001_init.up.sql   / 000001_init.down.sql    建立 4 張表
  000002_menu.up.sql   / 000002_menu.down.sql    放入 4 個品項
```

Kustomize 不會自動等「資料庫好了」才建表、「建表成功」才部署 App，所以由 `setup.sh` 分段執行：

```
setup.sh
  1. 套用 CNPG Cluster，等資料庫可以連線
  2. 建立 migration Job，等它成功（失敗就停下來並印出 log）
  3. 套用 App、Service、Gateway、HTTPRoute
```

Job 名稱帶上最新的 migration 編號，例如 `koupa-migrate-000002`。golang-migrate 會跳過已執行過的檔案，所以重跑 `setup.sh` 時，腳本會先刪掉舊的 Job 再建新的。

不讓 App 啟動時自己建表，避免 3 個 Pod 同時啟動時一起執行 migration。資料庫結構升級和部署並行時怎麼處理，留到 02 討論。

---

> [← 目錄](README.md) ｜ [上一篇](02-architecture.md) ｜ [下一篇](04-step-mechanisms.md)
