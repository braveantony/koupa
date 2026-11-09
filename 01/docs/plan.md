# Koupa 酷杯：第一階段實作計畫

> 對應設計文件：[design-v2/](design-v2/README.md)
>
> - **A 部分**：必須實作、必須測試的正確性條件（A1～A9）和量測驗證（A10～A15）。設計文件只寫「保證什麼」，這裡寫「怎麼做到」和「怎麼驗證」。
> - **B 部分**：任務拆解（Task 0～27），依階段排列，每個任務標明對應的 A 條目、檔案、介面和測試。

---

# A. 正確性條件

每一項都附「必測案例」。實作完成的定義是：必測案例都有自動化測試，而且通過。

## A1. 標記完成與配發叫號編號（設計 4.1）

`POST /orders/{id}/ready` 在同一個交易裡依序執行：

1. `SELECT … FROM orders WHERE id = $1 FOR UPDATE`。已完成就回傳原本的結果，不配號、不通知；
2. `SELECT pg_advisory_xact_lock(<叫號鎖常數>)`。全程式只有這一個常數，所有完成訂單的路徑都用它；
3. 配發 `ready_seq`（sequence），更新 `status`、`ready_seq`、`ready_at`，`ready_attempt` 寫入這次請求的嘗試編號；
4. `NOTIFY order_ready`；
5. `COMMIT`。

規則：

- 取得叫號鎖之後，不做任何網路呼叫或慢的操作；
- `ready_seq` 從 1 開始，只增不減，允許跳號（rollback 會留下空號）；
- 這個交易和所有會寫資料的交易一樣，受 A6 開頭的「所有寫入交易的共同設定」約束；
- 叫號用的 sequence 明確設定 `CACHE 1`、不循環（`NO CYCLE`）。cache 大於 1 時，不同資料庫連線會各自先保留一批號碼，即使依序取得叫號鎖，取出的號碼也可能倒退。

必測案例：

- 同一張訂單同時送兩個完成請求：只配發一個 `ready_seq`，只有一次 `NOTIFY`，兩個請求都回傳同一個結果；
- 兩張訂單同時完成，在交易中刻意加入延遲：提交順序與 `ready_seq` 大小順序一致；
- 每秒 20 張負載下，記錄等鎖時間和出餐請求的延遲（結果記錄在實測紀錄裡）。

## A2. 叫號廣播員（設計 4.2）

- 使用一條專用的資料庫連線執行 `LISTEN order_ready`，在自動提交模式下確認生效；
- 生效後，用**新的陳述式**查詢 `COALESCE(MAX(ready_seq), 0)`，當作進度；
- 由單一個迴圈處理三種觸發：收到通知、重新 `LISTEN` 之後、定時（預設每 2 秒，可設定）。每次都查 `ready_seq > 進度 ORDER BY ready_seq`，依序分派，再更新進度；
- 分派和連線登記、取消登記使用同一把 mutex，所以登記的瞬間不會漏掉正在分派的叫號；
- 分派採非阻塞寫入：連線的 channel 滿了，就把那條連線標成「溢位」，由連線自己結束。

必測案例：

- 資料庫連線中斷後重新 `LISTEN`：中斷期間完成的叫號，重新接上後會被分派；
- 刻意丟掉 `NOTIFY`（例如暫停 LISTEN 連線）：下一次定時觸發後的那次查詢就查到這個叫號並分派（分別記錄觸發、查詢完成、分派、用戶端收到的時間；後面幾段受查詢耗時和網路影響，不要求在一個週期內）；
- 分派順序一定依 `ready_seq` 遞增。

## A3. 每條看板連線的交接（設計 4.2、4.7）

每條連線有一個專屬的 buffered channel 和一個專屬的寫入 goroutine。channel 從登記起就同時扮演「暫存區」和「即時推送管道」，所以從補送切換到即時推送時，不需要另外搬移資料，也就不會有空隙。

1. 建立 channel，向廣播員登記；
2. 決定起點：
   - 第一次連線，或 `RESUME_FROM_LAST_ID` 關閉：用新的陳述式查 `COALESCE(MAX(ready_seq), 0)`；
   - 重連且開關打開：驗證 `Last-Event-ID` 必須是整數，而且介於 0 和目前最大 `ready_seq` 之間。不符合就回傳 400 並記錄，不要默默改成其他值；
3. 「已送出編號」初始化為起點；
4. 送出 `hello`（`id: <起點>`、版本），flush；
5. 用新的陳述式查出 H = 目前最大 `ready_seq`，補送 `起點 < ready_seq ≤ H`，依序送出並更新已送出編號；
6. 開始讀 channel：`ready_seq` ≤ 已送出編號的跳過，其餘照順序送出；
7. channel 溢位、寫入失敗，或收到關機通知時，結束連線並取消登記。

開關關閉時，第 2 步一律用目前最大值，斷線期間的叫號就會漏掉，這是步驟 0～2 刻意保留的行為。

必測案例：

- 登記時廣播員進度落後於 H：不重複、不漏；
- 登記時廣播員進度等於 H：不重複、不漏；
- 依 A1 的提交順序保證，正常情況下登記時的進度不會大於稍後查到的 H。用測試替身刻意製造「進度大於 H」的異常狀態：連線照樣不漏、不重複（channel 裡 ≤ 已送出編號的跳過，其餘照送），並記錄一筆警告；
- 在登記和查 H 之間有新的叫號提交：不漏、不重複；
- 收到 `hello` 後、還沒收到任何叫號就斷線，重連後補得回來；
- 資料庫裡還沒有任何叫號（起點為 0）；
- `Last-Event-ID` 格式錯誤、是負數、或大於目前最大值：回傳 400；
- 開關關閉：斷線期間的叫號確實漏掉；
- 慢看板造成溢位：連線被結束；開關打開時，重連後補送完整；開關關閉時，重連後從目前最大值開始，溢位期間的叫號不補。

## A4. SSE 事件格式（設計 4.2）

| 事件 | `id` | `data` |
|---|---|---|
| `hello` | 起點 | `{"version":"v2"}` |
| `ready` | `ready_seq` | `{"number":"0058","order_id":…}` |
| `shutdown` | 不帶 | 一行空的 `data:`（沒有 `data:` 行的事件，瀏覽器不會觸發） |
| 心跳 | 不帶 | 不是事件，是 `:` 開頭的註解行 |

`shutdown` 之前先送 `retry: <毫秒>`。

## A5. Graceful shutdown（設計 4.6）

- 設定：`preStop` 的 `sleep` 秒數＋`SHUTDOWN_TIMEOUT`＋緩衝（至少 5 秒）＜ `terminationGracePeriodSeconds`。`setup.sh` 或量測前的核對要檢查這個不等式；
- 收到 SIGTERM：
  1. 立刻呼叫 `http.Server.Shutdown`，期限為 `SHUTDOWN_TIMEOUT`。它會先關閉 listener，不再接受新連線；
  2. 用 `RegisterOnShutdown` 登記的函式只負責廣播「要關機了」（例如關閉一個共用的 channel），不等待任何寫入；每條 SSE 連線的 handler 收到通知後，自己送出 `retry:` 和 `shutdown`、flush 後結束。每次寫入都設寫入期限，卡住的看板不能拖過 `SHUTDOWN_TIMEOUT`；
  3. `Shutdown` 回傳逾時錯誤時，記錄還沒完成的請求數量，然後結束程式。

必測案例：

- 處理中的短請求在 SIGTERM 後仍能完成並回應；
- 連線正常、持續讀取的測試看板，每一台都收到 `shutdown` 才斷線，而且瀏覽器的 `EventSource` 確實觸發 `shutdown` 事件；寫入失敗或逾時的連線記錄原因，並在期限內結束；
- SIGTERM 之後的新連線被拒絕；
- 有一條看板寫入卡住時，其他看板照樣收到 `shutdown`，程式仍在 `SHUTDOWN_TIMEOUT` 內結束；
- 有一個故意卡住的請求時，程式在 `SHUTDOWN_TIMEOUT` 後結束，並留下 log。

## A6. 重送不重複下單（設計 4.8）

**所有寫入交易的共同設定**（下單不論防重複開關、出餐、設定售完都適用）：

- `transaction_timeout` 預設是 0（停用），所以要在 App 使用的資料庫角色上設定：`ALTER ROLE koupa SET transaction_timeout = '60s'`，由 migration 執行。這個設定是 PostgreSQL 17 才有的，所以 CNPG 的映像檔固定在 PostgreSQL 17 以上（確切版本待定，寫進 6.7 的依據）；
- 角色設定只對之後新登入的資料庫連線生效，已經存在的連線不受影響。所以 App 的連線池每建立一條連線，就查一次 `SHOW transaction_timeout`，不是 60 秒就關掉這條連線並記錄錯誤；App 啟動時也查一次並寫進 log，不是 60 秒就拒絕啟動。`measure.sh` 量測前核對這筆 log。migration 重跑後，要重新啟動 App，讓所有連線重新登入；
- 資料庫操作用和 HTTP 請求**分開**的 context（不會因為客人斷線而取消），期限同樣是 60 秒。這樣客人斷線時，交易照常完成或失敗，結果比較好判定；
- 每次寫入交易結束時，App 在「請求結束」log 裡記下交易結果：`committed`（提交成功）、`rolled_back`（已回滾）、`not_started`（沒開始交易）、`commit_unknown`（已送出 `COMMIT`、沒收到結果），以及送出 `COMMIT` 的時間。

下單交易使用 Read Committed，設定 `lock_timeout`（預設 5 秒）。鎖逾時的處理見第 4 點；交易逾時和連線中斷見第 5 點：

1. `INSERT INTO idempotency_keys (client_ref, request_hash, created_at) VALUES (…, now()) ON CONFLICT DO NOTHING RETURNING client_ref`；
2. 有回傳：依 A7 的順序鎖菜單、建立訂單、回填 `order_id`、提交；
3. 沒回傳：用下一個陳述式讀出這筆 key：
   - 讀到、hash 相同、`order_id` 有值：回傳原本的號碼牌；
   - 讀到、hash 不同：回傳 409；
   - 讀不到（剛好被清理）：rollback，從第 1 步重試，最多 3 次；
4. 任何一步碰到 `lock_timeout`：先 rollback，再回傳 503；
5. 碰到 `transaction_timeout`：PostgreSQL 會終止整個 session，原連線不能再 rollback，直接丟棄這條連線。碰到其他連線中斷：依資料庫驅動的規則淘汰這條不能用的連線。兩種情況都依發生時機分成：
   - 還沒送出 `COMMIT`：交易一定沒有提交，回傳 503；
   - 已經送出 `COMMIT`、還沒收到結果：**提交結果未知**。App 記一筆「提交結果未知」的 log，HTTP 連線還能寫就回傳 503（可以重試）。客人用同一個 `client_ref` 重送時，開關打開就由第 1、3 步查回原本的號碼牌或重新建立；開關關閉時可能產生重複訂單，這正是步驟 4 要展示的情況。

清理：只刪除 `order_id` 有值、`created_at < now() - interval '25 hours'` 的資料。`now()` 是交易開始的時間，所以 `created_at` 會比 key 實際提交的時間早，最多早一個交易的長度；多留 1 小時，就能保證 key 在**提交後**至少保留 24 小時（前提是 `transaction_timeout` 讓整筆交易不超過 1 小時；`statement_timeout`、`idle_in_transaction_session_timeout` 只限制單一陳述式或連續閒置，不能保證這一點）。

清理的必測案例：

- `created_at` 剛好 24 小時 59 分的 key 不會被刪；
- 下單交易被刻意拖到接近逾時，提交後立刻查得到 key，而且清理不會刪掉它；
- 用多個短陳述式加上短暫閒置交替執行，總時間超過 `transaction_timeout`：session 被終止，App 丟棄連線、回傳 503，資料庫裡沒有這筆訂單和 key；
- 用包裝資料庫連線的測試替身，真的執行 `COMMIT` 之後才回傳連線錯誤：App 記下「提交結果未知」；開關打開時，同一個 `client_ref` 重送後，資料庫裡剛好一張訂單。

必測案例：

- 同一個 `client_ref` 同時送到兩個 Pod：只建立一張訂單，兩邊回傳同一張號碼牌；
- 第一個請求還在處理時送出重送：重送等待後回傳同一張號碼牌；
- 第一個請求 rollback：重送會建立訂單；
- 內容不同：409；
- `lock_timeout`：503，而且交易已經 rollback；
- 判定重複後 key 被清理：重試後正常完成；
- 開關關閉：完全不碰 `idempotency_keys`，重送會產生重複訂單。

## A7. 鎖的順序（設計 3.2、4.1、4.8）

| 路徑 | 取得鎖的順序 |
|---|---|
| 下單 | `idempotency_keys`（開關打開時）→ `menu_items` 依 `id` 由小到大 `FOR SHARE` → 新增 `orders`、`order_items` |
| 設定售完 | `menu_items` 單筆更新；一次改多筆時依 `id` 由小到大 |
| 標記完成 | `orders` 單筆 `FOR UPDATE` → 叫號 advisory lock |

這三條路徑之間沒有反向取鎖，所以不會互相形成死鎖。資料庫仍然回報死鎖或鎖逾時（例如外鍵檢查造成的鎖）時，rollback 後回傳 503，由用戶端重送。

## A8. measure.sh（設計 5.1）

每個條件預設重複 5 次（`--runs`），每次都從第 1 步開始，各有自己的量測編號。所有模式都用同一個送單時段：送單時長 D（預設 60 秒），第 10 秒觸發變更，rollout 必須在第 D − 10 秒前完成，第 D 秒進入收尾。D 是否足夠待實測，可以用參數調整（D 至少 30 秒），但同一組比較必須相同。

