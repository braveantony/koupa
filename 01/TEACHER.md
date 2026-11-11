# Koupa 酷杯 01：講師手冊

學生講義是 [README.md](README.md)；範例應用本身（組成、部署、確認連得到、清除）見[Koupa 酷杯範例應用](docs/koupa-app.md)。這份手冊寫給講師：每一步背後的原理、上課節奏、常見提問、口頭補充，以及課前和改版後要做的驗證。所有數字都來自 [實測紀錄 2026-11-10](docs/measurements/2026-11-10.md)，只代表 tkha（單一主機上的 3 個 control plane＋3 個 worker）。

## 先看一張圖：整堂課只在講這件事

```
時間 ──────────────────────────────────────────────────────────────►
刪除 Pod 1
  │
  ├─► 控制平面：Pod 1 在 EndpointSlice 標成「正在終止」
  │        └─► 入口收到更新，才不再選 Pod 1    （tkha：最後一次選它在刪除後 0.4～1.5 秒）
  │
  └─► kubelet：執行 preStop ──► 送 SIGTERM ──► 程式結束
                 （步驟 2 起等 8 秒）
```

兩條線同時出發，彼此不等對方。如果 SIGTERM 先到，而入口在停止選它之前又把請求送過來，客人就可能收不到成功回應（問題一）；程式直接結束，連在它上面的看板就斷線，斷線期間的叫號就漏掉（問題二）；客人沒收到回應又重送，就可能變成兩張單（問題三）。新 Pod 那一端也有一樣的問題：Kubernetes 以為它準備好了，其實它還沒開始 listen（步驟 1）。

**一句話**：每一步都是在補這兩條線之間的空隙；Kubernetes 的設定能讓空隙變小，但只有程式本身（優雅關閉、補號、防重複）能處理剩下的部分。

---

## 1. 原理：每一步在補哪個空隙

| 步驟 | 補的空隙 | 對學生講的重點 |
|---|---|---|
| 小實驗 | 入口切斷太久沒有資料的長連線（與升級無關） | 長連線本身就會斷；心跳只對「閒置」有效 |
| 0 | — | 先量出問題。tkha 上步驟 0 五次都量得到 |
| 1 `readinessProbe` | 新 Pod 還沒 listen（開始在埠上接受連線）就被標成 Ready | 「容器啟動了」不等於「準備好了」 |
| 2 `preStop` | 入口還沒收到更新，舊 Pod 就收到 SIGTERM | 等待秒數要用量的，不要憑感覺填 |
| 3 優雅關閉（graceful shutdown） | 收到 SIGTERM 時手上的請求和看板 | 程式要自己收尾，還要通知看板重連 |
| 4 補號 | 看板斷線到重連之間的叫號 | 重連不夠，要從斷掉的地方接回去 |
| 5 防重複 | 回應遺失時客人重送 | Kubernetes 不能保證回應一定送到，程式要能安全地處理重送的請求 |

細節見設計文件的[每一步修正的機制](docs/design-v2/04-step-mechanisms.md)。

## 2. 上課節奏

機器時間是根據 2026-11-10 每次量測的實測時間推算的：control 和步驟 2～5 一次約 1.5 分鐘，步驟 0、1 一次約 3.5 分鐘（收尾時要等「結果未知」的請求）。討論時間沒有實測過，由講師依班級調整。

| 段落 | 內容 | 機器時間（5 次） |
|---|---|---|
| 開場 | 用 README 的圖和情境說明三種問題；學生預測步驟 0 的成績單 | — |
| 部署 | `./setup.sh`、拉取 Rush Hour 映像檔、打開三個頁面 | 未記錄 |
| 小實驗 | 沒有心跳等到第 300 秒斷線，再套用心跳跑 420 秒 | 約 12 分鐘（連線本身 300＋420 秒） |
| 對照組 | `./measure.sh control`：先確認量測工具和環境沒問題 | 約 7.5 分鐘 |
| 步驟 0 | 看成績單和時間線，找出送出失敗與看板斷線發生的時段；討論為什麼這次無法確認是否有重複訂單 | 約 18 分鐘 |
| 步驟 1 | 預測 → 量測 → 解釋「新 Pod 那一類失敗為什麼消失」 | 約 18 分鐘 |
| 步驟 2～5 | 每一步都先預測、再量、再解釋 | 每步約 7.5～9 分鐘 |
| 最後驗證 | `rollback`、`drain`、`scale-down` | 約 21 分鐘（三種各 5 次） |
| 關鍵時機實驗 | 講師示範（學生只預測和解釋），尤其是步驟 5 的重複訂單 | 約 7 分鐘（12 項） |

