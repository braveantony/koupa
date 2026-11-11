> [← 目錄](README.md) ｜ [上一篇](05-measurement.md)

# 6. 參考資料

## 6.1 環境需求

**課前已經準備好**（每個學生一套）：

- 3 個 worker，節點是主機上的 podman container；control plane 的數量不限（2026-11-10 實測的 tkha 是 3 個 control plane＋3 個 worker）。koupa 的 3 個 Pod 每個 worker 放一個，`measure.sh drain` 要求每個 worker 剛好一個 Pod；
- Cilium，並啟用 Gateway API、LoadBalancer IP 池和 L2 廣播（讓 LoadBalancer IP 在節點所在的網段裡連得到）；
- StorageClass `local-path`（把資料存在節點本機磁碟上）。`k8s/db/cluster.yaml` 明確指定它，不依賴叢集的預設 StorageClass；
- 主機上可以執行 podman，而且主機連得到 Gateway 分配到的 IP。

**由 `setup.sh` 安裝**：

- CNPG operator（固定 v1.30.1）。`setup.sh` 的第一段會安裝它，再建立資料庫，讓學生知道資料庫是怎麼來的。CNPG 在 02 也可能用到。

YAML 不寫死 IP，`measure.sh` 會自動查 Gateway 的 IP。

## 6.2 映像檔

| 映像檔 | 用途 | 在哪裡被拉取 |
|---|---|---|
| `ghcr.io/braveantony/koupa:v1.2.0` | App 舊版（v1 這條線的目前版本） | 叢集節點 |
| `ghcr.io/braveantony/koupa:v2.2.0` | App 新版（v2 這條線，有售完功能） | 叢集節點 |
| `ghcr.io/braveantony/rush-hour:<版本>` | 量測工具 | 主機 |