1. 用 `kubectl kustomize` 產生 step-N 的完整 manifest，在輸出中把映像檔設為 v1，再一次 `kubectl apply`；
2. `kubectl rollout status` 等待完成，然後核對：
   - Deployment 的 `observedGeneration` 等於 `generation`；
   - 所有運作中的 Pod 都屬於目前的 ReplicaSet；
   - 映像檔、環境變數、`preStop`、`terminationGracePeriodSeconds` 都符合 step-N 的 manifest，而且前一步才有的設定已經移除；
   - 有打開 `GRACEFUL_SHUTDOWN` 時，A5 的時間不等式成立；其他設定只檢查實際啟用的 `preStop` 小於寬限時間；`calibrate` 另依 A11 核對；
   - `TEST_HOOKS` 和 `TEST_MUTATION` 都沒有設定（A12、A14）；
   - 每個 Pod 的啟動 log 顯示 `transaction_timeout` 是 60 秒（A6 的共同設定）；
3. 啟動時間線收集（A11）；從 Gateway 的 status 取得 IP；
4. 啟動 rush-hour，等 3 台看板都收到 `hello`，再開始送單；
5. 第 10 秒 `kubectl set image` 改成 v2；第 D − 10 秒 rollout 還沒完成，這次量測無效；
6. 第 D 秒通知 rush-hour 進入收尾（A9）；
7. 停止時間線收集，產生 `evidence/<量測編號>/`（內容見設計 5.4）。

其他模式：

| 模式 | 和 step-N 的差別 |
|---|---|
| `control` | 套用步驟 0，第 5 步什麼都不做。必須 0 失敗、0 漏號、0 重複（A13） |
| `calibrate` | 套用步驟 0，加上很長的 `preStop` 和足夠的寬限時間，另外送探測流量，量入口停止送流量的延遲（A11） |
| `rollback` | 先套用 v2，第 5 步改成 v1 |
| `drain`、`scale-down` | 第 5 步改成 drain 一個 worker，或把 replica 縮成 2（A15） |
| `all` | 講師課前驗證用：control、步驟 0～4 交錯執行，重複 5 輪 |

任何一步失敗或逾時，就停止並把這次量測標成無效。收尾階段（A9）的逾時不在此列，依 A9 的規則處理。

## A9. Rush Hour 收尾與對帳（設計 5.3）

1. 停止送新單；
2. 等已經送出的請求和重試在 Rush Hour 端結束，最多 10 秒。逾時還沒結束的列為「用戶端在途」；
3. **等伺服器端結果確定**：用戶端結束等待，不代表 App 和資料庫已經處理完。**每一次**下單和出餐嘗試（包括已經收到成功回應的，因為入口重試時，較早的那次上游嘗試可能還在處理），先看入口紀錄裡這次嘗試的**上游嘗試次數**（入口嘗試轉給 Pod 的次數，不保證真的送達，見 A11）：
   - **入口紀錄取不到，或這次嘗試在入口還沒結束**：列為「伺服器端結果未知」；
   - **0 次**：入口沒有把它轉給任何 Pod，伺服器端已確定（沒有建立任何東西）；
   - **1 次**：App 那邊最多只有一次處理。已經收到完整成功回應的，伺服器端已確定；其他的依下面其中一項判定為已確定：
     - App log 有這次嘗試的「請求結束」，而且交易結果是 `committed`、`rolled_back` 或 `not_started`；
     - 交易結果是 `commit_unknown`，而且從送出 `COMMIT` 起已經超過 `transaction_timeout`。交易最長就是這麼久，過了這個時間，資料庫裡看到的就是最終結果；
     - App log 有「收到」、沒有「請求結束」，處理它的 Pod 程式已經結束，而且從程式結束起已經超過 `transaction_timeout`。程式結束時資料庫連線會中斷，還沒提交的交易最晚在這段時間內一定會被終止；
     - App log 沒有「收到」：請求可能還在路上，之後才抵達，所以**不算**已確定；
   - **2 次以上**：入口重試過，同一個嘗試編號可能在 App 被處理不只一次，而 App 分不出是哪一次上游嘗試。所以一律列為「伺服器端結果未知」。

   最多等 `transaction_timeout` 加 10 秒。還不符合的嘗試列為「伺服器端結果未知」；
4. 廚房繼續把「製作中」的單做完，最多 10 秒，然後停止發出出餐請求。這一步新增的出餐嘗試，再照第 3 步確認一次，才進入下一步；
5. 用**單一個陳述式**查 `/api/audit`，取得這次量測（`client_ref` 開頭是量測編號）的截止游標 C，也就是這次量測已完成叫號中最大的 `ready_seq`；
6. 等每台看板收到的最大編號 ≥ C，最多 10 秒。逾時的看板列為「沒追上」；
7. 對帳：
   - 漏號：`ready_seq` 大於該看板第一個起點、≤ C、屬於這次量測，但看板沒收到；
   - 「在途未知」「還沒完成」「沒追上」「查詢失敗」分別列出，不算進漏號，也不當作零；
   - 有未確定項目時，相關成績一律寫成「已確認 N；另有 M 項未知」，不可以印成單獨的 0。例如某台看板沒追上，就寫「漏號：已確認 0；另有 12 個叫號未知」；
   - 訂單數、重複訂單、出餐結果都在第 3 步之後才查。有「伺服器端結果未知」的嘗試時，寫成「重複訂單：已確認 N；另有 M 筆伺服器端結果未知，可能增加」，不能因為等過一段時間沒查到就當成 0。這些嘗試相關的訂單如果之後才提交、配到大於 C 的叫號，也一併列為未知，不算進漏號；
   - 關鍵資料不足（例如某台看板沒有起點、audit 查詢失敗）：整次量測標成無效。

成績單分成兩區：

- **主要成績**（學生看的）：訂單數、第一次送出沒有成功回應、重試後仍沒有成功回應、重複訂單、每台看板漏號；
- **診斷資訊**（加上 `--verbose` 才顯示）：失敗嘗試的狀態和原因（A10）、斷線種類、未確定項目和原因、SIGTERM 之後才到的請求數、時間線、rollout 結果、主機資源；
- 重複 5 次後另外印彙整表：每次的數字、中位數和範圍。

## A10. 嘗試紀錄與結果分類（設計 5.3）

**紀錄**

- 每次嘗試（包括重試）都有一個嘗試編號，用 `X-Koupa-Attempt` header 送出；
- Rush Hour 每次嘗試記一筆 JSONL：量測編號、`client_ref` 或 `order_id`、嘗試編號、第幾次、送出時間、收到回應的時間、HTTP 狀態、錯誤種類（連線被拒、連線被重置、逾時）、回應 header 裡的 Pod UID，以及 Rush Hour 自己的排程延遲（實際送出時間減預定送出時間）；
- App 每次嘗試記兩筆 log：收到請求（嘗試編號、Pod UID、時間），以及請求結束（時間、HTTP 狀態、寫入和 flush 有沒有回傳錯誤、請求的 context 是否被取消）；
- App 收到 SIGTERM 和程式結束前各記一筆 log。`GRACEFUL_SHUTDOWN` 關閉時，App 收到 SIGTERM 後只寫這一行 log 就立刻結束，不等請求或看板收尾；這一行同時當作「程式結束前的標記」（A11 的 log 完整性）；這行 log 仍可能來不及收集。kubelet 記錄的 container 結束時間（Pod status 的 `finishedAt`）是 container 實際結束的另一項紀錄；SIGTERM log 和 `finishedAt` 是不同的事件，時間線上分別標示，不當成同一個時間點，兩者都沒有就依 A11 標成缺少；
- 出餐：Rush Hour 記下回應裡的 `ready_seq`（有收到回應的話）；App 在出餐交易裡記下嘗試編號、`order_id`、這次是「配發新號」還是「已經完成、回傳原結果」，以及提交結果。資料庫的 `ready_attempt` 是最終依據：等於這次的嘗試編號，就是這次配的號；`order_id` 已經有 `ready_seq` 但 `ready_attempt` 不是這次，就是別次配的；資料庫和 log 互相矛盾時標「未知」；
- 每個 Pod 的 log 收集是否完整（A11），記在證據包裡。

**看得到的狀態**（每次失敗的嘗試依下面的順序判定，第一個符合的就是它的狀態，不推論原因）：

1. Rush Hour 收到完整的 HTTP 回應，而且狀態是錯誤（4xx、5xx）→「客人收到錯誤回應」；
2. 其他情況都是「客人沒有收到完整回應」（連線被拒、被重置、逾時）。先看入口紀錄：取不到、這次嘗試在入口還沒結束，或上游嘗試次數 2 次以上 →「未知」（App 的紀錄可能來自其中任何一次上游嘗試，不能代表全部）；上游嘗試次數 0 →「沒送到 App」；
3. 上游嘗試次數剛好 1 次，才依 App 的紀錄細分：

| 細分 | 判定 |
|---|---|
| App 還沒有任何紀錄 | 歸「未知」：請求可能之後才抵達 |
| 送到了，App 沒有結束這個請求 | 有「收到」、沒有「請求結束」，而且這個 Pod 的 log 收集完整 |
| App 結束請求時寫入失敗或被取消 | 「請求結束」記錄寫入錯誤或 context 被取消 |
| App 回報寫入和 flush 成功 | 「請求結束」記錄寫入和 flush 都沒有錯誤。寫入成功只代表交給了本機的網路層，不代表對方收到 |
| 未知 | log 收集不完整，或紀錄互相矛盾 |

**原因**（只有證據齊全才寫，否則「未知」）：

| 原因 | 必要證據 |
|---|---|
| 入口在 Pod 開始終止後仍選了它 | 入口紀錄裡這次嘗試的上游嘗試次數是 1、上游位址屬於某個 Pod，而且入口開始處理這次嘗試的時間，確定晚於這個 Pod 的推估終止起點（考慮時間精度後仍晚於；分不出先後就標「未知」）。這只說明「入口選了它」，請求實際有沒有送到 App，仍看上面的狀態；入口紀錄取不到時一律「未知」 |
| 請求處理到一半，程式就結束了 | 狀態是「送到了，App 沒有結束這個請求」，App log 的「收到」早於這個 Pod 的程式結束時間（兩個來源一致），而且客人沒有收到成功回應 |

主機資源（Rush Hour 的排程延遲、主機 CPU）不列為原因，只在同一段時間超過門檻時，標成「同時觀察到資源吃緊」的環境狀態（門檻待實測）。

**出餐和叫號的檢查**

- 同一個 `order_id` 最多一個 `ready_seq`（資料庫查詢）；
- 每台看板收到的 `ready_seq` 序列嚴格遞增、不重複（從 Rush Hour 的看板紀錄檢查）。

必測案例：

- 用戶端逾時後，App 才提交訂單（用 A12 的 `order.before_commit` 暫停超過 Rush Hour 的逾時再放行）：交易照常提交，「請求結束」記錄 `committed`，收尾不會把這筆當成「沒建立」，最終對帳查得到它；暫停到收尾等待上限之後才放行，成績單列為「伺服器端結果未知」，不印出確定的 0；
- 延遲抵達：在入口和 App 之間放一個測試用代理，把某個請求扣住，直到 Rush Hour 逾時、收尾開始之後才放行。收尾不能先把這筆印成確定的 0：入口紀錄顯示它已轉給 Pod，所以列為「伺服器端結果未知」；放行後 App 建立的訂單，在下一次對帳查得到；
- 入口重試且已經回應成功：第一次上游嘗試被測試代理延遲，第二次成功回給 Rush Hour，第一次在收尾開始後才提交（防重複開關關閉）。收尾必須把這筆列為「伺服器端結果未知」，重複訂單不能印出確定的 0；
- 入口重試且其中一次已在 App 結束：第一次上游嘗試被測試代理延遲，第二次在 App 正常結束。拆成兩個案例：Rush Hour 沒收到完整回應時，狀態分類是「未知」；Rush Hour 收到完整的 4xx／5xx 時，狀態分類是「客人收到錯誤回應」。兩種情況的伺服器端結果都必須列為「未知」；
- 入口重試：第一次上游嘗試已經轉送、被測試代理延遲處理，入口再重試一次並以失敗或本機回應結束。收尾不能因為最終旗標是 `UF` 或本機回應就判定「沒送到」，必須列為「伺服器端結果未知」；
- 提交結果未知：用包裝資料庫連線的測試替身，讓 App 送出 `COMMIT` 後立刻收到連線錯誤、資料庫端稍後才完成提交。App 記錄 `commit_unknown` 和「請求結束」；收尾在 `COMMIT` 送出時間加 `transaction_timeout` 之前，不把這筆當成確定，之後查得到這張訂單；
- 用 A12 的暫停點，各造出一筆「送到了，App 沒有結束這個請求」和「App 回報寫入和 flush 成功」，狀態判定正確；
- `GRACEFUL_SHUTDOWN` 關閉時，處理中的請求遇到 SIGTERM：收集到「收到 SIGTERM」那一行且之前串流連續，判為「送到了，App 沒有結束這個請求」；刪掉那一行，判為「未知」；
- App 成功寫出 503、Rush Hour 也收到 503：只歸「客人收到錯誤回應」；
- 刪掉某個 Pod 的部分 log，相關嘗試變成「未知」，不會被誤判成「沒送到 App」。

## A11. 時間線收集與 preStop 校正（設計 5.4、4.5）

**收集**：量測期間在主機上同時收集，每一筆都記兩個時間：來源本身的事件時間（有的話），以及收集端收到的時間。

| 來源 | 指令 | 事件時間 |
|---|---|---|
| Pod 變化 | `kubectl get pods -n koupa -w -o json` | 推估終止起點（文件裡簡稱「刪除請求時間」）＝ 第一次觀察到刪除時的 `deletionTimestamp` 減 `deletionGracePeriodSeconds`（`deletionTimestamp` 本身是預計刪除的時間，不是開始終止的時間；之後寬限時間可能被縮短，所以只用第一次觀察到的值）。這是推估值，精度是秒；能取得 API server 的 audit log 時再交叉核對 |
| EndpointSlice | `kubectl get endpointslice -n koupa -l kubernetes.io/service-name=koupa -w -o json` | 沒有，只有收集端時間 |
| events | `kubectl get events -n koupa -w -o json` | 事件本身的時間戳 |
| App log | 每個 koupa Pod 一條 `kubectl logs -f --timestamps`，新 Pod 出現時自動加開 | App 自己寫的時間，以及收到 SIGTERM、程式結束的時間 |
| 入口 | Envoy 的 access log 和統計（取得方式待查，要記的欄位見下方「入口紀錄」） | access log 的開始時間 |
| 流量 | `hubble observe -n koupa -o json`：tk8s 是否已啟用 Hubble 待確認 | Hubble 的時間 |