量測跑著的時候，請學生先寫下預測；成績單印出後再對答案，並請他們對照 `timeline.txt` 和 `failures.json`，找出失敗與 Pod 終止的時間關係。

## 3. 常見提問

**Q：`preStop` 直接設 30 秒不就好了？**
可以更保險，但每個舊 Pod 都要多等這麼久，rollout 變慢，而且 `preStop` 占用關機寬限時間：步驟 3 要符合 `preStop` ＋ `SHUTDOWN_TIMEOUT` ＋ 5 ＜ 寬限。這門課的態度是「量出來再加緩衝」，見第 5 節。

**Q：readinessProbe 不是基本設定嗎？為什麼步驟 0 沒有？**
步驟 0 是「什麼都沒改」的真實樣子。原本設計預期 Go 程式啟動很快、不需要它，是在 tkha 上量到失敗才加成步驟 1（第 6 節）。這正好示範「步驟要依量測結果調整」。

**Q：步驟 2 的送出失敗已經是 0，為什麼漏號沒有變少？**
`preStop` 只讓入口有時間停止送「新」請求。8 秒後程式照樣直接結束，連在舊 Pod 上的看板還是被切斷，重連後從「現在」開始收。要靠步驟 3 通知看板重連、步驟 4 補號。

**Q：步驟 1 的漏號（每台已確認 25～164）怎麼比步驟 0 還多？**
實測紀錄沒有查這個原因，不要當成結論講。可以和學生一起看的觀察（都未驗證）：步驟 1 的 rollout 比較久（步驟 0 在第 11.2～11.8 秒完成，步驟 1 在第 12.9～25.9 秒），看板斷線、重連的時段可能跟著拉長；步驟 0 五次中有四次另有未知的叫號，已確認的數字只是下限。兩步都還沒有修正漏號，能確定的只有「都還有漏號」。

**Q：步驟 5 的成績單和步驟 4 一模一樣，這一步有什麼用？**
這次預設負載的 25 次量測（步驟 0～4）沒有確認到重複訂單：已確認的重複訂單都是 0，其中步驟 0、1 仍有伺服器端結果未知。「訂單已經存好、回應沒送到、客人重送」這種一瞬間的情況用關鍵時機實驗刻意製造（第 1 項）：防重複關閉時，同一個 `client_ref` 有 2 張訂單；打開時重送拿回同一張。要清楚告訴學生：這是「機制在受控實驗中有效」，不是「這一步在自然負載下改善了重複訂單」。

**Q：成績單為什麼有「未知」，不能直接算成 0 嗎？**
例如請求送出後連線斷了，伺服器到底有沒有建立訂單，客人端看不出來。證據不足就標未知、寫明原因，不當成功也不當失敗。步驟 0、1 的重複訂單就是「已確認 0；另有未知」。

**Q：5 次都是 0，就代表不會發生？**
不代表。只代表這 5 次沒觀察到；5 次也估計不了很少發生的失敗率。

**Q：廚房頁有時候看到兩張同號的單？**
號碼牌 0001～9999 循環。只要有一張單在 9999 張新單之後還沒完成，就會和新的同號單同時出現。成績單用 `orders.id` 和 `ready_seq` 對帳，不受影響。見第 10 節。

## 4. 口頭補充（不實作）