- 都事先建置好，設為公開，不需要登入就能拉取；
- 架構為 `linux/amd64`；
- 標籤固定，不使用 `latest`；
- 課前分別在主機和節點上確認拉得到。
- 目前 Rush Hour 的版本是 `0.1.0`（manifest digest 記在[實測紀錄](../measurements/2026-11-10.md#2-映像檔與-digest)）。

**版本規則**：koupa 的 tag 用 Kubernetes 那種語意化版本 `vMAJOR.MINOR.PATCH`。

- MAJOR 是版本線：`1` 是基本的 App（沒有售完標示），`2` 是加上售完標示的 App。課程說的「v1 升級到 v2」就是從 1 這條線換到 2 這條線；
- MINOR 加一表示有新功能（包括畫面改版），PATCH 加一表示只修 bug；
- 兩條線一起改版，MINOR.PATCH 相同（例如 `v1.2.0` 和 `v2.2.0`）；
- **推上 ghcr 的 tag 不再覆蓋**。內容變了就換新的版本號，所以 Deployment 的映像檔字串一定會變，Kubernetes 會照平常的 rolling update 拉新的映像檔；
- 目前的版本只寫在 `01/images.env`（`KOUPA_V1_TAG`、`KOUPA_V2_TAG`）。`k8s/base` 用 `KOUPA_V1_TAG`（`scripts/check-manifests.sh` 會檢查兩者一致），`measure.sh`、`scripts/preflight.sh`、`scripts/evidence.sh` 和實驗腳本都從這裡讀；
- App 的版本字串就是完整的 tag（建置時 `VERSION=v1.2.0`），`/api/whoami`、看板的 hello 事件和 `/koupa --version` 都會顯示；有沒有售完功能看 MAJOR（2 以上才有），不是比對整個字串；
- 發布用 `scripts/publish-images.sh`（`make publish`）：從 HEAD 的乾淨匯出建置兩條線，tag 已經在 ghcr 上就拒絕推送；`--retag-history` 把下表較早的版本號補到既有的 digest 上，同樣不覆蓋。登入資訊用 `podman login` 存的那一份，不放在命令列上；
- 「不覆蓋」是靠這個發布流程做到的，**registry 本身不會擋**：ghcr 沒有「tag 不存在才建立」的原子操作。所以同一時間只能有一個人（講師）發布。腳本用固定的鎖檔 `/tmp/koupa-publish-images.lock` 擋住同一台主機上的第二個執行；不同主機之間沒有鎖，靠作業規定（只由講師從一個地方發布）；每個 tag 推送或複製前會再查一次確實不存在（只有 registry 對這個 tag 回「manifest unknown」才算不存在，連不上、沒有權限等其他錯誤一律拒絕），推送後馬上確認 registry 上的 digest 就是剛推的那一個，不一致就大聲報錯停下來。

**發布順序**（`k8s/base` 和 `images.env` 合併後就指向新版本號，所以中間不能拖）：

1. 在分支上調高 `images.env` 兩條線的版本號，`k8s/base` 跟著改（`scripts/check-manifests.sh` 會檢查一致）；
2. 合併到 main；
3. **馬上**在 main 的乾淨工作目錄 `podman login ghcr.io`、`make publish`；
4. 把印出的 digest 記進下表、commit；
5. 最後才公告新版本。

在第 3 步完成之前，用到這個 commit 的人跑 `setup.sh` 會在改動叢集之前就停下來，顯示「映像檔 v1.2.0 還沒發布」；`measure.sh` 的 preflight 也會因為解析不到 tag 而不開始量測。

**版本紀錄**（manifest digest，`ghcr.io/braveantony/koupa`）：

| 版本 | 內容 | v1 這條線 | v2 這條線 |
|---|---|---|---|
| `v1.0.0`／`v2.0.0` | 第一階段 App，2026-11-10 量測用的版本（從 commit `1b58e14` 建置） | `sha256:e34801f1457f6026984c3b9095cd32eecb91f158d32b8475e5374a554ccd9fb3` | `sha256:935764966ab0f6d68e1b292bf18befcdf33310ceed073d17fa77a43ded69b891` |
| `v1.1.0`／`v2.1.0` | 奶茶店風格的畫面改版、飲料照片、純 HTTP 下點餐的修正（從 commit `a2d5fbf` 建置） | `sha256:e767bd6d12d316733c826da07b387674791c4f7e1e11cff8d5f1b8aa0459bdea` | `sha256:082a7412ec4de56f160c7d1bbe91162a7f119a4e0742421345d866c609648cf4` |
| `v1.2.0`／`v2.2.0` | 杯封膜風格的畫面（PR #8）；2026-11-11 從 main `b82c589` 建置 | `sha256:58ba573aa03db542eb0943966237bc4836b341ca72bc9de20c1e6aca9fd40312` | `sha256:fab97b7b17a46ba624294939858d95587745a0caa9cc76f53c55ed0a7215d278` |

`v1.1.0` 和 `v1.2.0` 之間曾經推過一版有錯的建置（`5f97bce7…`／`90065b38…`），沒有給版本號，不要使用。

**舊的浮動 tag**：`koupa:v1`、`koupa:v2` 是以前「同一個 tag 重複覆蓋」的做法，已經停用。它們停在現在的位置（等於 `v1.1.0`、`v2.1.0`），不會再更新，程式和文件也都不再引用。實測紀錄裡寫的 `koupa:v1`、`koupa:v2` 是當時的名字，對應的版本見[實測紀錄第 2 節](../measurements/2026-11-10.md#2-映像檔與-digest)的補記。

學生不用安裝 Go，也不用自己建置映像檔。

## 6.3 交付內容

```
01/
├── app/            Koupa App 的 Go 程式碼（三個頁面、/api/audit）
├── rush-hour/      量測工具的 Go 程式碼
├── migrations/     golang-migrate 的 SQL 檔
├── k8s/
│   ├── db/         CNPG Cluster
│   ├── migrate/    migration Job
│   ├── base/       App、Service、Gateway、HTTPRoute
│   ├── steps/      step-1 ～ step-5（一層疊一層）
│   └── idle-lab/   閒置斷線小實驗
├── setup.sh        分段部署：資料庫 → 建表 → App
├── measure.sh      一鍵量測與校正（step-N、control、calibrate、rollback、drain、scale-down、all）
├── evidence/       每次量測的證據包（執行時產生，不放進版本控制）
├── docs/
│   ├── design.md     第一版設計文件（保留對照）
│   ├── design-v2/    第二版設計文件（依章節拆成多個檔案）
│   ├── koupa-app.md  範例應用介紹：組成、部署、確認連得到、清除
│   └── plan.md       實作計畫：正確性條件、必測案例、任務拆解
├── README.md       學生講義：環境需求、部署和重跑指令、每一步改什麼、為什麼改、怎麼量測和取得證據包、預期的定性結果
└── TEACHER.md      講師手冊：原理、上課節奏、常見提問、口頭補充內容
```

## 6.4 待實測項目

結果欄是 2026-11-10 在 tkha 上的實測（[實測紀錄](../measurements/2026-11-10.md)），只代表這個環境；換了環境要重新量。

| 項目 | 實測後要決定的事 | 結果 | 章節 |
|---|---|---|---|
| 叫號鎖 | 每秒 20 張負載下的等鎖時間和出餐延遲是否可以接受 | **已實測，可以接受**：35 次量測的等鎖時間中位數 0.08 ms；約 1.7% 的叫號等了 100 ms 以上（最長 1.7 秒，收尾時 2.6 秒），成群出現在幾秒之內，沒有升級的 control 也有。這些等待沒有造成成績單上的失敗（control 5 次都是 0）。尖峰的原因**待查** | [4.1](04-step-mechanisms.md#41-標記完成與配發叫號編號) |
| 入口切斷長連線 | 是哪一種逾時、切斷時間、心跳間隔，或改調 HTTPRoute | **已實測**：沒寫 `timeouts` 的路由沒有總時間上限（[env-findings 3.2](../env-findings.md#32-逾時)）；閒置的看板在第 300 秒被 `stream_idle_timeout` 切斷（`SI`）；心跳 15 秒（`k8s/idle-lab`）後 420 秒不斷；`timeouts.request: 60s` 時第 60 秒切斷（`UT`），心跳無效。做法：HTTPRoute 不寫 `timeouts`，需要時開心跳 | [4.3](04-step-mechanisms.md#43-閒置斷線小實驗) |
| `preStop` 秒數、關機寬限時間 | 實際需要等多久；入口多快停止送流量給終止中的 Pod | **已實測**：calibrate 兩次：Task 23b 7 個有效樣本、候選值 1.504 秒 → 7 秒；2026-11-10 重跑 7 個有效樣本、候選值 2.127 秒 → **8 秒**（照公式取最新一次校正所有有效樣本的最大值；上界全部是模型推估，緩衝 5 秒暫定）。現行 `preStop` 8 秒、寬限 40 秒（8＋15＋5 ＜ 40）。步驟 2～5 改成 8 秒之後重跑的 20 次量測，舊 Pod 收到 SIGTERM 之後被送到的請求造成的失敗都是 0（7 秒時的 20 次也是 0；[實測紀錄 2026-11-10 第 8.4 節](../measurements/2026-11-10.md#84-prestop-8-秒重跑control步驟-25-交錯-5-輪)） | [4.6](04-step-mechanisms.md#46-步驟-2prestop)、[4.7](04-step-mechanisms.md#47-步驟-3graceful-shutdown) |
| readinessProbe | 要不要加進修正步驟 | **已實測，加成步驟 1**：約 28% 的新 Pod 晚約 5 秒才 listen，沒有 probe 時已經 Ready 而收到請求（`UF`／`Connection refused`）（[env-findings 第 9、10 節](../env-findings.md#10-readinessprobe-成為步驟-1-之後的實測task-23c)）。`all` 再確認：90 個新 Pod 中 27 個晚 5 秒；步驟 0 有這種 Pod 的 4 次各多 34～44 張失敗，步驟 1～5 的 25 次這類失敗都是 0 | [4.5](04-step-mechanisms.md#45-步驟-1readinessprobe) |
| 各步驟的成績單 | 預期效果是否成立；講義的範例數字換成實測結果 | **已實測**（每個條件 5 次）：「第一次送出沒有成功回應」步驟 0 中位數 51（8～53）→ 步驟 1 7（4～12）→ 步驟 2～5 都是 0；漏號步驟 2 中位數約 80 → 步驟 3 約 20 → 步驟 4、5 是 0；重複訂單見下一列。講義的範例數字用實測紀錄第 8 節 | [4](04-step-mechanisms.md)、[5](05-measurement.md) |
| 重複訂單 | 預設負載下量不量得到 | **已實測，量不到**：步驟 0～4 的 25 次，確定的重複訂單都是 0（步驟 0、1 只有「伺服器端結果未知」）。步驟 5 的效果靠關鍵時機實驗第 1 項證明（防重複關閉時 2 張、打開時拿回同一張） | [5.3](05-measurement.md#53-成績單的定義) |
| 號碼牌循環 | 循環時是否還有未完成的同號訂單 | **已觀察到，已處理**：2026-11-10 時資料庫留著之前實驗沒完成的訂單（23137 張，主要是 k6 只下單沒有廚房），號碼牌（1～9999）循環後同號的未完成訂單同時存在（當天 61948 張訂單建立時就有同號舊單）。成績單用 order_id／ready_seq，不受影響；但廚房清單會出現同號的兩張單。處理：一次清掉這 23137 張（刪除前匯出 CSV），k6 比較改成每次量測後清掉自己的未完成訂單（實測紀錄第 11 節）。仍有的風險：號碼牌本身照樣循環，只要有一張單超過 9999 張新單都還沒完成（例如以後別的實驗又留下沒做完的單），同號的未完成訂單還是會同時出現；號碼牌沒有改成跳過未完成的號碼 | [3.3](03-app-and-data.md#33-識別碼對照) |
| Rush Hour 預設負載 | 每秒 20 張是否足以看出差異，又不會把系統壓垮 | **已實測，足夠**：步驟 0、1 量得出差異；control 5 次都是 0；主機 CPU 最高 41% | [5.2](05-measurement.md#52-rush-hour-的角色) |
| 對照組 | 不升級時是否真的 0 失敗；步驟 0 是否量得到問題 | **已實測**：control 5 次都是 0 失敗、0 漏號、0 重複、沒有未知；步驟 0 每一次都量到問題 | [5.1](05-measurement.md#51-每次都量同一個方向) |
| 入口停止送流量的延遲 | 「開始終止 → 入口不再送流量」要多久、每次差多少 | **已實測**：入口最後一次把請求送給終止中的 Pod，是刪除請求後 0.39～1.46 秒（Task 23b）、0.65～1.18 秒（2026-11-10）；模型推估的上界 1.01～2.13 秒。每個 Pod 差約 0.5 秒，同一次 rollout 的 3 個 Pod 也不同 | [5.4](05-measurement.md#54-因果時間線) |
| 時間線的資料來源 | 已在 tkha 實測（[env-findings](../env-findings.md)） | access log 用 `CiliumGatewayClassConfig` 開，但只能掛在專屬 GatewayClass 的 `parametersRef`（Gateway 層級不支援），輸出到各節點 `cilium-envoy` 的 stdout，只有收到請求的節點有 log；admin 是 unix socket，用 `cilium-dbg envoy admin` 取得（容器內無 curl）；入口預設不重試，HTTPRoute 不寫 `retry` 即可，寫 `retry.attempts: N` 則最多 N+1 次上游嘗試；Hubble 在 agent Pod 內可用（`/usr/bin/hubble`，看得到 Gateway 的 HTTP 請求與耗時，但沒有上游位址和嘗試次數），沒有 Relay 和主機 CLI。stream idle 的實際切斷行為已實測（見「入口切斷長連線」）。仍待確認：terminating 端點在 Envoy 的處理（calibrate 的上界全部是模型推估） | [5.4](05-measurement.md#54-因果時間線) |
| 主機資源 | 預設負載下主機會不會成為瓶頸 | **已實測，不會**：每秒取樣的主機 CPU 最高 41%（步驟 0），其他條件 32～36% | [5.3](05-measurement.md#53-成績單的定義) |
| 送單時段 | 預設 60 秒、第 10 秒觸發變更，是否容納得下 rollout | **已實測，容納得下**：rollout 最晚在第 32.5 秒完成（`preStop` 8 秒重跑的步驟 3；7 秒時最晚 32.4 秒，都是 3 個新 Pod 都晚 5 秒 listen 的那次），期限第 50 秒；步驟 0 約第 11.5 秒 | [5.1](05-measurement.md#51-每次都量同一個方向) |
| 主機資源門檻、`preStop` 緩衝秒數 | 排程延遲和 CPU 多高算「主機資源吃緊」；校正值要加多少緩衝 | 維持待定：主機一直沒有吃緊（CPU 最高 41%），量不到門檻該設在哪裡；緩衝 5 秒仍是暫定值，但步驟 2～5 的 40 次（`all` 裡 `preStop` 7 秒的 20 次＋8 秒重跑的 20 次）舊 Pod 失敗都是 0，沒有證據顯示需要更大 | [5.3](05-measurement.md#53-成績單的定義)、[5.4](05-measurement.md#54-因果時間線) |
| drain、縮減 Pod 數量 | 在這兩種終止情況下，各項保證和量測結果是否成立 | **已實測**（步驟 5 設定）：drain（tkha-worker-3）5 次、scale-down 5 次、rollback 5 次，成績單全部是 0；drain 後 uncordon，分布回到 1／1／1 | [5.7](05-measurement.md#57-其他-pod-終止的情況) |

## 6.5 不在本階段範圍

**口頭說明、不實作**（放在講師手冊）：

- 把關機寬限時間設很長，等看板自己斷線：如果 App 不主動結束看板連線，連線也沒有因為其他原因中斷，升級就可能一直卡到寬限時間用完；
- 多台看板錯開時間重連，避免同時擠到新 Pod：實務上常見的細節，當作延伸內容。

**後續候選（先記錄，不做）**：

- 看板多一欄「製作中」：左邊列出製作中的號碼，右邊列出請取餐的號碼。這要讓 SSE 多送「新訂單」事件，升級期間新舊版並存時會多出事件相容性的問題，所以不放進 01。

**原則**：本階段追求更深入，不是更大。只有直接支撐驗證時，才增加工具或觀測能力。

**結論的邊界**：結果只代表單一主機、單一資料庫實例、文件記錄的版本和設定。講義和講師手冊要寫明這一點。

**驗收**：另一個人在全新的叢集上，只照 README 操作，跑完所有步驟，交回證據包（不只是成績單），定性結論和講師的實測一致。

**講師的驗證工作**（寫進講師手冊，註明執行次數和預估時間，時間待實測）：

| 時機 | 要做的事 |
|---|---|
| 程式或環境改版後 | 記錄實際的 Cilium、Envoy、CNPG、PostgreSQL 版本，補上 6.7 對應版本的依據連結，包括該 Cilium 版本 Gateway API 轉換成 Envoy 設定的原始碼，並保存實際的 Envoy 設定和取得方式（取不到的標為缺少）；自動化測試（必測案例、已知答案測試、關鍵時機實驗）、`calibrate`、`measure.sh all`、瀏覽器和另一套工具的交叉檢查、drain 和縮減 Pod 數量 |
| 每次開課前 | `control` 一次、步驟 0 一次的快速檢查，確認環境沒有異常。這不取代改版後依實作計畫 A13 做的 5 次基準驗證 |
| 課程定稿前 | 第三方重跑驗收 |

任何一項失敗，先停止上課用的量測，依實作計畫的對應條目調查。

**不做**：付款、會員、外送、資料庫高可用；投影片（內容定案後可以另外整理成投影片大綱）。

## 6.6 課程三階段與命名

同一個 App 貫穿三個階段。`01/`、`02/`、`03/` 各自包含可以獨立使用的完整檔案，學生可以從任何一個階段開始。01 定案後才複製到 02，02 的講義會列出跟 01 相比改了哪些檔案。

| 階段 | 技術主題 | 店裡的故事 |
|---|---|---|
| 01 | Rolling update 時怎麼處理長連線和短連線 | 下午茶尖峰時段系統要升級，點餐不能掉單，叫號不能漏號 |
| 02 | 程式升級、退版時，資料庫 schema 怎麼跟著變 | 每杯飲料要記錄「甜度、冰塊」，資料表要加欄位，出問題要能退版 |
| 03 | 用 Cilium Gateway API 分流做金絲雀部署（先把少量流量導到新版） | 夏季限定新品「旺來綠茶」上市，先開放給 10% 的客人 |

| 項目 | 內容 |
|---|---|
| 店名 | **Koupa 酷杯**：希臘文的「杯子」（κούπα），念起來像 Kube |
| 技術名稱 | `koupa`，用在映像檔、Deployment、Service、HTTPRoute 和 namespace |
| 口號 | 課程標語「微糖、去冰、零停機」；App 畫面上的口號是「一杯好茶，隨時都在」 |
| 菜單 | 波霸奶茶、晚霞紅茶、綠茶三劍客、冬瓜檸檬（migration 000005 改名；原本是滾動珍珠奶茶、Pod 沫紅茶、微服務綠茶、容器冬瓜茶） |
| 03 新品 | 旺來綠茶（鳳梨綠茶）。黃色呼應金絲雀，台語「旺來」也有生意興旺的意思 |
| 量測工具 | **Rush Hour**，技術名稱 `rush-hour` |

## 6.7 保證的來源

每個結論都標明它靠的是什麼，讀者才知道換一個環境還成不成立。「規範」在任何符合規範的環境都成立；「App 設計」要靠實作計畫的必測案例證明；「環境實測」只代表 tk8s 和文件記錄的版本。

| 結論 | 來源類型 | 依據 | 前提或待確認 |
|---|---|---|---|
| `preStop` 執行完才送 SIGTERM；刪除 Pod 後，端點更新和 kubelet 關機各自進行 | 規範 | [Pod 生命週期：Pod 的終止](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-termination) | — |
| 終止中的 Pod 在 EndpointSlice 標成 `terminating`，`ready` 變成 false | 規範 | [Pod 終止與 EndpointSlice](https://kubernetes.io/docs/tutorials/services/pods-and-endpoint-termination-flow/) | — |
| `deletionTimestamp` 是預計刪除的時間 | 規範 | [ObjectMeta](https://pkg.go.dev/k8s.io/apimachinery/pkg/apis/meta/v1#ObjectMeta) | — |
| 入口多久才停止送流量給終止中的 Pod | 環境實測 | `measure.sh calibrate`（5.4） | Cilium 1.20.4、Envoy 1.39.5（已在 tkha 實測，Kubernetes 1.37.2、CRI-O 1.37.2、Gateway API v1.6.3，見 [env-findings 1.4](../env-findings.md)） |
| 入口的逾時和重試設定 | 環境實測 | tk8s 上 Envoy 的即時設定；`cilium-dbg envoy admin config`（[env-findings 2.1、3](../env-findings.md)） | 已用 tkha 即時 config dump 實測：預設不重試；沒寫 `timeouts` 時**沒有** route timeout（不是 15 秒）；stream idle 300 秒（config 已確認，切斷行為待測）；`retry.attempts: N` 是 N 次重試（見 [env-findings 3](../env-findings.md)）；4.3 要依此修正 |
| `Shutdown` 先關 listener，再等進行中的請求；`RegisterOnShutdown` 在關機開始時執行 | 規範 | [Go `net/http` Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown) | 以實作使用的 Go 版本為準 |
| 沒有 `data:` 行的 SSE 事件不會觸發；重連時帶上 `Last-Event-ID`；`retry:` 設定重連間隔 | 規範 | [WHATWG Server-sent events](https://html.spec.whatwg.org/multipage/server-sent-events.html) | 用實際瀏覽器驗證（5.6） |
| `NOTIFY` 在交易提交後才送出；`LISTEN` 生效前的通知收不到 | 規範 | [PostgreSQL NOTIFY](https://www.postgresql.org/docs/current/sql-notify.html)、[LISTEN](https://www.postgresql.org/docs/current/sql-listen.html) | 以 CNPG 使用的 PostgreSQL 版本為準 |
| sequence 取號不會因 rollback 退回，所以 `ready_seq` 會跳號 | 規範 | [PostgreSQL 序列函式](https://www.postgresql.org/docs/current/functions-sequence.html) | — |
| 叫號編號照提交順序、看板交接不漏不重複 | App 設計 | 4.1、4.2；實作計畫 A1～A3 | 必測案例全部通過 |
| 重送不重複下單 | App 設計 | 4.9；實作計畫 A6 | 必測案例全部通過；key 提交後至少保留 24 小時 |
| 各步驟的成績差異 | 環境實測 | 5.1～5.7 | 只代表單一主機、單一資料庫實例 |
| 哪些操作會重啟資料庫 | 規範 | [CloudNativePG v1.30.1：rolling updates](https://github.com/cloudnative-pg/cloudnative-pg/blob/v1.30.1/docs/src/rolling_update.md) | CNPG v1.30.1；官方支援 Kubernetes 1.34～1.36，tk8s 的 1.37 屬於「測過、不在支援清單」（[supported_releases](https://github.com/cloudnative-pg/cloudnative-pg/blob/v1.30.1/docs/src/supported_releases.md)） |
| 整筆交易的總時間上限（`transaction_timeout`），用來保證 key 提交後至少保留 24 小時 | 規範 | [PostgreSQL 用戶端連線設定](https://www.postgresql.org/docs/17/runtime-config-client.html) | 需要 PostgreSQL 17 以上；映像檔固定為 `ghcr.io/cloudnative-pg/postgresql:17.11-202611020810-system-trixie`，index digest `sha256:03a1c6a62b27e8ad31cae5c085131b5ac706b09403d6fcb6c58d16c8daa257cd`（[env-findings 5](../env-findings.md)） |

---

> [← 目錄](README.md) ｜ [上一篇](05-measurement.md)