**入口紀錄**（取得方式待查；取不到時，下面依賴它的判定一律「未知」）：

- 查明這個 Cilium 版本的入口預設會不會重試、能不能在 HTTPRoute 或 Cilium 設定裡關掉（待查）。能關掉的話，量測時關閉入口重試，上游嘗試次數最多 1 次，A9 的「未知」會少很多；關不掉就照實記錄；

- access log 每筆要有：開始時間、路徑、HTTP 狀態、上游位址、回應旗標（response flags）、回應細節（response code details）、上游嘗試次數（`%UPSTREAM_REQUEST_ATTEMPT_COUNT%`，表示入口嘗試轉送幾次，不保證已送達 App）、總耗時，以及 Rush Hour 送的 `X-Koupa-Attempt`（用來和嘗試紀錄對上）；
- 回應旗標用來輔助判定原因，以實際 Envoy 版本的定義為準。和本課程最相關的是：沒有健康的上游（`UH`）、連上游失敗（`UF`）、上游連線中途被關（`UC`）、上游逾時（`UT`）、重試次數用完（`URX`）、用戶端先斷線（`DC`）；
- 上游嘗試次數要分清楚三種情況：明確記錄為 0、依紀錄格式的規格省略這個欄位（例如 gRPC access log 在從未嘗試上游時會省略），以及欄位沒設定或收集失敗。前兩種才能當成「0 次」，分不出來就標「未知」；
- 上游嘗試次數大於 1，表示入口內部重試過，最終的上游位址不代表唯一的選擇。這時「入口選了哪個 Pod」標「未知」，除非取得每次上游嘗試的紀錄；
- 統計：量測開始和結束各抓一次 koupa 後端 cluster 的計數器，取差值。至少包括上游請求數、5xx 數、重試數、上游連線帶著進行中請求被遠端關閉的次數（確切的計數器名稱依實際版本確認）。

**log 完整性**：每個 Pod 記錄 log 收集的開始和結束時間、Pod UID、串流錯誤；App 啟動時和程式結束前各寫一筆標記。符合下面全部條件，才算這個 Pod 的 log「涵蓋」某次嘗試：

- 收集開始時間早於 App 的啟動標記（或 `kubectl logs` 從頭讀起有讀到啟動標記）；
- 從啟動標記到程式結束前的標記（或到量測結束時 Pod 仍在執行），收集都連續、沒有串流錯誤。「程式結束前的標記」依開關不同：`GRACEFUL_SHUTDOWN` 打開時是收尾完成後的最後一行；關閉時就是「收到 SIGTERM」那一行（之後程式立刻結束，不會再有其他 log）。沒收到這一行時，`finishedAt` 只能補程式結束的時間，不能證明 log 完整；曾經中斷的話，要能證明缺口已經完整補讀（例如用 `--since-time` 重讀並比對行數），否則不算涵蓋；
- 這次嘗試的時間落在收集期間內。

不符合時，這個 Pod 相關的判定（例如「沒送到 App」）一律「未知」。

- 某個 Pod 的 log 串流在程式結束前就中斷，標成「log 不完整」；
- 取不到的來源在時間線上標註缺少，不影響其他來源；
- 合併後輸出 `timeline.jsonl` 和一張文字版時間線。只知道區間的事件畫成區間。

**`calibrate` 模式**

1. 套用步驟 0，`preStop` 設 20 秒，`terminationGracePeriodSeconds` 設 45 秒，讓舊 Pod 在終止後照常服務一段時間；
2. 除了一般負載，另外以每秒 50 次送探測請求 `GET /api/whoami`（回傳 Pod UID），每個請求都有編號；
3. 觸發 rollout。對每個被終止的 Pod，入口「停止選它」的時間只能確定在一個區間裡：
   - **下界** L：入口最後一次選它的時間不會早於 L。有逐次上游嘗試的紀錄時，L 是最後一次選到它的那次嘗試的入口時間；沒有的話，用最後一次命中的那個探測請求在 Rush Hour 端的**送出時間**（入口一定在送出之後才選，所以這是保守的下界）。App 收到的時間另外記錄，但不當成入口選取的時間；
   - **上界** U：用下面兩種方法之一取得，取不到就沒有上界；
   - 延遲的校正值用**上界**算：U − 刪除請求時間。只有下界的樣本不能用來校正；
4. 上界的取法（優先用第一種）：
   - **直接證據**：只有涵蓋所有入口實例的**端點或設定更新紀錄**，能證明這個 Pod 從某個時間起已經不在可選的後端裡，那個時間就是 U。逐次上游嘗試的紀錄只能用來精確定出下界 L：之後沒再選到它，可能只是剛好沒選到，不能當成直接證據。紀錄缺漏或涵蓋範圍不明時，直接證據判為「未知」；
   - **模型推估**：在「入口每次獨立挑選後端，而且命中機率維持在刪除前的 p」這個模型假設下，從 L 之後連續 k 次探測都沒命中、而 (1 − p)ᵏ ≤ 0.001，第 k 次探測在 Rush Hour 端**收到回應的時間**就是 U（入口一定在這之前就已經選好後端，所以是保守的上界）。p 取刪除請求前 10 秒這個 Pod 的命中比例。這是模型下的估計，不是已證明的事實，所以：
     - 先排除明顯違反假設的樣本：刪除前的逐次命中序列用連段檢定（runs test）檢查相鄰命中是否相關，刪除後到 L 之間的命中比例和刪除前比較，有明顯差異就不用這個方法；檢定沒有顯著差異不代表假設成立，只代表沒有找到反證；
     - 證據包記錄模型、p、k、樣本數和檢定結果；
     - 刪除前的探測樣本少於 100 次、無法計算或檢定，或 p = 0 時，不使用模型推估，上界標「未知」。樣本數門檻和檢定的顯著水準（預設 0.05）事先寫進設定；
5. 樣本有效的條件（預先固定，全部成立才算有效）：
   - 刪除請求之後，這個 Pod 至少命中過一次（否則分不出「入口很快拿掉它」和「根本沒被選到」）；
   - 取得了上界 U，而且 U 早於 `preStop` 結束至少 5 秒（否則 20 秒不夠長，把 `preStop` 和寬限時間一起加長後重新量測）；
   - 探測請求不重用用戶端連線（每次新連線），並記錄這個設定；入口和 Pod 之間的連線重用無法由用戶端控制，取得入口紀錄時再交叉核對；
6. 重複到至少有 5 個有效樣本。步驟 1 的 `preStop` 秒數取有效樣本中「U − 刪除請求時間」的最大值，再加緩衝（緩衝秒數待實測後訂），寫明這是本環境的候選值，以及上界是直接證據還是模型推估。有效樣本不足 5 個，就不產生候選值；這時步驟 1 改用講師手冊列的保守值，成績單標示「`preStop` 未經校正」。每個樣本的下界、上界、緩衝都保留在證據包；
7. 之後每次量測都統計「SIGTERM 之後才送到這個 Pod 的請求」（設計 5.3）。步驟 1 以後只要不是 0，就標成「候選值需要調查」：先用入口紀錄和連線資訊確認這些請求是新選的流量、既有連線上的請求，還是紀錄延遲，再決定要不要重新校正。

必測案例：

- 來源完整時，時間線上每個被終止的 koupa Pod 都有刪除請求、SIGTERM、程式結束三個點；來源缺失時，改標「缺少」或可以證實的時間區間，不補造精確時間；
- 某個來源取不到時，時間線照常產生，並標註缺少的來源；
- `calibrate` 的寬限時間大於 `preStop`，探測請求的回應都帶有 Pod UID。

## A12. 關鍵時機暫停點（設計 5.5）

**控制方式**

- 只有 `TEST_HOOKS=1` 時，App 才在另一個埠（例如 9090）開啟控制介面。這個埠不在 Service 和 HTTPRoute 裡，只能用 `kubectl port-forward` 連；
- 測試腳本對**所有** koupa Pod 設定同樣的暫停點，不管請求被分到哪個 Pod 都會停；
- 暫停點會回報「有請求停在這裡」，回報內容包括 Pod UID、Pod 名稱和這次請求的嘗試編號。腳本收到回報才進行下一步，不靠固定的等待時間；需要刪除 Pod 的時機，一律刪除回報裡的那個 Pod，並確認之後的 SIGTERM 和放行紀錄都屬於同一個 Pod 和同一次嘗試；
- 控制動作只有 `arm`（啟用）、`release`（放行）。斷線一律由測試用戶端自己關閉連線，不由伺服器端模擬。

**暫停點**

| 名稱 | 位置 |
|---|---|
| `order.before_commit` | 下單交易已建立訂單、還沒送出 `COMMIT` |
| `order.after_commit` | 下單交易提交後、寫出回應前 |
| `request.mid` | 短請求處理中（交易開始前） |
| `sse.after_hello` | 看板連線完成登記、決定起點、成功 flush `hello` 之後，查補送上界 H 之前（A3 第 4、5 步之間） |

**各時機的順序**（對應設計 5.5 的表格）：

1. 提交後重送：`arm order.after_commit` → 用戶端送單 → 等回報 → 用戶端關閉連線 → `release` → 用戶端用同一個 `client_ref` 重送 → 查資料庫這個 `client_ref` 的訂單數；
2. 處理中收到 SIGTERM：`arm request.mid` → 用戶端送單 → 等回報 → `kubectl delete pod <回報裡的 Pod> --wait=false`（刪除前先開始 watch 這個 Pod），之後依開關分兩條流程：
   - `GRACEFUL_SHUTDOWN` 關閉：不等 SIGTERM log，也不 `release`（程式會立刻結束，暫停中的請求跟著中斷）。觀察到 container 結束或 Pod 已經被刪除後，檢查用戶端沒有收到完整的成功回應；沒取得 `finishedAt` 時，結束時間標為缺少，不影響這項檢查；
   - `GRACEFUL_SHUTDOWN` 打開：等這個 Pod 的 App log 出現 SIGTERM → 等 2 秒（小於 `SHUTDOWN_TIMEOUT`）→ `release` → 檢查用戶端收到成功回應；
3. 交接時有新叫號：`arm sse.after_hello` → 看板連線 → 等回報 → 標記一張單完成 → `release` → 檢查看板收到這個叫號剛好一次。開關關閉和打開各跑一次；
4. 斷線期間有新叫號：看板連線並收到 `hello` → 看板關閉連線 → 標記一張單完成 → 看板帶著 `Last-Event-ID` 重連 → 檢查有沒有收到這個叫號；
5. 寫入被擋住：
   - 三台測試看板從叢集內的測試 Pod 直接連到同一個 koupa Pod 的 IP，不經過入口，避免入口的緩衝吸收掉資料。一台連上後停止讀取，另外兩台正常讀；
   - 測試模式下持續推送填充事件（SSE 註解行）。App 在每次寫入前後各記一筆（連線、寫入編號、時間），由獨立的監測 goroutine 判定「某次寫入開始後超過 1 秒還沒返回」，記一筆「寫入被擋住」；
   - 看到這筆紀錄後 `kubectl delete pod <受測 Pod> --wait=false` → 等這個 Pod 的 App log 出現 SIGTERM，從那時開始計時 → 檢查另外兩台都收到 `shutdown`，程式在 `SHUTDOWN_TIMEOUT` 加上記錄的量測誤差內結束，而且從刪除到程式結束沒有超過寬限時間 → 最後等 Pod 結束；
   - 另外用 Go 測試驗證寫入期限：`httptest` 伺服器加上一個連上後不讀取的原始 TCP 用戶端，確認 `ResponseController.SetWriteDeadline` 沒有回傳錯誤，而且被擋住的寫入在期限後回傳逾時錯誤。

每個時機的每一步都設逾時，逾時就停止並保存紀錄。

必測案例：設計 5.5 表格的每一列，在對應的開關設定下，實際結果都符合預期欄位。

## A13. 對照組（設計 5.1）

- `control` 模式必須 0 失敗、0 漏號、0 重複，而且**所有相關項目都可以判定**。有任何「未知」，對照組就算未通過，不能當成 0；
- 步驟 0 的自然負載，5 次中必須至少觀察到一種預定的問題（送出沒有成功回應、漏號、重複訂單）；
- 每一種問題，另外分成兩種證據，成績單和講義不可以混用：

| 問題 | 自然負載的證據 | 受控實驗的證據 |
|---|---|---|
| 送出沒有成功回應 | 步驟 0 的自然負載 | A12 第 2 項 |
| 漏號 | 步驟 0 的自然負載 | A12 第 4 項 |
| 重複訂單 | 步驟 0 的自然負載（可能很少） | A12 第 1 項 |

- 某個問題在步驟 0 的自然負載裡沒觀察到，就只能說「對應的機制在受控實驗中有效」，**不能**說「這一步在自然負載下改善了這個問題」；

- 任一項不成立：`measure.sh` 以非 0 的結束狀態停止正式比較，並提示先調查原因（可能在工具、環境或 App）。講師可以加上 `--diagnostic` 繼續收集資料，這時每張成績單都標示「診斷模式：對照組不成立，不可用於比較」。

## A14. 量測工具的驗證（設計 5.6）