- **寬限時間設很長、等看板自己斷線**：如果 App 不主動結束看板連線，而連線又沒有因其他原因中斷，升級就可能一直等到寬限時間用完。所以步驟 3 的 App 會主動送 `shutdown`。
- **多台看板錯開時間重連**：避免同時擠到同一個新 Pod。實務上常見，當作延伸內容。
- **升級期間新舊版並存**：rolling update 期間客人可能被分到還沒換掉的 v1 Pod，v1 不檢查售完，所以售完的品項還點得到。成績單不算這一項，留給 02（資料庫和新舊版相容）鋪路。
- **readinessProbe 的替代方案為什麼不好**：App 先 listen 再連資料庫，會接到請求卻處理不了；縮短 DNS 解析器的逾時（`dnsConfig` 的 `timeout:1`）只能把 5 秒縮成 1 秒；Cilium 端點取得身分前丟掉封包是環境行為，不在課程修正範圍。
- **入口逾時對應的 Envoy 設定名稱**：閒置逾時是 HCM（HTTP connection manager）的 `stream_idle_timeout`（tkha 是 300 秒）；總時間上限是路由的 `timeout`／`max_stream_duration`。HTTPRoute 沒寫 `timeouts` 時，Cilium 產生 `max_stream_duration: 0s`，Envoy 因此不套用 15 秒的預設 route timeout（[env-findings 3.2](docs/env-findings.md#32-逾時)）。

## 5. `preStop` 秒數的依據與限制

**公式**（`measure.sh calibrate`，[實作計畫 A11](docs/plan.md)）：對每個被終止的舊 Pod 量兩個時間：

- L：入口最後一次沒有歧義地把請求送給它的時間（入口的逐次紀錄）；
- U：L 之後連續 k 次探測都沒選到它，而且 (1 − p)ᵏ ≤ 0.001 時，第 k 次探測收到回應的時間（p 是刪除前 10 秒的命中比例）。

只有通過檢定、U 早於 `preStop`（calibrate 用 20 秒）結束至少 5 秒的樣本才算有效，至少要 5 個。**候選值 ＝ 所有有效樣本中 U − 刪除請求時間的最大值**，步驟 2 的 `preStop` ＝ ceil(候選值) ＋ 緩衝 5 秒。

| 校正 | 有效樣本 | 候選值 | `preStop` | 證據 |
|---|---|---|---|---|
| Task 23b（2026-11-09 22:06） | 7 | 1.504 秒 | 7 秒 | `evidence/calibrate-20261109-220635/` |
| 2026-11-10 重跑（12:52） | 7 | 2.127 秒 | **8 秒（現行）** | `evidence/calibrate-20261110-125225/` |

`preStop` 改成 8 秒之後，步驟 2～5 重跑的 20 次，「第一次送出沒有成功回應」都是 0（7 秒時的 20 次也是 0）；入口最後一次把請求送給終止中的 Pod，是刪除請求後 0.65～1.18 秒（Task 23b 是 0.39～1.46 秒）。

**限制**（要讓學生知道）：

- **上界全部是模型推估**：沒有涵蓋所有入口實例的端點更新紀錄，直接證據是未知。模型假設入口每次獨立挑選後端，但每次 run 都有 1～2 個 Pod 檢定不通過、沒有上界，表示這個假設本身可疑；
- **候選值只代表這個環境**，不是未來的上限。兩次校正就差了 0.6 秒；換了主機、Cilium 版本或節點數都要重新量。所以之後每次量測都要看「SIGTERM 之後才到的請求」（`--verbose` 成績單的診斷資訊，或 `scorecard.json` 的 `diagnostics.pods`）：步驟 2 以後只要不是 0，先查是新選的流量、既有連線上的請求，還是紀錄延遲，再決定要不要重新校正；
- **緩衝 5 秒是暫定值**：步驟 2～5 的 40 次舊 Pod 失敗都是 0，沒有證據顯示要更大，但也沒有量出該多大；
- 刪除請求的時間只到整秒，U − 刪除請求時間可能多算不到 1 秒。

**校正失敗時的保守值：20 秒**（有效樣本不足 5 個時，`measure.sh calibrate` 不產生候選值，結束碼 2）。20 秒和 calibrate 自己用的 `preStop` 相同，比兩次候選值都長很多。改用時要一起改：

1. `k8s/steps/step-2` 的 `seconds` 改成 20；
2. `k8s/steps/step-3` 的 `terminationGracePeriodSeconds` 改成 45（20 ＋ 15 ＋ 5 ＝ 40，寬限要大於 40；calibrate 也用 45）；
3. `scripts/check-manifests.sh` 裡 `STEP2`、`STEP3` 的期望值跟著改，`scripts/check-manifests.sh step-2`、`step-3` 要通過；
4. `k8s/steps/step-2` 的標記 `koupa.dev/prestop-calibrated` 改成 `"false"`（Pod 範本的 annotation，步驟 3～5 繼承），`scripts/check-manifests.sh` 裡 `STEP2` 的 `preStopCalibrated` 也改成 `"false"`。`measure.sh` 量測前讀 live Deployment 上的這個標記（存成證據包的 `deployment-at-start.json`），步驟 2 以後的成績單標題下一行就會印「preStop 未經校正：20 秒是講師手冊的保守值……」，`scorecard.json` 的 `prestop_calibration.status` 是 `uncalibrated`。標記是 `"true"` 時印「preStop：N 秒（已依本環境校正）」；沒有標記或不是 `true`／`false` 時印「未知」，不會當成已校正。重新校正成功、改回候選值時，記得把標記改回 `"true"`。實測紀錄裡仍要寫明用的是保守值。

如果有效樣本不足，是因為 U 距離 `preStop` 結束不到 5 秒，就表示 20 秒不夠長。請把 `k8s/calibrate` 的 `preStop` 和寬限時間一起加長，再重新校正。

## 6. readinessProbe 的依據

量測紀錄：[env-findings 第 9 節](docs/env-findings.md#9-步驟-2-的-503-與-prestop-重新校正task-23b)（該節用重新編號前的步驟編號）、[第 10 節](docs/env-findings.md#10-readinessprobe-成為步驟-1-之後的實測task-23c)、[實測紀錄第 7 節](docs/measurements/2026-11-10.md#7-readinessprobe-的依據)。

- **現象**：約 28% 的新 Pod（57 個中 16 個）啟動後 5.02～5.03 秒才 listen。2026-11-10 的 30 次量測再確認：90 個新 Pod 中 27 個（30%）；
- **原因**：Pod 的第一個 DNS 查詢在 Cilium 端點還是 `reserved:init` 身分（還沒取得正式身分）時被丟掉（Hubble 記 `DROPPED POLICY_DENIED`；叢集沒有任何 NetworkPolicy），要等解析器 5 秒逾時後重送，App 才連得上資料庫、開始 listen；
- **影響**：沒有 probe 時這種 Pod 一啟動就 Ready，入口送過去的請求得到 `UF`（連不上上游）／`Connection refused`。步驟 0 有這種 Pod 的 4 次，各多出 34～44 張「第一次送出沒有成功回應」；沒有的那一次只有 8 張。這就是步驟 0 數字忽高忽低（8～53）的原因；
- **加上 probe 之後**：步驟 1～5 的 25 次量測有 23 個這種 Pod，「新 Pod 還沒 listen」造成的失敗**全部是 0**。它們在 listen 之後約 2 秒（一個探測週期）才 Ready，入口在 listen 之前一次也沒選到它們；
- **代價**：rollout 變慢。步驟 0 在第 11.2～11.8 秒完成，步驟 1～5 在第 12.3～32.5 秒，3 個新 Pod 都晚 5 秒時最慢；期限是第 50 秒，全部都在期限內完成；
- 加長 `preStop` 對這一類失敗沒有作用，因為它們和舊 Pod 的關閉無關。

Hubble 只有 agent 內 4095 筆的環形緩衝，流量大時舊紀錄會被蓋掉；沒有 Hubble 資料的 Pod，是根據時間上的變化模式推論 DNS 原因。

## 7. 正式量測時關閉入口重試

Koupa 的 HTTPRoute **不寫 `retry`**，入口就不會重試（[env-findings 3.1](docs/env-findings.md#31-重試)）。不要在上課時示範加上 `retry`：

- Cilium 產生的重試條件包含 `reset` 和 `connect-failure`，連 `POST /orders` 也會重試；`retry.attempts: N` 是 N 次重試（最多 N＋1 次上游嘗試），不是總次數；
- 入口重試過的請求，「入口選了哪個 Pod」「伺服器有沒有建立訂單」都無法確定。Task 25 在獨立的 Envoy 1.39.5 環境（`experiments/ingress/run.sh`）打開重試驗證：5 個案例 35 項檢查都通過，入口重試時伺服器端結果一律判成「未知」，成績單的未知會大幅增加。

`preflight` 只核對 Deployment，不核對 HTTPRoute。量測前確認 HTTPRoute 沒有 `retry` 和 `timeouts`（下面的指令應該什麼都不印）：

```bash
kubectl -n koupa get httproute koupa -o jsonpath='{.spec.rules[*].retry}{.spec.rules[*].timeouts}'; echo
```

## 8. 閒置斷線小實驗的結論

[實測紀錄第 4 節](docs/measurements/2026-11-10.md#4-閒置斷線小實驗)：

| 設定 | 結果 | 入口紀錄 |
|---|---|---|
| 沒有心跳，HTTPRoute 沒有 `timeouts` | 第 300 秒被切斷（curl 結束碼 18） | `SI`、`stream_idle_timeout`、`duration_ms` 300001 |
| 心跳 15 秒（`k8s/idle-lab`） | 420 秒內收到 28 次心跳，沒有被切斷 | `DC`（curl 自己結束） |
| 心跳 15 秒＋HTTPRoute `timeouts.request: 60s` | 第 60 秒被切斷，心跳無效 | `UT`、`response_timeout`、`duration_ms` 60000 |

- tkha 的入口預設沒有總時間上限；切斷靜默看板的是 300 秒的閒置逾時；
- 心跳只對閒置逾時有效。路由一旦寫了 `timeouts.request`，不管有沒有心跳，時間到就切斷；
- Koupa 的做法：HTTPRoute 不寫 `timeouts`；要讓靜默的看板保持連線就打開 `SSE_HEARTBEAT`，間隔遠小於 300 秒。步驟 0～5 不開心跳，避免多一個變因；
- 瀏覽器的 `EventSource` 斷線後會自動重連，頁面上看起來只是「閃一下」，要看入口紀錄或 App log（`board_closed reason=client_gone`）才知道發生過。

## 9. 驗證工作表

設計 [6.5](docs/design-v2/06-reference.md#65-不在本階段範圍) 的三個時機。時間是 2026-11-10 在 tkha 上的實測（[實測紀錄第 12 節](docs/measurements/2026-11-10.md#12-這次執行的過程)）；沒有記錄的標「未記錄」。任何一項失敗，先停止上課用的量測，依實作計畫的對應條目調查。

**程式或環境改版後**（下表有記錄時間的項目合計約 2 小時 50 分鐘，不含自動化測試和入口重試實驗）：

| 項目 | 指令（在 `01/` 執行） | 2026-11-10 |
|---|---|---|
| 記錄版本與 Envoy 設定 | 版本記進實測紀錄；`kubectl -n kube-system exec <cilium agent Pod> -c cilium-agent -- cilium-dbg envoy admin config > envoy-config.json`（Pod 名稱用 `kubectl -n kube-system get pods -l k8s-app=cilium` 查） | — |
| App 改了：發布新版本 | 照[設計文件 6.2](docs/design-v2/06-reference.md#62-映像檔) 的發布順序：在 `images.env` 調高兩條線的版本號（新功能加 MINOR、只修 bug 加 PATCH，例如 `v1.2.0`／`v2.2.0` → `v1.2.1`／`v2.2.1`）、`k8s/base` 的映像檔跟著改；合併到 main 之後**馬上**在 main 上 `podman login ghcr.io`、`make publish`（`scripts/publish-images.sh`：從 HEAD 的乾淨匯出建置；tag 已經存在、或沒辦法確定它不存在，都會拒絕；推送後確認 registry 上的 digest 就是剛推的，印出 digest），把 digest 記進 6.2 的版本表；發布新版要寫 release notes（[`docs/releases/`](docs/releases/README.md)，一個版本一個檔案，digest 也補進去），最後才公告。同一時間只能有一個人發布（腳本用 `/tmp/koupa-publish-images.lock` 擋住同一台主機上的第二個執行，跨主機靠作業規定）。離線或開發用叢集的映像檔是自己載入節點的，可以用 `KOUPA_SKIP_REGISTRY_CHECK=1 ./setup.sh` 跳過「映像檔已發布」的檢查（會印警告）。tag 不再覆蓋，所以不用在 worker 上 `crictl pull`、也不用 `rollout restart`：`./setup.sh` 套用新的映像檔字串就會 rolling update。Rush Hour 改了先調高 `RUSH_HOUR_VERSION`，再 `make push-rush-hour`（走同一支腳本：從 HEAD 建置，版本已經存在或查不到 registry 都會拒絕）；`experiments/critical/build-images.sh` | — |
| 自動化測試 | `make test`；`for t in test/measure/test-*.sh; do bash "$t"; done`；`for o in base step-1 step-2 step-3 step-4 step-5 calibrate test-hooks idle-lab; do scripts/check-manifests.sh $o; done`；`experiments/verify/compare-tools_test.sh`；`experiments/critical/test/test-run-summary.sh`；`experiments/ingress/test/ownership.sh` | 全部通過；時間未記錄 |
| 閒置斷線小實驗 | README 的小實驗步驟，外加 HTTPRoute 暫時加 `timeouts.request: 60s` 的一組（做完要移除） | 約 15 分鐘 |
| 關鍵時機實驗 | `experiments/critical/run.sh` | 12／12 PASS，約 7 分鐘 |
| 入口重試和延遲抵達 | `experiments/ingress/run.sh` | 2026-11-09 通過（5 個案例 35 項）；時間未記錄 |
| `calibrate` | `./measure.sh calibrate --runs 8`（達到 5 個有效樣本就停） | 3 次 run、7 個有效樣本，約 10 分鐘 |
| `measure.sh all` | `./measure.sh all`（control、步驟 0～5 交錯 5 輪） | 35 次全部有效，約 75 分鐘 |
| 瀏覽器和另一套工具的交叉檢查 | `experiments/verify/mutation.sh`；`experiments/verify/protocol.sh`；`experiments/verify/compare-tools.sh` | 全部通過，三項合計約 42 分鐘 |
| drain、縮減 Pod 數量（加上退版） | `./measure.sh rollback`；`./measure.sh drain`；`./measure.sh scale-down` | 各 5 次全部 0，合計約 21 分鐘 |

`preStop` 改了秒數（重新校正）之後，至少重跑 control 和步驟 2～5 交錯 5 輪（2026-11-10 是 25 次、約 40 分鐘）。關鍵時機實驗的映像檔如果是私有的，先在主機 `podman login ghcr.io`，腳本會暫時建立 pull secret，結束後刪除。

**每次開課前**（約 5 分鐘）：

```bash
./measure.sh control --runs 1
./measure.sh step-0 --runs 1
```

control 要全部 0 而且沒有未知，步驟 0 要量到問題。這不取代改版後的 5 次基準驗證（`measure.sh all`）。

**課程定稿前**：第三方重跑驗收（第 11 節）。

## 10. 已知現象

- **步驟 0～4 在預設負載下量不到重複訂單**：已確認的重複訂單都是 0（步驟 0、1 只有「伺服器端結果未知」）。步驟 5 的效果由關鍵時機實驗第 1 項證明；
- **叫號鎖等待的尖峰**：等鎖時間中位數約 0.08 ms，但約 1.7% 的叫號等了 100 ms 以上，最長 1.7 秒（`all` 收尾時 2.6 秒；`preStop` 8 秒重跑時 25 次中有 5 次 0.85～1.5 秒），成群出現在幾秒之內，連不升級的 control 也有，所以不是 rollout 造成的。原因沒有調查，也沒有造成成績單上的失敗；
- **Envoy 統計和 access log 的小差額**：每次差 2～9 筆，原因未解釋。逐筆對帳用的是 access log，不受影響（Rush Hour 和入口紀錄算出的兩個失敗數每次都相同）；
- **一個漏號的離群值**：`preStop` 8 秒重跑的步驟 2 第 5 輪，board-2 漏 109 個（其他約 80），原因沒有另外查。步驟 3 第 2 輪 board-2 漏 41 是 3 個新 Pod 都晚 5 秒 listen、rollout 最慢（第 32.5 秒）的那一次；
- **號碼牌循環**：2026-11-10 時資料庫留著先前實驗沒完成的訂單，號碼牌循環後廚房清單出現同號的兩張單。已一次清掉 23137 張（刪除前匯出 CSV，見實測紀錄第 11 節）；號碼牌本身照樣循環，沒有改成跳過未完成的號碼；
- **k6 比較會自己清掉未完成的訂單**：k6 只下單、沒有廚房，每張單都停在「製作中」。`experiments/verify/compare-tools.sh` 每次 k6 量測結束後，在同一個交易裡刪掉這次 k6 量測（`client_ref` 是 `<量測編號>-c<n>`）還在製作中的訂單和相關資料，張數寫在該次的 `cleanup.txt`。清除失敗時這次 k6 量測算無效，整個比較以結束碼 1 結束。要手動清：`experiments/verify/compare-tools.sh cleanup-k6-orders RUNID DIR`。k6 的數字來自 k6 自己的結果，不讀資料庫，清除不會改變比較結果；
- **Rush Hour 的映像檔**：`ghcr.io/braveantony/rush-hour:0.1.0`（manifest digest `sha256:816c3ae3333e0f2393aef83e1907691ec75dbbd4cbc3cf948494f00a3a8c1b8c`，image ID `51cce18663b1…`，從 commit `1b58e14` 建置）。學生用 `podman pull` 取得；`measure.sh` 用主機上這份映像檔的 image ID 執行，找不到就停止並提示 `podman pull`。`rush-hour/` 的程式改了，要重新建置並推送這個映像檔，並依 `Makefile` 的註解調高 `RUSH_HOUR_VERSION`（`measure.sh` 的預設值 `RUSH_HOUR_IMAGE` 也要跟著改）；
- rollback／drain／scale-down 是 `preStop` 7 秒時量的，改成 8 秒之後沒有重跑。

## 11. 第三方重跑驗收

> **狀態：待執行（需要另一個人）**。2026-11-10 的實測全部由講師這一方執行。

**目的**：確認「只照 README 操作」就能重現課程的結論（設計 [6.5](docs/design-v2/06-reference.md#65-不在本階段範圍) 的驗收條件）。

**對方要做的事**：

1. 在一套全新的叢集上（符合 README 第 1 節的條件，沒有跑過 Koupa），clone 課程定稿的 commit；
2. 只看 `01/README.md` 操作，不看這份手冊、不問講師：部署、小實驗、`control`、步驟 0～5（各 5 次）、`rollback`、`drain`、`scale-down`；
3. 每個卡住、看不懂或和 README 描述不同的地方，記下來（哪一節、做了什麼、看到什麼）。

**對方要交回的東西**：

- 整個 `01/evidence/` 目錄（每次量測的證據包，不只是成績單），例如 `tar czf evidence.tgz evidence/`；
- 每個指令的終端機輸出；
- 使用的 commit（`git rev-parse HEAD`）和叢集環境說明（節點數、Kubernetes、Cilium 版本；證據包的 `versions.txt` 也有）；
- 第 3 點的筆記。

**講師怎麼比對**：比的是**定性結論**，不是數字。逐項打勾：

| 結論 | 怎麼看 |
|---|---|
| control 全部 0，沒有未知 | `summary-control-*.txt` |
| 步驟 0 量得到至少一種問題 | `summary-step-0-*.txt` |
| 步驟 1 的「第一次送出沒有成功回應」少於步驟 0；剩下的發生在舊 Pod | 彙整表；`--verbose` 成績單或 `failures.json` 的原因 |
| 步驟 2 以後「第一次送出沒有成功回應」是 0 | 彙整表。不是 0 時，先看「SIGTERM 之後才到的請求」：對方的環境可能需要重新校正 `preStop`（第 5 節） |
| 漏號：步驟 2 還在、步驟 3 變少、步驟 4、5 是 0 | 彙整表 |
| 步驟 0～4 沒有已確認的重複訂單 | 彙整表（量到也不算錯，但要查是哪個時機） |
| rollback、drain、scale-down 沒有失敗和漏號 | 三個彙整表 |
| 證據包完整 | 每個證據包的 `evidence-index.txt` 沒有 `MISSING` |

另外看對方的筆記：README 不夠清楚的地方直接修改 README。任何一項結論不一致，回到實作計畫的對應任務調查（例如送出失敗 → A10、A11；漏號 → A2、A3；對照組 → A13），查清楚之前不定稿。

## 參考資料

- [學生講義](README.md)
- [實測紀錄 2026-11-10](docs/measurements/2026-11-10.md)
- [設計文件](docs/design-v2/README.md)：[量測方法](docs/design-v2/05-measurement.md)、[待實測項目與結果](docs/design-v2/06-reference.md#64-待實測項目)
- [環境盤點](docs/env-findings.md)
- [實作計畫](docs/plan.md)：A11（校正）、A12（關鍵時機）、A13（對照組）、A14（量測工具的驗證）