- **已知答案測試**：對帳程式寫成純函式，輸入是資料庫的叫號集合、每台看板的起點和收到的序列、截止游標 C，以及各看板是否追上 C、各資料來源是否完整、相關嘗試的伺服器端結果是否確定；輸出是已確認的漏號、重複，以及證據不足而列為未知的範圍。測試資料涵蓋：單筆漏號、多筆漏號、重複收到、斷線後補回、起點邊界（等於起點的不算）、截止邊界（等於 C 的要算）、空集合；
- **實際注入**：`TEST_MUTATION` 只在測試時設定：
  - `drop-ready=<n>`：廣播員故意不把某 n 個叫號送給某一條連線，App log 記下是哪幾個；
  - `duplicate-order=<n>`：故意對 n 個 `client_ref` 多建一張訂單（不經過 `idempotency_keys`）。

  對帳結果和 App log 逐筆比對。「注入了幾個」和「最後漏了幾個」分開記錄：注入的叫號如果在重連後被補回，就不算漏號；
- **協定**：用無頭瀏覽器（例如 Playwright 控制的 Chromium）開看板頁面，和 Rush Hour 的看板同時連線，記錄兩條連線各自連到的 Pod 和起點，在兩者都連著的共同區間內逐筆比較 `ready_seq`，必須一致。`shutdown` 依各連線所在 Pod 的終止事件分別驗證；要逐筆比較 `shutdown`，就讓兩條連線直接連到同一個受測 Pod；另外用 `curl -N` 保存原始 SSE 串流，檢查欄位、空行和事件格式（`hello`、`ready`、`retry:`、`shutdown` 的空 `data:`），結果放進證據包；
- **和入口的紀錄對帳**：限定同一段時間、`/orders` 路徑、最終回應（不算入口內部的重試），Rush Hour 和 access log 依路徑和嘗試編號逐筆比對；Envoy 統計的計數器是整個 cluster 的，沒有路徑維度，所以用它涵蓋的全部流量比較，另外列出非 `/orders` 的流量、入口自己產生的回應、逾時等無法拆分的差額。差額依「客人端逾時」「入口內部重試」「其他路徑或其他流量」「沒經過入口」分類列出，對不上的部分照實保留（A11 確認入口紀錄的取得方式之後才做）；
- **另一套工具**：k6 或 vegeta 以相同的速率、逾時和重試設定，對步驟 0 和步驟 2 各量 5 次「送出沒有成功回應」，比較兩套工具看到的增減方向和範圍。兩邊不一致就調查原因；兩邊一致但沒有改善，也照實記錄，並回頭檢討這一步的假設。

## A15. 其他 Pod 終止的情況（設計 5.7）

**drain**

1. 量測前列出目標 worker 上的所有 Pod，記進證據包。目標必須有 koupa Pod、沒有資料庫 Pod，而且沒有使用 `emptyDir` 存重要資料的其他 Pod；不符合就換一個 worker 或停止；
2. 記錄受影響的 koupa Pod 數（topology spread 下預期是 1 個）；
3. 第 5 步執行 `kubectl drain <worker> --ignore-daemonsets`。不加 `--delete-emptydir-data`，遇到需要它的 Pod 就停止並回報；
4. 量測結束或中途失敗都要執行 `kubectl uncordon <worker>`，並確認節點可排程、Deployment 回到 3 個可用 Pod。drain 不會把替代 Pod 搬回原節點，所以下一次量測前要重新平衡：刪除擠在同一個 worker 上的 Pod，讓它重新排程，再核對 3 個 Pod 的節點分布。分布不符合下一次量測的前置條件，就停止並回報，不假設刪除一次就會恢復。

**scale-down**

1. 第 5 步執行 `kubectl scale deploy/koupa --replicas=2`；
2. 量測結束或中途失敗都要恢復成 3，並確認 3 個 Pod 都可用。

兩種都在步驟 4 的設定下量 5 次，比較和升級相同的條件（不漏號、不重複、送出沒有成功回應的張數），數字和升級分開報告。

---

# B. 任務拆解

> **給執行任務的 agent：** 必須搭配 superpowers:subagent-driven-development（建議）或 superpowers:executing-plans，逐項執行。步驟用 checkbox（`- [ ]`）追蹤。

**目標：** 做出 Koupa App v1／v2、Rush Hour、Kubernetes manifest 和 `measure.sh`，讓 A 部分的每個必測案例都有自動化測試並通過，並產出第一份實測紀錄。

**架構：** 一個 Go module 編出兩個程式：`app`（Koupa）和 `rush-hour`。判定邏輯（收尾對帳、狀態分類、漏號對帳、`preStop` 校正）都寫成純函式，各自有已知答案測試；需要資料庫的測試連到 podman 跑的 PostgreSQL 17；需要叢集的測試由 `measure.sh` 和 `experiments/` 底下的腳本執行。

**技術：** Go 1.26、pgx v5（`pgxpool`）、`log/slog` JSON log、golang-migrate、PostgreSQL 17（CNPG）、Kustomize、bash、Playwright（Node）、k6。

**規格：** [design-v2/](design-v2/README.md) 和本文件的 A 部分。每個任務都標了對應的 A 條目，必測案例以 A 部分的文字為準，這裡不重抄。

## 全域限制

每個任務都隱含遵守這一節。

- 程式碼、註解、commit 訊息用英文；文件、講義、成績單上給學生看的文字用繁體中文（台灣用語），一律用「連線」，不用「連接」。
- PostgreSQL 17 以上；CNPG 映像檔固定 tag，版本寫進 6.7。
- `ALTER ROLE koupa SET transaction_timeout = '60s'`，由 migration 執行；連線池每條新連線和 App 啟動時都檢查。
- 下單交易 Read Committed、`lock_timeout` 預設 5 秒。資料庫操作用和 HTTP 請求分開的 context，期限 60 秒。
- 叫號 sequence：`START 1`、`CACHE 1`、`NO CYCLE`。全程式只有一個叫號 advisory lock 常數：`const readyLockKey int64 = 0x6B6F757061`（"koupa"）。
- `idempotency_keys` 清理條件：`order_id IS NOT NULL AND created_at < now() - interval '25 hours'`。
- 時間不等式：`preStop` 秒數＋`SHUTDOWN_TIMEOUT`＋5 秒 ＜ `terminationGracePeriodSeconds`。
- Rush Hour 預設：每秒 20 張、每張 1～4 杯、失敗重試 1 次、廚房 1～3 秒出餐、3 台看板。
- 送單時段：D 預設 60 秒（至少 30），第 10 秒觸發變更，第 D − 10 秒前 rollout 要完成，第 D 秒收尾。每個條件預設 5 次。
- `calibrate`：`preStop` 20 秒、寬限 45 秒、探測每秒 50 次、模型門檻 (1 − p)ᵏ ≤ 0.001、刪除前樣本至少 100 次、顯著水準 0.05、U 至少早於 `preStop` 結束 5 秒、至少 5 個有效樣本。
- 有未知項目時，成績寫成「已確認 N；另有 M 項未知」，不可以印成單獨的 0。
- 正式量測時 `TEST_HOOKS`、`TEST_MUTATION` 必須沒有設定。
- 映像檔：`ghcr.io/braveantony/koupa:v1`、`:v2`、`ghcr.io/braveantony/rush-hour:<版本>`；distroless、`linux/amd64`、公開、不用 `latest`。
- namespace `koupa`；技術名稱 `koupa`、`rush-hour`。
- 標頭名稱：`X-Koupa-Attempt`（嘗試編號，Rush Hour 送）、`X-Koupa-Pod-UID`（App 回）。

## 檔案結構

```
01/
├── go.mod                      module koupa
├── Makefile                    test、test-short、images、push
├── internal/                   兩個程式共用
│   ├── wire/                   標頭名稱、SSE 事件名稱、AttemptID 格式
│   └── testdb/                 整合測試用：每個測試一個全新資料庫
├── app/
│   ├── main.go                 讀設定、組裝、啟動、關機
│   ├── config/                 環境變數
│   ├── applog/                 slog JSON log、固定事件名稱
│   ├── store/                  連線池、寫入交易包裝、訂單、出餐、售完、清理
│   ├── broadcast/              叫號廣播員
│   ├── board/                  SSE handler、交接
│   ├── server/                 路由、頁面、/api/*、中介層、關機流程
│   ├── hooks/                  TEST_HOOKS 暫停點、TEST_MUTATION
│   └── web/                    三個頁面（embed）
├── rush-hour/
│   ├── main.go
│   ├── record/                 JSONL 紀錄格式
│   ├── load/                   固定速率客人、廚房、探測
│   ├── sse/                    依 EventSource 規則的看板用戶端
│   ├── verdict/                純函式：收尾對帳、狀態分類、漏號對帳、成績單
│   ├── calib/                  純函式：preStop 校正
│   └── winddown/               收尾流程
├── testproxy/                  測試用 TCP 代理（資料庫的提交結果未知、HTTP 延遲）
├── migrations/
├── k8s/{db,migrate,base,steps/step-1..4,idle-lab,calibrate,test-hooks}/
├── scripts/                    test-db.sh、collect-timeline.sh、check-manifests.sh
├── experiments/                A12 關鍵時機、A10 入口代理、A14 交叉檢查
├── setup.sh
├── measure.sh
├── README.md
└── TEACHER.md
```

## 審閱重點

規格隱含、但各任務的測試不一定涵蓋，最可能出問題的五種情況。每一項的測試已經加進負責的任務。

1. **App 重啟後廣播員的進度**：新 Pod 的進度從資料庫目前最大值開始，不會把舊叫號重送給新連線的看板（Task 8 `TestBroadcasterStartsAtCurrentMax`）。
2. **號碼牌循環**：`number` 到 9999 後回到 0001，不是 0000，也不會因 sequence 用完而報錯（Task 2 `TestOrderNumberWrapsTo0001`）。
3. **送單時段內 rush-hour 自己的排程落後**：排程延遲超過 1 秒時照實記錄，不補送、不擠在一起送（Task 15 `TestSchedulerDoesNotBurstAfterStall`）。
4. **看板頁面在 `hello` 之前就斷線**：瀏覽器沒有 `Last-Event-ID`，重連視為第一次連線，不回 400（Task 9 `TestNoLastEventIDIsFirstConnect`）。
5. **`measure.sh` 中途被 Ctrl-C**：drain 過的節點一定 uncordon、replica 一定恢復、背景收集程序一定結束（Task 23 `test-measure-trap.sh`）。

---

## 階段 0：基礎

### Task 0：專案骨架與測試資料庫

**檔案：**
- 建立：`go.mod`、`Makefile`、`.gitignore`（含 `evidence/`）、`scripts/test-db.sh`、`internal/testdb/testdb.go`、`internal/testdb/testdb_test.go`、`internal/wire/wire.go`

**介面：**
- 產出：`testdb.New(t *testing.T) *pgxpool.Pool`：在 `KOUPA_TEST_DSN` 指的伺服器上，從套用過全部 migration 的範本資料庫複製一個新資料庫，測試結束時刪除。`-short` 時呼叫 `t.Skip`；沒設定 `KOUPA_TEST_DSN` 又不是 `-short` 時 `t.Fatal`（不要默默跳過）。
- 產出：`wire.HeaderAttempt = "X-Koupa-Attempt"`、`wire.HeaderPodUID = "X-Koupa-Pod-UID"`、`wire.NewAttemptID(runID string, seq uint64, try int) string`，格式 `<runID>-a<seq>-t<try>`。

- [ ] **Step 1：** `git init`（在 `01/`），建立 `go.mod`（`module koupa`、`go 1.26`）。
- [ ] **Step 2：** 寫 `scripts/test-db.sh`：用 podman 啟動 `postgres:17`（固定 digest），埠 `127.0.0.1:55432`，等可以連線，以 superuser 建 `koupa` 角色和 `koupa_template` 資料庫，用 golang-migrate 套用 `migrations/`，印出 `export KOUPA_TEST_DSN=…`。重跑時先移除舊 container。
- [ ] **Step 3：** 寫失敗的測試 `TestNewGivesIsolatedDatabase`：兩次 `testdb.New` 拿到的資料庫名稱不同；在第一個建表，第二個看不到。
- [ ] **Step 4：** `go test ./internal/testdb/` → FAIL（`New` 未定義）。
- [ ] **Step 5：** 實作 `testdb.New` 和 `wire`。
- [ ] **Step 6：** `eval "$(scripts/test-db.sh)" && go test ./internal/...` → PASS。`Makefile` 的 `test` 目標先跑 `test-db.sh` 再 `go test -race ./...`。
- [ ] **Step 7：** commit `chore: scaffold module and test database`。

### Task 1：環境盤點（調查任務，無 TDD）

這個任務回答 A11 和 6.4 的「待查」。後面依賴入口紀錄的任務（Task 19、22、25、26）都要先讀它的結果。

**檔案：**
- 建立：`docs/env-findings.md`
- 修改：`docs/design-v2/06-reference.md`（6.4、6.7 補上版本和連結）

- [ ] **Step 1：** 記錄版本：Kubernetes、CRI-O、Cilium（`cilium version`）、Cilium 內建 Envoy、Gateway API CRD、主機 podman。指令和輸出原文貼進 `env-findings.md`。
- [ ] **Step 2：** 找出 Envoy 的取得方式：admin 介面（config dump、`/stats`）怎麼連、access log 能不能開、能不能自訂格式加上 `%UPSTREAM_REQUEST_ATTEMPT_COUNT%`、`%RESPONSE_FLAGS%`、`%RESPONSE_CODE_DETAILS%`、`%REQ(X-KOUPA-ATTEMPT)%`。每一項寫「可以／不行／部分」和證據。
- [ ] **Step 3：** 從 config dump 找出 koupa 路由的重試政策、各種逾時（route timeout、idle timeout、stream idle timeout、max stream duration）。查該 Cilium 版本的 Gateway API 轉換原始碼，確認重試能不能用 HTTPRoute 或 Cilium 設定關掉。
- [ ] **Step 4：** 確認 Hubble 是否啟用、`hubble observe` 能不能在主機上執行。
- [ ] **Step 5：** 選定 CNPG operator 版本和 PostgreSQL 17 映像檔 tag 與 digest。
- [ ] **Step 6：** 結論表：A11 每一個入口欄位、A14 三方對帳，標「可做／降級做法／做不到（判定一律未知）」。更新 6.4、6.7。
- [ ] **Step 7：** commit `docs: record environment findings`。

---

## 階段 1：App

### Task 2：Migration（A1、A6、3.4、3.5）

**檔案：**
- 建立：`migrations/000001_init.{up,down}.sql`、`migrations/000002_menu.{up,down}.sql`、`migrations/000003_role_settings.{up,down}.sql`、`app/store/schema_test.go`

**介面：**
- 產出資料表和欄位照 3.4。另有：`order_number_seq`（`MINVALUE 1 MAXVALUE 9999 CYCLE`）、`ready_seq_seq`（`START 1 CACHE 1 NO CYCLE`）、`orders.client_ref` 有索引但**不唯一**、`idempotency_keys.client_ref` 是主鍵。
- `000003` 執行 `ALTER ROLE koupa SET transaction_timeout = '60s'`；down 執行 `RESET`。

- [ ] **Step 1：** 寫失敗的測試：
  - `TestReadySeqSequenceOptions`：查 `pg_sequences`，`cache_size = 1`、`cycle = false`、`start_value = 1`；
  - `TestOrderNumberWrapsTo0001`：把 `order_number_seq` `setval` 到 9999，下一個 `nextval` 是 1，App 格式化成 `"0001"`；
  - `TestClientRefNotUnique`：同一個 `client_ref` 插兩張訂單成功；
  - `TestRoleTransactionTimeout`：用 `koupa` 角色新登入，`SHOW transaction_timeout` 是 `1min`；
  - `TestMenuSeeded`：4 個品項，名稱照 6.6。
- [ ] **Step 2：** `go test ./app/store/ -run 'Schema|Seq|Wraps|ClientRef|Role|Menu'` → FAIL。
- [ ] **Step 3：** 寫 SQL。
- [ ] **Step 4：** 重跑 `scripts/test-db.sh`（重建範本），同一指令 → PASS；另外 `migrate down` 到 0 再 `up` 一次成功。
- [ ] **Step 5：** commit `feat(db): add schema, sequences and role timeout`。

### Task 3：設定、log、連線池（A6 共同設定、A10 紀錄）

**檔案：**
- 建立：`app/config/config.go`、`app/config/config_test.go`、`app/applog/applog.go`、`app/applog/applog_test.go`、`app/store/pool.go`、`app/store/pool_test.go`

**介面：**
- 產出：`config.Load(getenv func(string) string) (Config, error)`。欄位和環境變數：`DatabaseURL`（`DATABASE_URL`）、`PodUID`（`POD_UID`）、`PodName`（`POD_NAME`）、`Version`（建置時 `-ldflags -X`，值 `v1` 或 `v2`）、`GracefulShutdown`（`GRACEFUL_SHUTDOWN`）、`ShutdownTimeout`（`SHUTDOWN_TIMEOUT`，預設 15s）、`ResumeFromLastID`（`RESUME_FROM_LAST_ID`）、`IdempotencyKey`（`IDEMPOTENCY_KEY`）、`SSEHeartbeat`（`SSE_HEARTBEAT`，0 表示關閉）、`SSERetry`（`SSE_RETRY`，預設 1s）、`LockTimeout`（`LOCK_TIMEOUT`，預設 5s）、`DBTimeout`（固定 60s）、`BroadcastPoll`（`BROADCAST_POLL_INTERVAL`，預設 2s）、`WriteDeadline`（`SSE_WRITE_DEADLINE`，預設 2s）、`TestHooks`（`TEST_HOOKS`）、`TestMutation`（`TEST_MUTATION`，解析成 `Mutation{DropReady int; DuplicateOrder int}`）。布林只接受 `0`、`1`、空字串，其他值回錯誤。
- 產出：`applog.New(w io.Writer, podUID, podName, version string) *slog.Logger`。事件名稱常數：`EvStartup`、`EvTxTimeoutCheck`、`EvRequestReceived`、`EvRequestEnd`、`EvSigterm`、`EvExit`、`EvCommitUnknown`、`EvWriteStart`、`EvWriteEnd`、`EvWriteBlocked`、`EvMutationDropReady`、`EvMutationDuplicateOrder`、`EvHookPaused`、`EvShutdownTimeout`、`EvProgressAheadOfH`。每筆都有 `ts`（RFC3339Nano）、`event`、`pod_uid`。
- 產出：`store.NewPool(ctx, cfg config.Config, log *slog.Logger) (*pgxpool.Pool, error)`：`AfterConnect` 查 `SHOW transaction_timeout`，不是 `1min` 就回錯誤並記錄，讓這條連線被丟棄；回傳前先取一條連線確認，失敗就讓 App 拒絕啟動。

- [ ] **Step 1：** 寫失敗的測試：
  - `TestLoadDefaults`、`TestLoadRejectsBadBool`（`GRACEFUL_SHUTDOWN=yes` 回錯誤）、`TestLoadMutation`（`drop-ready=3` → `DropReady == 3`）；
  - `TestLogLineShape`：輸出一行 JSON，包含 `ts`、`event`、`pod_uid`；
  - `TestPoolRejectsWrongTimeout`：測試裡對資料庫 `ALTER ROLE koupa SET transaction_timeout = '0'`，`NewPool` 回錯誤，log 有一筆 `EvTxTimeoutCheck` 且 `ok=false`；
  - `TestPoolAcceptsCorrectTimeout`。
- [ ] **Step 2：** `go test ./app/config/ ./app/applog/ ./app/store/ -run 'Load|LogLine|Pool'` → FAIL。
- [ ] **Step 3：** 實作上述三個函式。
- [ ] **Step 4：** 同一指令 → PASS。
- [ ] **Step 5：** commit `feat(app): config, structured log and checked pool`。

### Task 4：寫入交易包裝與測試用 TCP 代理（A6 第 5 點、A10 提交結果未知）

**檔案：**
- 建立：`testproxy/tcp.go`、`testproxy/tcp_test.go`、`app/store/tx.go`、`app/store/tx_test.go`

**介面：**
- 產出：`testproxy.NewTCP(upstream string) *TCP`，方法 `Addr() string`、`OnClientMessage(func(msg []byte) Action)`、`Close()`。`Action` 有 `Forward`、`ForwardThenCutClient`（轉送給資料庫，然後立刻關掉用戶端那一側，丟掉資料庫的回覆）。代理解析 PostgreSQL 前端訊息邊界（1 位元組型別＋4 位元組長度），讓 hook 一次看到一則完整訊息。
- 產出：`type TxOutcome string`，值 `committed`、`rolled_back`、`not_started`、`commit_unknown`。`type TxResult struct { Outcome TxOutcome; CommitSentAt time.Time }`。
- 產出：`store.RunWriteTx(ctx context.Context, pool *pgxpool.Pool, opts TxOptions, fn func(ctx context.Context, tx pgx.Tx) error) (TxResult, error)`。`TxOptions{LockTimeout time.Duration}`，用 `SET LOCAL lock_timeout`。`ctx` 由呼叫端用 `store.DBContext()` 建立：`context.WithTimeout(context.Background(), 60*time.Second)`，**不**從 HTTP 請求衍生。送出 `COMMIT` 前記下 `CommitSentAt`；`COMMIT` 回傳連線錯誤就是 `commit_unknown`。
- 產出：`store.IsLockTimeout(err) bool`（SQLSTATE `55P03`）、`store.IsDeadlock(err) bool`（`40P01`）。

- [ ] **Step 1：** 寫失敗的測試：
  - `TestCommitted`、`TestRolledBackOnFnError`、`TestNotStartedWhenBeginFails`；
  - `TestCommitUnknownButCommitted`：App 經過 `testproxy` 連資料庫，hook 遇到內容含 `commit`（不分大小寫，`Q` 或 `P` 訊息）時 `ForwardThenCutClient`。`RunWriteTx` 回 `commit_unknown`、`CommitSentAt` 非零；直接連資料庫查得到這筆資料；
  - `TestTransactionTimeoutKillsSession`：在測試資料庫把角色 `transaction_timeout` 改成 `2s`，`fn` 交替執行 `SELECT 1` 和 `time.Sleep(500ms)` 共 3 秒。結果是 `rolled_back` 以外的錯誤、資料沒寫入，而且這條連線沒有被放回池子（池子的 `Stat().TotalConns()` 減少或下一次取得的是新連線）；
  - `TestLockTimeoutRollsBack`：另一個交易先鎖住一列，`LockTimeout=200ms`，結果 `IsLockTimeout(err)`、`Outcome == rolled_back`；
  - `TestDBContextIgnoresRequestCancel`：HTTP 請求的 context 取消後，交易仍然提交。
- [ ] **Step 2：** `go test ./testproxy/ ./app/store/ -run 'TCP|Commit|RolledBack|NotStarted|TransactionTimeout|LockTimeout|DBContext'` → FAIL。
- [ ] **Step 3：** 實作。
- [ ] **Step 4：** 同一指令加 `-race` → PASS。
- [ ] **Step 5：** commit `feat(store): write-tx wrapper with outcome tracking`。

### Task 5：下單與售完（A6 開關關閉路徑、A7、3.2）

**檔案：**
- 建立：`app/store/orders.go`、`app/store/orders_test.go`、`app/store/menu.go`、`app/store/menu_test.go`

**介面：**
- 消耗：`RunWriteTx`、`DBContext`。
- 產出：`type OrderRequest struct { ClientRef string; Items []OrderItem }`、`OrderItem{MenuItemID int64; Quantity int}`；`RequestHash(req OrderRequest) string`（品項依 `MenuItemID` 排序後的 SHA-256 hex）。
- 產出：`store.CreateOrderTx(ctx, tx pgx.Tx, req OrderRequest, checkSoldOut bool) (Order, error)`：依 A7 鎖菜單（`id` 由小到大 `FOR SHARE`）、`checkSoldOut` 時有售完品項回 `ErrSoldOut`、插入 `orders`、`order_items`。`Order{ID int64; Number string; Total int64}`。
- 產出：`store.SetAvailable(ctx, pool, ids []int64, available bool) (TxResult, error)`：依 `id` 由小到大更新。
- 產出：`store.ListPending(ctx, pool, limit int) ([]Order, error)`、`store.ListMenu(ctx, pool) ([]MenuItem, error)`。

- [ ] **Step 1：** 寫失敗的測試：
  - `TestCreateOrderComputesTotal`；
  - `TestSoldOutRejectedWhenChecking`、`TestSoldOutIgnoredWhenNotChecking`（v1 行為）；
  - `TestSoldOutAndOrderSerialize`：一個交易先 `CreateOrderTx` 到鎖住菜單後暫停，另一個 `SetAvailable(false)` 會等待；放行後售完才生效；
  - `TestRequestHashOrderIndependent`：品項順序不同，hash 相同；數量不同，hash 不同。
- [ ] **Step 2：** 執行 → FAIL。
- [ ] **Step 3：** 實作。
- [ ] **Step 4：** 執行加 `-race` → PASS。
- [ ] **Step 5：** commit `feat(store): orders and sold-out with lock order`。

### Task 6：防重複下單與清理（A6）

**檔案：**
- 建立：`app/store/idempotency.go`、`app/store/idempotency_test.go`

**介面：**
- 消耗：`CreateOrderTx`、`RunWriteTx`、`RequestHash`。
- 產出：`store.PlaceOrder(ctx, pool, req OrderRequest, opts PlaceOptions) (PlaceResult, TxResult, error)`。`PlaceOptions{Idempotency bool; CheckSoldOut bool; LockTimeout time.Duration; DuplicateOrder func(clientRef string) bool}`（後者給 `TEST_MUTATION`，Task 12 接上）。`PlaceResult{Order Order; Replayed bool}`。錯誤：`ErrConflict`（409）、`ErrSoldOut`（422），鎖逾時和死鎖由呼叫端用 `IsLockTimeout`／`IsDeadlock` 判斷回 503。key 讀不到時從頭重試最多 3 次。
- 產出：`store.CleanupKeys(ctx, pool) (deleted int64, err error)`，條件照全域限制。App 每小時跑一次。

- [ ] **Step 1：** 寫失敗的測試，涵蓋 A6「必測案例」七項和「清理的必測案例」四項，名稱一對一：`TestConcurrentSameClientRef`（兩個 goroutine、各自的連線池，模擬兩個 Pod）、`TestRetryWhileFirstInFlight`、`TestRetryAfterRollbackCreates`、`TestDifferentBodyConflict`、`TestLockTimeout503`、`TestKeyCleanedBetweenConflictAndRead`、`TestIdempotencyOffDuplicates`、`TestCleanupKeeps24h59m`、`TestCleanupAfterSlowCommit`、`TestTxTimeoutLeavesNoKeyNoOrder`、`TestCommitUnknownThenRetryOneOrder`（用 Task 4 的代理）。開關關閉的測試要斷言 `idempotency_keys` 一筆都沒有。
- [ ] **Step 2：** 執行 → FAIL。
- [ ] **Step 3：** 實作 A6 第 1～5 點。
- [ ] **Step 4：** 執行加 `-race` → PASS。
- [ ] **Step 5：** commit `feat(store): idempotent order placement and key cleanup`。

### Task 7：標記完成與配號（A1）

**檔案：**
- 建立：`app/store/ready.go`、`app/store/ready_test.go`

**介面：**
- 產出：`store.MarkReady(ctx, pool, orderID int64, attemptID string, delayAfterLock time.Duration) (ReadyResult, TxResult, error)`。`ReadyResult{OrderID int64; ReadySeq int64; Number string; NewlyAssigned bool}`。`delayAfterLock` 只給測試用，正式路徑傳 0。不存在的訂單回 `ErrNotFound`。
- 交易內容照 A1 第 1～5 步，`ready_attempt = attemptID`，`NOTIFY order_ready, '<ready_seq>'`。

- [ ] **Step 1：** 寫失敗的測試：
  - `TestConcurrentReadySameOrder`：同一張單兩個請求同時送，只有一個 `NewlyAssigned`，兩者 `ReadySeq` 相同；另開一條 `LISTEN` 連線只收到一則通知；
  - `TestCommitOrderMatchesSeq`：兩張訂單，第一個 `delayAfterLock=300ms`；用 `pg_stat_activity` 以外的方法記錄提交順序（每個交易提交後把 `clock_timestamp()` 和 `ready_seq` 寫進測試表）。提交時間順序和 `ready_seq` 大小順序一致；
  - `TestReadyAttemptRecorded`：`ready_attempt` 等於傳入的嘗試編號；重按時不變；
  - `BenchmarkReadyUnderLoad`：每秒 20 張的出餐，記錄等鎖時間（結果寫進實測紀錄，不設通過門檻）。
- [ ] **Step 2：** 執行 → FAIL。
- [ ] **Step 3：** 實作。
- [ ] **Step 4：** 執行加 `-race` → PASS。
- [ ] **Step 5：** commit `feat(store): mark ready with ordered sequence`。

### Task 8：叫號廣播員（A2）

**檔案：**
- 建立：`app/broadcast/broadcaster.go`、`app/broadcast/broadcaster_test.go`

**介面：**
- 產出：`type ReadyEvent struct { ReadySeq int64; OrderID int64; Number string }`。
- 產出：`broadcast.New(pool *pgxpool.Pool, listenDSN string, poll time.Duration, log *slog.Logger, drop DropFunc) *Broadcaster`。`DropFunc func(subID uint64, seq int64) bool`，正式為 nil（Task 12 接 `drop-ready`）。
- 方法：`Run(ctx) error`（`LISTEN` 生效後查目前最大值當進度，然後進入單一迴圈）；`Ready() <-chan struct{}`（`LISTEN` 第一次生效後關閉，給 readinessProbe 和 Task 9 用）；`Register(ch chan<- ReadyEvent) (subID uint64, progress int64)`（在 mutex 裡登記並回傳當下進度）；`Unregister(subID)`；`Overflowed(subID) <-chan struct{}`。
- `store.ReadyAfter(ctx, pool, after int64, limit int) ([]ReadyEvent, error)` 和 `store.MaxReadySeq(ctx, pool) (int64, error)` 放在 `app/store/ready.go`。

- [ ] **Step 1：** 寫失敗的測試：
  - `TestDispatchInSeqOrder`；
  - `TestReconnectListenDispatchesMissed`：用 `pg_terminate_backend` 殺掉 LISTEN 連線，期間完成兩個叫號，重新接上後被分派；
  - `TestMissedNotifyCaughtByPoll`：`poll=200ms`，在 LISTEN 連線上 `UNLISTEN`（模擬丟通知）後完成叫號，記錄觸發、查詢完成、分派三個時間，斷言下一次定時查詢後被分派；
  - `TestOverflowMarksSubscriber`：channel 容量 1，不讀，連續分派 3 個，`Overflowed` 被關閉，廣播員沒有被擋住；
  - `TestBroadcasterStartsAtCurrentMax`：資料庫已有 ready_seq 1～5，新的廣播員啟動後，登記的連線不會收到 1～5。
- [ ] **Step 2：** 執行 → FAIL。
- [ ] **Step 3：** 實作。
- [ ] **Step 4：** 執行加 `-race` → PASS。
- [ ] **Step 5：** commit `feat(broadcast): single-loop dispatcher with poll fallback`。

### Task 9：SSE 看板連線（A3、A4、心跳）

**檔案：**
- 建立：`app/board/handler.go`、`app/board/handler_test.go`、`app/board/sse.go`（事件編碼）

**介面：**
- 消耗：`Broadcaster.Register/Unregister/Overflowed`、`store.MaxReadySeq`、`store.ReadyAfter`。
- 產出：`board.Handler{Broadcaster; Pool; Cfg config.Config; Log *slog.Logger; Shutdown <-chan struct{}; Hooks HookPoint}`，實作 `http.Handler`。`HookPoint` 介面 `Pause(ctx, name, attemptID string)`，正式為 no-op（Task 12 實作）；在 A3 第 4、5 步之間呼叫 `sse.after_hello`。
- 事件格式照 A4。每次寫入用 `http.NewResponseController(w).SetWriteDeadline(now+WriteDeadline)`，寫入前後記 `EvWriteStart`、`EvWriteEnd`（只在 `TestHooks` 時記，避免正式 log 太大）。
- 進度大於 H 時記 `EvProgressAheadOfH`。

- [ ] **Step 1：** 寫失敗的測試（`httptest.Server`＋真的資料庫＋真的廣播員），涵蓋 A3 必測案例九項，名稱一對一：`TestProgressBehindH`、`TestProgressEqualsH`、`TestProgressAheadOfHWithFake`（用假的 `Register` 回傳大於 H 的進度）、`TestCommitBetweenRegisterAndH`（用 `Hooks` 在第 4、5 步之間完成一個叫號）、`TestReconnectAfterHelloOnly`、`TestEmptyDatabaseStartsAtZero`、`TestBadLastEventID400`（`abc`、`-1`、最大值＋1 三種）、`TestResumeOffMissesGap`、`TestSlowBoardOverflow`（開關打開和關閉兩種）。另加：
  - `TestNoLastEventIDIsFirstConnect`：開關打開、沒有 `Last-Event-ID` 標頭，起點是目前最大值，回 200；
  - `TestEventWireFormat`：原始位元組逐行比對 `hello`、`ready`、`retry:`、`shutdown`（空 `data:`）、心跳 `:` 行；
  - `TestHeartbeatInterval`：`SSE_HEARTBEAT=200ms`，1 秒內收到 4～6 個註解行。
- [ ] **Step 2：** 執行 → FAIL。
- [ ] **Step 3：** 實作 A3 第 1～7 步。
- [ ] **Step 4：** 執行加 `-race` → PASS。
- [ ] **Step 5：** commit `feat(board): SSE handoff from backfill to live`。

### Task 10：HTTP 伺服器、API、關機流程（A5、4.4、3.1）

**檔案：**
- 建立：`app/server/server.go`、`app/server/api.go`、`app/server/middleware.go`、`app/server/shutdown.go`、`app/server/*_test.go`、`app/main.go`

**介面：**
- 消耗：Task 3～9 的全部產出。
- 路由：`GET /`（點餐頁）、`GET /kitchen`、`GET /board`、`GET /api/menu`、`POST /orders`、`GET /api/orders?status=pending`、`POST /orders/{id}/ready`、`POST /api/menu/{id}/available`（v2 才有，v1 回 404）、`GET /events`（SSE）、`GET /api/audit?run=<runID>&mode=cursor|orders|ready`、`GET /api/whoami`、`GET /healthz`（readiness：伺服器已啟動＋`Pool.Ping`＋`Broadcaster.Ready()`）。
- `/api/audit` 的 `cursor` 用單一陳述式回傳 C（A9 第 5 步）；`orders` 回傳該量測每個 `client_ref` 的訂單 id 清單；`ready` 回傳該量測的 `(order_id, ready_seq, ready_attempt)`。
- 中介層：每個回應加 `X-Koupa-Pod-UID`；每個請求記 `EvRequestReceived`（`attempt`、`path`）和 `EvRequestEnd`（`status`、`write_err`、`flush_err`、`ctx_canceled`、`tx_outcome`、`commit_sent_at`）。
- 產出：`server.Run(ctx, cfg, deps) error`。收到 SIGTERM：
  - `GracefulShutdown=false`：記一行 `EvSigterm` 後 `os.Exit(0)`，不做其他事；
  - `GracefulShutdown=true`：記 `EvSigterm` → 關閉 `Shutdown` channel（`RegisterOnShutdown` 只做這件事）→ `srv.Shutdown(ctx with ShutdownTimeout)` → 逾時記 `EvShutdownTimeout`（附未完成請求數）→ 記 `EvExit`。
- 啟動時記 `EvStartup`（版本、所有開關）。

- [ ] **Step 1：** 寫失敗的測試（以子程序方式啟動編好的 App binary，才能送真的 SIGTERM）：A5 必測案例五項 `TestInflightCompletesAfterSigterm`、`TestBoardsGetShutdownEvent`、`TestNewConnRefusedAfterSigterm`、`TestBlockedBoardDoesNotDelayOthers`（一個原始 TCP 用戶端連上不讀）、`TestStuckRequestExitsAtTimeout`；以及：
  - `TestStep0ExitsImmediately`：`GRACEFUL_SHUTDOWN=0`，處理中的請求被中斷，log 最後一行是 `EvSigterm`；
  - `TestPodUIDHeader`、`TestRequestEndFields`、`TestAuditCursorSingleStatement`、`TestSoldOutEndpointV1Is404`、`TestHealthzWaitsForListen`；
  - `TestWriteDeadlineRawTCP`：A12 第 5 項最後一點的 Go 測試。
- [ ] **Step 2：** 執行 → FAIL。
- [ ] **Step 3：** 實作。
- [ ] **Step 4：** 執行加 `-race` → PASS。
- [ ] **Step 5：** commit `feat(server): routes, audit API and shutdown modes`。

### Task 11：三個頁面

**檔案：**
- 建立：`app/web/{order,kitchen,board}.html`、`app/web/app.js`、`app/web/web.go`（`embed`）、`experiments/browser/pages.spec.ts`

**介面：**
- 點餐頁：每次送出產生 `crypto.randomUUID()` 當 `client_ref`，失敗重送沿用；售完品項灰色。
- 看板頁：`EventSource('/events')`，監聽 `hello`（顯示「服務版本：vX」）、`ready`（顯示「請 0058 號取餐 🧋」）、`shutdown`（只記錄，交給瀏覽器重連）。

- [ ] **Step 1：** 寫 Playwright 測試 `pages.spec.ts`：送一張單拿到號碼牌；廚房按完成後看板顯示該號碼；v2 設售完後點餐頁該品項 disabled。
- [ ] **Step 2：** 對本機 App 執行 `npx playwright test experiments/browser/pages.spec.ts` → FAIL。
- [ ] **Step 3：** 實作頁面。
- [ ] **Step 4：** 同一指令 → PASS。
- [ ] **Step 5：** commit `feat(web): order, kitchen and board pages`。

### Task 12：測試暫停點與故意出錯（A12 控制方式、A14 實際注入）

**檔案：**
- 建立：`app/hooks/hooks.go`、`app/hooks/hooks_test.go`
- 修改：`app/main.go`（接上 `Hooks`、`DropFunc`、`DuplicateOrder`）

**介面：**
- 產出：`hooks.New(enabled bool, log *slog.Logger) *Registry`，實作 `HookPoint`。`enabled=false` 時 `Pause` 直接回傳，也不開埠。
- 控制介面（只在 `TEST_HOOKS=1`，埠 9090）：`POST /hooks/{name}/arm`、`POST /hooks/{name}/release`、`GET /hooks/{name}/wait?timeout=30s`（長輪詢，回傳 `{"pod_uid","pod_name","attempt"}`）。名稱只接受 `order.before_commit`、`order.after_commit`、`request.mid`、`sse.after_hello`。
- `TEST_MUTATION`：`drop-ready=<n>` 對第一條登記的連線丟掉接下來 n 個叫號並記 `EvMutationDropReady`（附 `ready_seq`）；`duplicate-order=<n>` 對接下來 n 個 `client_ref` 在同一交易多插一張訂單並記 `EvMutationDuplicateOrder`。

- [ ] **Step 1：** 寫失敗的測試：`TestDisabledHooksNoPort`、`TestArmWaitRelease`、`TestUnknownHook404`、`TestDropReadyLogsSeqs`、`TestDuplicateOrderLogsClientRefs`、`TestBeforeCommitPausesBeforeCommit`（暫停時查資料庫還看不到訂單）、`TestAfterCommitPausesAfterCommit`（暫停時查得到訂單）。
- [ ] **Step 2：** 執行 → FAIL。
- [ ] **Step 3：** 實作，並在 `PlaceOrder`、中介層、`board.Handler` 裡呼叫對應的暫停點。
- [ ] **Step 4：** 執行加 `-race` → PASS。
- [ ] **Step 5：** commit `feat(hooks): test-only pause points and mutations`。

### Task 13：映像檔

**檔案：**
- 建立：`app/Containerfile`、`rush-hour/Containerfile`
- 修改：`Makefile`（`images`、`push`）

- [ ] **Step 1：** 兩階段建置：`golang:1.26` 編譯（`CGO_ENABLED=0`、`-ldflags "-X koupa/app/config.Version=v1"`），複製到 `gcr.io/distroless/static-debian12:nonroot`（固定 digest）。v1、v2 同一份程式碼，只差 `Version`。
- [ ] **Step 2：** `make images` → 三個映像檔建好；`podman image inspect` 確認架構 `amd64`、大小約 20 MB。
- [ ] **Step 3：** `podman run --rm koupa:v2` 不給 `DATABASE_URL` 時，以非 0 結束並印出錯誤。
- [ ] **Step 4：** `make push`，再從另一台節點 `crictl pull` 確認不用登入拉得到。記下 digest。
- [ ] **Step 5：** commit `build: distroless images for app and rush-hour`。

---

## 階段 2：Kubernetes

### Task 14：Manifest 與 setup.sh（3.5、4、6.1）

**檔案：**
- 建立：`k8s/db/cluster.yaml`、`k8s/migrate/job.yaml`、`k8s/base/{deployment,service,gateway,httproute,namespace,kustomization}.yaml`、`k8s/steps/step-{1,2,3,4}/kustomization.yaml`、`k8s/idle-lab/kustomization.yaml`、`k8s/calibrate/kustomization.yaml`、`k8s/test-hooks/kustomization.yaml`、`setup.sh`、`scripts/check-manifests.sh`

**介面：**
- base：3 replica、`topologySpreadConstraints`（`kubernetes.io/hostname`、`maxSkew: 1`、`whenUnsatisfiable: DoNotSchedule`）、`POD_UID`／`POD_NAME` 用 downward API、映像檔 `koupa:v1`、預設 rolling update。
- step-1：`lifecycle.preStop.sleep.seconds`，值先用 TEACHER.md 的保守值，Task 23 校正後更新。
- step-2：`GRACEFUL_SHUTDOWN=1`、`SHUTDOWN_TIMEOUT`、`terminationGracePeriodSeconds`。
- step-3：`RESUME_FROM_LAST_ID=1`；step-4：`IDEMPOTENCY_KEY=1`。
- calibrate：以 base 為準，`preStop` 20 秒、寬限 45 秒。test-hooks：以 step-4 為準，加 `TEST_HOOKS=1` 和 9090 containerPort（不加進 Service）。
- `scripts/check-manifests.sh <step>`：從 `kubectl kustomize` 的輸出檢查該步應有、不應有的欄位和 A5 不等式。

- [ ] **Step 1：** 寫 `scripts/check-manifests.sh` 的期待表（每一步 × 每個欄位：有／沒有／值），先跑 → FAIL（manifest 還不存在）。
- [ ] **Step 2：** 寫 manifest。
- [ ] **Step 3：** `for s in base step-1 step-2 step-3 step-4 calibrate test-hooks; do scripts/check-manifests.sh $s; done` → 全部 PASS。
- [ ] **Step 4：** 寫 `setup.sh`（3.5 的三段、重跑時先刪舊 Job、失敗印 log）。在 tk8s 執行兩次，第二次也成功；`curl http://<gateway-ip>/api/whoami` 回傳 Pod UID。
- [ ] **Step 5：** commit `feat(k8s): base, steps and setup script`。

---

## 階段 3：Rush Hour

### Task 15：紀錄格式與固定速率送單（A10 紀錄、5.2）

**檔案：**
- 建立：`rush-hour/record/record.go`、`rush-hour/record/record_test.go`、`rush-hour/load/scheduler.go`、`rush-hour/load/customer.go`、`rush-hour/load/*_test.go`

**介面：**
- 產出：`record.Attempt`（JSONL 欄位）：`run`、`kind`（`order`／`ready`／`probe`）、`client_ref`、`order_id`、`attempt`、`try`、`scheduled_at`、`sent_at`、`responded_at`、`status`、`err_kind`（`refused`／`reset`／`timeout`／空）、`pod_uid`、`ready_seq`、`sched_lag_ms`、`full_response`（布林）。`record.Writer`：多 goroutine 安全、每筆 flush。
- 產出：`load.Scheduler{Rate float64; Start time.Time}`，`Next() (scheduled time.Time)`；落後時照時間表繼續，不補送。
- 產出：`load.Customer{Client *http.Client; Base string; Run string; Timeout time.Duration; Retries int; W *record.Writer}`，`Place(ctx, seq uint64)`：隨機 1～4 杯、第 1 次失敗用同一個 `client_ref` 和內容重試 1 次，每次嘗試新的 `X-Koupa-Attempt`。`http.Client` 不自動重試（POST 本來就不會），`Transport` 的 keep-alive 照預設。

- [ ] **Step 1：** 寫失敗的測試：`TestAttemptJSONRoundTrip`、`TestSchedulerFixedRate`（20/s、1 秒內 20 個時間點、間隔 50ms）、`TestSchedulerDoesNotBurstAfterStall`（呼叫端卡住 1 秒後，之後的時間點仍是原時間表，不連續吐出 20 個過期時間點：過期的立即送出但記下 `sched_lag_ms`，不把間隔壓縮成 0 以外的值）、`TestCustomerRetriesSameClientRef`（假伺服器第一次回 503）、`TestCustomerRecordsTimeout`。
- [ ] **Step 2：** `go test ./rush-hour/record/ ./rush-hour/load/` → FAIL。
- [ ] **Step 3：** 實作。
- [ ] **Step 4：** 加 `-race` → PASS。
- [ ] **Step 5：** commit `feat(rush-hour): attempt records and fixed-rate customers`。

### Task 16：看板用戶端（5.2、WHATWG SSE）

**檔案：**
- 建立：`rush-hour/sse/client.go`、`rush-hour/sse/parser.go`、`rush-hour/sse/*_test.go`

**介面：**
- 產出：`sse.Parse(r io.Reader, emit func(Event))`：照 WHATWG 規則處理 `id`、`event`、`data`、`retry`、註解行、空行派送、沒有 `data` 的事件不派送、`id` 含 NUL 時忽略。
- 產出：`sse.Board{Name string; URL string; W *record.BoardWriter}`，`Run(ctx)`：斷線後依 `retry` 等待再重連，帶 `Last-Event-ID`。每條連線記 `connect`（時間、Pod UID）、`hello`（起點、版本）、`ready`（`ready_seq`）、`shutdown`、`disconnect`（有沒有先收到 `shutdown`）。

- [ ] **Step 1：** 寫失敗的測試：`TestParseSpecExamples`（WHATWG 規格裡的範例逐一）、`TestEmptyDataDispatches`、`TestNoDataNotDispatched`、`TestRetryUpdatesDelay`、`TestReconnectSendsLastEventID`、`TestShutdownThenDisconnectRecorded`。
- [ ] **Step 2：** 執行 → FAIL。
- [ ] **Step 3：** 實作。
- [ ] **Step 4：** 加 `-race` → PASS。
- [ ] **Step 5：** commit `feat(rush-hour): EventSource-compatible board client`。

### Task 17：廚房與探測（5.2、A11 探測）

**檔案：**
- 建立：`rush-hour/load/kitchen.go`、`rush-hour/load/probe.go`、測試檔

**介面：**
- 產出：`load.Kitchen{…; Poll time.Duration; Delay func() time.Duration}`，`Run(ctx)`、`Drain(ctx, maxWait time.Duration)`（A9 第 4 步）、`Stop()`。出餐嘗試同樣寫 `record.Attempt`（`kind=ready`、`ready_seq`）。
- 產出：`load.Prober{Rate 50; NewConnPerRequest true}`，`Run(ctx)`：每次新連線 `GET /api/whoami`，記 `kind=probe`、`pod_uid`、`sent_at`、`responded_at`。

- [ ] **Step 1：** 寫失敗的測試：`TestKitchenMarksEachPendingOnce`、`TestKitchenDrainStopsAfterMax`、`TestProberUsesNewConnection`（假伺服器計算 `ConnState` 新連線數等於請求數）。
- [ ] **Step 2：** 執行 → FAIL。
- [ ] **Step 3：** 實作。
- [ ] **Step 4：** 加 `-race` → PASS。
- [ ] **Step 5：** commit `feat(rush-hour): kitchen and probe`。

### Task 18：判定純函式（A9、A10、A14 已知答案）

**檔案：**
- 建立：`rush-hour/verdict/settle.go`、`rush-hour/verdict/classify.go`、`rush-hour/verdict/missing.go`、`rush-hour/verdict/count.go`、`rush-hour/verdict/*_test.go`

**介面：**
- 產出：`type Count struct { Confirmed int; Unknown int }`，`String()` 在 `Unknown > 0` 時輸出「已確認 N；另有 M 項未知」，否則輸出 `N`。
- 產出：`verdict.Settle(e AttemptEvidence, now time.Time, txTimeout time.Duration) Settlement`。`AttemptEvidence{FullSuccess bool; Ingress *IngressRecord; AppReceived bool; AppEnd *AppEnd; PodExitedAt *time.Time; LogCovered bool}`；`IngressRecord{Finished bool; UpstreamAttempts *int}`（nil 表示欄位缺少或收集失敗）；`AppEnd{TxOutcome string; CommitSentAt time.Time}`。`Settlement{Settled bool; Reason string}`。規則照 A9 第 3 步，逐條。
- 產出：`verdict.State(a AttemptEvidence, gotFullErrorResponse bool) State`，值照 A10 的狀態和細分（`ClientGotError`、`NotReachedApp`、`ReachedNotEnded`、`WriteFailedOrCanceled`、`WriteFlushOK`、`Unknown`），順序照 A10。
- 產出：`verdict.Cause(...) Cause`：只有 A10「原因」表的必要證據齊全才回傳對應原因，否則 `CauseUnknown`。
- 產出：`verdict.Missing(in MissingInput) MissingResult`。`MissingInput{DBReady []ReadyRow; Boards []BoardSeq; Cursor int64; UnsettledSeqs map[int64]bool}`；`BoardSeq{Name string; Start *int64; Received []int64; CaughtUp bool}`；`MissingResult{PerBoard map[string]Count; Duplicates map[string][]int64; NotIncreasing map[string]bool; Invalid bool}`。`Start == nil` → `Invalid`。
- 產出：`verdict.Duplicates(orders map[string][]int64, unsettledRefs map[string]bool) (clientRefs Count, extraOrders Count)`。

- [ ] **Step 1：** 寫表格驅動的失敗測試：
  - `TestSettle`：A9 第 3 步每一條規則至少一列，包括「入口取不到」「入口未結束」「0 次」「1 次＋完整成功」「1 次＋committed／rolled_back／not_started」「1 次＋commit_unknown 未滿／已滿 60 秒」「1 次＋只有收到＋Pod 結束未滿／已滿 60 秒」「1 次＋沒有收到」「2 次」「欄位缺少（nil）」；
  - `TestState`：A10 判定順序每一層一列，包含「完整 503 → ClientGotError，即使 App log 也有寫入成功」；
  - `TestCause`：證據不齊的每一種都回 `CauseUnknown`；
  - `TestMissing`：A14 已知答案資料七類（單筆、多筆、重複、補回、起點邊界、C 邊界、空集合），加上「沒追上 → 該看板 Unknown 等於範圍內未收到數、Confirmed 0」和「起點缺少 → Invalid」；
  - `TestCountString`：`{0,12}` → `已確認 0；另有 12 項未知`；`{0,0}` → `0`。
- [ ] **Step 2：** `go test ./rush-hour/verdict/` → FAIL。
- [ ] **Step 3：** 實作。
- [ ] **Step 4：** → PASS。
- [ ] **Step 5：** commit `feat(verdict): settlement, state, cause and missing checks`。

### Task 19：收尾流程與成績單（A9）

依賴 Task 1 的入口紀錄結論：取不到入口紀錄時，`Ingress` 一律為 nil，判定自然變成未知。

**檔案：**
- 建立：`rush-hour/winddown/winddown.go`、`rush-hour/winddown/report.go`、`rush-hour/main.go`、測試檔、`rush-hour/winddown/testdata/`

**介面：**
- 消耗：Task 15～18 全部。
- 產出：`rush-hour run --base URL --run ID --duration 60s --rate 20 --boards 3 --out DIR [--ingress-log FILE] [--app-logs DIR] [--verbose]`。看板都收到 `hello` 後在 stdout 印一行 `SENDING_STARTED <unix-nano>`（`measure.sh` 用它對時）；第 D 秒自己進入收尾。收尾時從 `--app-logs` 和 `--ingress-log` 讀證據（由 `measure.sh` 持續寫入的檔案），依 A9 第 1～7 步執行。
- 產出：`rush-hour probe --base URL --run ID --duration …`（Task 23 的 calibrate 用）。
- 診斷資訊另外包含：每個 Pod 在 `EvSigterm` 之後才記下 `EvRequestReceived` 的請求數（設計 5.3、A11 第 7 步），以及同一段時間的主機 CPU、記憶體和 rush-hour 排程延遲；超過門檻只標「同時觀察到資源吃緊」。
- 輸出：`DIR/attempts.jsonl`、`DIR/boards.jsonl`、`DIR/scorecard.txt`、`DIR/scorecard.json`、結束狀態：0 正常；2 量測無效。
- 產出：`rush-hour summarize DIR...`：讀多次的 `scorecard.json`，印每次數字、中位數、範圍。

- [ ] **Step 1：** 寫失敗的測試，用 `testdata/` 的假紀錄（不連伺服器）：`TestReportMainSection`、`TestReportVerboseSection`、`TestReportUnknownNeverZero`（有未知時主要成績的每一格都不是單獨的 `0`）、`TestInvalidWhenBoardHasNoStart`（結束狀態 2、成績單第一行「本次量測無效」）、`TestSummarizeMedianRange`。
- [ ] **Step 2：** 執行 → FAIL。
- [ ] **Step 3：** 實作收尾的等待上限：用戶端 10 秒、伺服器端 `transaction_timeout`＋10 秒、廚房 10 秒、看板追上 10 秒。
- [ ] **Step 4：** → PASS。另外對本機 App（Task 10）跑一次 `rush-hour run --duration 30s`，成績單全部是 0、沒有未知。
- [ ] **Step 5：** commit `feat(rush-hour): wind-down, scorecard and summary`。

---

## 階段 4：量測

### Task 20：時間線收集（A11 收集、log 完整性）

**檔案：**
- 建立：`scripts/collect-timeline.sh`、`rush-hour/timeline/merge.go`、`rush-hour/timeline/coverage.go`、測試檔

**介面：**
- `collect-timeline.sh start <dir>`：背景啟動 A11 表格中的每個來源，每行前面加收集端時間（`date +%s.%N`），App log 每個 Pod 一個檔案，新 Pod 出現時自動加開；另外每秒記錄主機 CPU 和記憶體（讀 `/proc/stat`、`/proc/meminfo`）；開始和結束各抓一次 Envoy koupa cluster 的統計（取得方式依 Task 1）。寫 `<dir>/collectors.pid`。`collect-timeline.sh stop <dir>`：結束所有收集程序。取不到的來源寫一筆 `missing`。
- 產出：`rush-hour timeline <dir>`：輸出 `timeline.jsonl`（每筆有 `source`、`event_time`、`collected_at`、`kind`、`pod_uid`、`interval_start`／`interval_end`（只知道區間時））和 `timeline.txt`。
- 產出：`timeline.Coverage(podLog PodLogMeta) CoverageResult`，規則照 A11「log 完整性」三條。終止起點用「第一次觀察到的 `deletionTimestamp` − `deletionGracePeriodSeconds`」。

- [ ] **Step 1：** 寫失敗的測試：`TestDeletionStartUsesFirstObservation`（第二次觀察的寬限時間變短，結果不變）、`TestCoverageNeedsStartMarker`、`TestCoverageStep0EndsAtSigterm`、`TestCoverageGapNotFilledIsUncovered`、`TestFinishedAtDoesNotProveCoverage`、`TestMissingSourceMarked`、`TestTimelineKeepsBothTimes`。
- [ ] **Step 2：** 執行 → FAIL。
- [ ] **Step 3：** 實作。
- [ ] **Step 4：** → PASS。在 tk8s 手動 `kubectl rollout restart deploy/koupa`，前後 start／stop，產生的時間線上每個舊 Pod 都有刪除請求、SIGTERM、程式結束三點，或標明缺少。
- [ ] **Step 5：** commit `feat(timeline): collectors, merge and log coverage`。

### Task 21：preStop 校正（A11 calibrate）

**檔案：**
- 建立：`rush-hour/calib/calib.go`、`rush-hour/calib/runs.go`、`rush-hour/calib/*_test.go`

**介面：**
- 產出：`calib.Estimate(in Input, cfg Config) Sample`。`Input{Probes []Probe; DeletionStart time.Time; PreStopEnd time.Time; DirectUpper *time.Time; PerAttemptIngress bool}`；`Probe{SentAt, RespondedAt time.Time; PodUID string; IngressSelectedAt *time.Time}`；`Config{MinPreSamples 100; Alpha 0.05; MissProb 0.001; PreWindow 10s; Margin 5s}`。`Sample{Lower time.Time; Upper *time.Time; UpperKind string /* direct|model|none */; P float64; K int; N int; RunsZ float64; Valid bool; InvalidReason string}`。
- 產出：`calib.RunsTest(hits []bool) (z float64, p float64)`。
- 產出：`calib.Candidate(samples []Sample) (seconds float64, ok bool)`：有效樣本少於 5 個回 `ok=false`；否則回 `max(Upper − DeletionStart)`（不含緩衝，緩衝由 TEACHER.md 決定）。

- [ ] **Step 1：** 寫失敗的測試：
  - `TestLowerUsesSentAtNotAppTime`；
  - `TestUpperModelK`：p = 1/3 時 k = 18（(2/3)^17 > 0.001、(2/3)^18 ≤ 0.001），U 是第 18 次探測的 `RespondedAt`；
  - `TestDirectUpperPreferred`；
  - `TestNoUpperWhenFewSamples`（99 次 → `UpperKind=none`）、`TestNoUpperWhenPZero`、`TestRunsTestRejectsCorrelated`（交替序列 `TFTFTF…` 被拒）、`TestRateChangeRejectsModel`；
  - `TestInvalidWhenNoHitAfterDeletion`、`TestInvalidWhenUpperTooCloseToPreStopEnd`（U 只早 4 秒）；
  - `TestCandidateNeedsFive`。
- [ ] **Step 2：** `go test ./rush-hour/calib/` → FAIL。
- [ ] **Step 3：** 實作。
- [ ] **Step 4：** → PASS。
- [ ] **Step 5：** commit `feat(calib): bounded stop-selection estimate`。

### Task 22：measure.sh 主流程（A8、A13、5.4 證據包）

**檔案：**
- 建立：`measure.sh`、`scripts/preflight.sh`、`scripts/evidence.sh`、`test/measure/*.sh`

**介面：**
- `measure.sh <mode> [--runs 5] [--duration 60] [--rate 20] [--verbose] [--diagnostic]`，`mode` 為 `step-0`～`step-4`、`control`、`calibrate`、`rollback`、`drain`、`scale-down`、`all`。
- `scripts/preflight.sh <step>`：A8 第 2 步的六項核對，任一不符印出哪一項並以 1 結束。
- 一次量測照 A8 第 1～7 步；等 rush-hour 印出 `SENDING_STARTED` 後計時第 10 秒；第 D − 10 秒 rollout 未完成 → 該次無效。
- `scripts/evidence.sh <run-dir>`：存 manifest（`kubectl kustomize` 輸出和實際 `kubectl get -o yaml`）、版本、映像檔 digest、參數、原始紀錄、`/api/audit` 結果、成績單。
- A13：`control` 有任何失敗或未知、或步驟 0 的 5 次都沒觀察到問題 → 結束狀態 3，除非 `--diagnostic`（這時每張成績單第一行加「診斷模式：對照組不成立，不可用於比較」）。

- [ ] **Step 1：** 寫失敗的腳本測試（不連叢集，用假的 `kubectl`、`rush-hour` 放在 `PATH` 前面）：`test-preflight-detects-leftover-env.sh`（step-1 卻帶著 `GRACEFUL_SHUTDOWN`）、`test-preflight-rejects-test-hooks.sh`、`test-invalid-when-rollout-late.sh`、`test-control-unknown-fails.sh`（結束狀態 3）、`test-diagnostic-banner.sh`、`test-evidence-layout.sh`。
- [ ] **Step 2：** `for t in test/measure/test-*.sh; do bash $t; done` → FAIL。
- [ ] **Step 3：** 實作。
- [ ] **Step 4：** → PASS。在 tk8s 跑 `measure.sh control --runs 1` 和 `measure.sh step-0 --runs 1`，證據包齊全。
- [ ] **Step 5：** commit `feat(measure): step and control modes with evidence pack`。

### Task 23：其他模式與中斷復原（A8 其他模式、A11 calibrate 流程、A15）

**檔案：**
- 修改：`measure.sh`
- 建立：`test/measure/test-measure-trap.sh`、`test/measure/test-drain-target.sh`

**介面：**
- `calibrate`：套用 `k8s/calibrate`，同時跑 `rush-hour run` 和 `rush-hour probe`，結束後對每個被終止的 Pod 呼叫 `calib.Estimate`，重複到 5 個有效樣本或達到 `--runs`；把候選值、樣本表寫進 `evidence/calibrate-<時間>/`。
- `rollback`：先套 v2，第 5 步改 v1。
- `drain`：A15 的目標檢查（有 koupa Pod、沒有資料庫 Pod、沒有 `emptyDir` 重要資料）→ `kubectl drain --ignore-daemonsets`（不加 `--delete-emptydir-data`）→ uncordon → 重新平衡並核對分布。
- `scale-down`：縮成 2，結束後恢復 3 並確認可用。
- `all`：control、step-0～4 交錯 5 輪。
- `trap` 處理 `EXIT`、`INT`、`TERM`：uncordon、恢復 replica、`collect-timeline.sh stop`。

- [ ] **Step 1：** 寫失敗的腳本測試：`test-measure-trap.sh`（假 `kubectl` 記錄呼叫，在 drain 之後送 SIGINT，斷言之後呼叫過 `uncordon` 和 `collect-timeline.sh stop`）、`test-drain-target.sh`（目標上有資料庫 Pod → 換一個 worker；全部都不符合 → 停止）、`test-all-interleaves.sh`（呼叫順序是 control、0、1、2、3、4 重複 5 次）。
- [ ] **Step 2：** 執行 → FAIL。
- [ ] **Step 3：** 實作。
- [ ] **Step 4：** → PASS。在 tk8s 跑 `measure.sh calibrate`，把候選值和是否「未經校正」更新到 `k8s/steps/step-1` 和 TEACHER.md。
- [ ] **Step 5：** commit `feat(measure): calibrate, rollback, drain, scale-down, all`。

---

## 階段 5：關鍵時機與量測工具驗證

### Task 24：關鍵時機實驗（A12、5.5）

**檔案：**
- 建立：`experiments/critical/run.sh`、`experiments/critical/{01-retry-after-commit,02-sigterm-mid,03-handoff,04-gap,05-blocked-write,06-late-commit,07-state-samples}.sh`、`experiments/critical/slow-board/`（叢集內測試 Pod 用的小程式，Go）

**介面：**
- 消耗：Task 12 的控制介面、`k8s/test-hooks`。每個腳本先 `kubectl port-forward` 到**每個** koupa Pod 的 9090 並 `arm` 同一個暫停點，再照 A12「各時機的順序」執行，每一步設逾時。
- 每個腳本對每種開關設定各跑一次，輸出 `PASS`／`FAIL` 和證據目錄。`run.sh` 全部跑完，有任何 FAIL 就以非 0 結束。

- [ ] **Step 1：** 先寫每個腳本的預期結果檢查（5.5 表格每一列 × 開關設定），對還沒有實作的步驟執行 → FAIL。
- [ ] **Step 2：** 實作 01～04。
- [ ] **Step 3：** 實作 05：`slow-board` 在叢集內直接連 Pod IP，一條不讀、兩條正常讀；等 App log 出現 `EvWriteBlocked` 後刪除 Pod，從 SIGTERM log 起計時。
- [ ] **Step 4：** 實作 A10 需要暫停點的三個必測案例：`06-late-commit`（`order.before_commit` 暫停超過 Rush Hour 逾時再放行；另一次暫停到收尾等待上限之後才放行，成績單必須是「伺服器端結果未知」）；`07-state-samples`（各造出一筆「送到了，App 沒有結束這個請求」和「App 回報寫入和 flush 成功」，用 Task 18 的 `verdict.State` 判定）。
- [ ] **Step 5：** `experiments/critical/run.sh` → 全部 PASS。
- [ ] **Step 6：** commit `test(experiments): critical timing scenarios`。

### Task 25：入口重試與延遲抵達（A10 必測案例前四項）

依賴 Task 1：用和 Cilium 相同版本的 Envoy、相同的重試和 access log 設定。

**檔案：**
- 建立：`experiments/ingress/envoy.yaml`、`experiments/ingress/run.sh`、`testproxy/http.go`、`testproxy/http_test.go`

**介面：**
- 產出：`testproxy.NewHTTP(upstream string) *HTTP`，方法 `HoldNext(match func(*http.Request) bool) (release func())`：扣住下一個符合的請求，直到呼叫 `release`。
- 本機組合：podman 跑 Envoy → `testproxy.HTTP` → App binary → 測試資料庫。Envoy 的 access log 輸出成檔案，交給 `rush-hour run --ingress-log`。

- [ ] **Step 1：** 寫 `testproxy/http_test.go` 的失敗測試 `TestHoldAndRelease`，以及 `run.sh` 裡四個案例的預期（A10 必測案例「延遲抵達」「入口重試且已經回應成功」「入口重試且其中一次已在 App 結束」兩種、「最終旗標是 UF 或本機回應」）。
- [ ] **Step 2：** 執行 → FAIL。
- [ ] **Step 3：** 實作代理和 Envoy 設定。
- [ ] **Step 4：** `experiments/ingress/run.sh` → 四個案例的伺服器端結果都是「未知」，狀態分類符合 A10。
- [ ] **Step 5：** commit `test(experiments): ingress retry and late arrival`。

Task 1 結論是「入口重試可以關掉」時，這個任務仍然要做（驗證判定邏輯），並在 TEACHER.md 註明正式量測時關閉重試。

### Task 26：量測工具交叉驗證（A14）

**檔案：**
- 建立：`experiments/verify/mutation.sh`、`experiments/verify/browser.spec.ts`、`experiments/verify/curl-stream.sh`、`experiments/verify/ingress-reconcile.go`、`experiments/verify/k6-orders.js`、`experiments/verify/compare-tools.sh`

- [ ] **Step 1：** `mutation.sh`：分別用 `TEST_MUTATION=drop-ready=3` 和 `duplicate-order=2` 部署（`k8s/test-hooks` 改這個環境變數），跑一次量測，比對 App log 的注入紀錄和成績單：注入的漏號如果被補回就不算，`duplicate-order` 的重複訂單數必須等於 2。期待寫好後先跑 → FAIL（環境變數未接上時）。
- [ ] **Step 2：** `browser.spec.ts`：Playwright 開看板頁，和 Rush Hour 看板同時連線；在共同區間逐筆比對 `ready_seq`；直接連同一個 Pod 時比對 `shutdown`。`curl-stream.sh`：`curl -N` 存原始串流，檢查欄位格式。
- [ ] **Step 3：** `ingress-reconcile.go`：依 A14「和入口的紀錄對帳」的範圍和差額分類輸出表格（Task 1 結論是做不到時，輸出「入口紀錄取不到」並結束）。
- [ ] **Step 4：** `compare-tools.sh`：k6 以相同速率、逾時、重試 1 次，對步驟 0 和步驟 2 各量 5 次，和 Rush Hour 的「送出沒有成功回應」並排印出方向和範圍。
- [ ] **Step 5：** 全部執行，結果存進 `evidence/verify-<時間>/`。
- [ ] **Step 6：** commit `test(verify): mutation, protocol and tool cross-checks`。

---

## 階段 6：文件與驗收

### Task 27：講義、講師手冊、實測紀錄

**檔案：**
- 建立：`README.md`、`TEACHER.md`、`docs/measurements/<日期>.md`
- 修改：`docs/design-v2/06-reference.md`（6.4 每一項填上結果或維持待實測）、`docs/design-v2/04-step-mechanisms.md`（實測推翻的步驟照實改）

- [ ] **Step 1：** 先做兩個實測小實驗：閒置斷線小實驗（套用 `k8s/idle-lab`，記錄切斷時間和 Envoy 逾時種類，決定心跳間隔或改調 HTTPRoute，依 4.3）；readinessProbe（步驟 0 量測中有沒有「新 Pod 還沒準備好就接收流量」造成的失敗，依 4.9 決定要不要加進步驟）。
- [ ] **Step 2：** 講師跑完 6.5「程式或環境改版後」整列：自動化測試、`calibrate`、`measure.sh all`、Task 24～26、drain 和 scale-down。全部結果和證據包路徑寫進 `docs/measurements/<日期>.md`。
- [ ] **Step 3：** 寫 README.md（6.3 列的內容；範例數字用 Step 2 的實測值，標明環境和次數）和 TEACHER.md（原理、閒置斷線小實驗結論、保守 `preStop` 值、口頭補充、驗證工作表和預估時間）。
- [ ] **Step 4：** 用 docs-review skill 審閱 README.md 和 TEACHER.md。
- [ ] **Step 5：** 第三方重跑：另一個人在全新的 tk8s 上只照 README 操作，交回證據包；比對定性結論，不一致就回到對應任務調查。
- [ ] **Step 6：** commit `docs: student guide, teacher guide and measurement record`。
