> [← 目錄](README.md) ｜ [上一篇](05-measurement.md)

# 6. 參考資料

## 6.1 環境需求

**課前已經準備好**（每個學生一套，和 tk8s 規格相同）：

- 1 個 control plane＋3 個 worker，節點是主機上的 podman container；
- Cilium，並啟用 Gateway API、LoadBalancer IP 池和 L2 廣播（讓 LoadBalancer IP 在節點所在的網段裡連得到）；
- 預設的 StorageClass `local-path`（把資料存在節點本機磁碟上）；
- 主機上可以執行 podman，而且主機連得到 Gateway 分配到的 IP。

**學生自己安裝**：

- CNPG operator（固定版本）。學生講義的第一步就是安裝它，讓學生知道資料庫是怎麼來的。CNPG 在 02 也可能用到。

YAML 不寫死 IP，`measure.sh` 會自動查 Gateway 的 IP。

## 6.2 映像檔

| 映像檔 | 用途 | 在哪裡被拉取 |
|---|---|---|
| `registry.gitlab.com/kubeantony/koupa:v1` | App 舊版 | 叢集節點 |
| `registry.gitlab.com/kubeantony/koupa:v2` | App 新版（售完功能） | 叢集節點 |
| `registry.gitlab.com/kubeantony/rush-hour:<版本>` | 量測工具 | 主機 |

- 都事先建置好，設為公開，不需要登入就能拉取；
- 架構為 `linux/amd64`；
- 標籤固定，不使用 `latest`；
- 課前分別在主機和節點上確認拉得到。

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
│   ├── steps/      step-1 ～ step-4（一層疊一層）
│   └── idle-lab/   閒置斷線小實驗
├── setup.sh        分段部署：資料庫 → 建表 → App
├── measure.sh      一鍵量測與校正（step-N、control、calibrate、rollback、drain、scale-down、all）
├── evidence/       每次量測的證據包（執行時產生，不放進版本控制）
├── docs/
│   ├── design.md     第一版設計文件（保留對照）
│   ├── design-v2/    第二版設計文件（依章節拆成多個檔案）
│   └── plan.md       實作計畫：正確性條件、必測案例、任務拆解
├── README.md       學生講義：環境需求、部署和重跑指令、每一步改什麼、為什麼改、怎麼量測和取得證據包、預期的定性結果
└── TEACHER.md      講師手冊：原理、上課節奏、常見提問、口頭補充內容
```

## 6.4 待實測項目

| 項目 | 實測後要決定的事 | 章節 |
|---|---|---|
| 叫號鎖 | 每秒 20 張負載下的等鎖時間和出餐延遲是否可以接受 | [4.1](04-step-mechanisms.md#41-標記完成與配發叫號編號) |
| 入口切斷長連線 | 是哪一種逾時、切斷時間、心跳間隔，或改調 HTTPRoute。依 Cilium 1.20.4 原始碼，沒寫 `timeouts` 的路由會落到 Envoy 預設 route timeout 15 秒（[env-findings 3.2](../env-findings.md)），待即時 config dump 確認 | [4.3](04-step-mechanisms.md#43-閒置斷線小實驗) |
| `preStop` 秒數、關機寬限時間 | 實際需要等多久；入口多快停止送流量給終止中的 Pod | [4.5](04-step-mechanisms.md#45-步驟-1prestop)、[4.6](04-step-mechanisms.md#46-步驟-2graceful-shutdown) |
| readinessProbe | 要不要加進修正步驟 | [4.9](04-step-mechanisms.md#49-readinessprobe待實測) |
| 各步驟的成績單 | 預期效果是否成立；講義的範例數字換成實測結果 | [4](04-step-mechanisms.md)、[5](05-measurement.md) |
| 重複訂單 | 預設負載下量不量得到 | [5.3](05-measurement.md#53-成績單的定義) |
| 號碼牌循環 | 循環時是否還有未完成的同號訂單 | [3.3](03-app-and-data.md#33-識別碼對照) |
| Rush Hour 預設負載 | 每秒 20 張是否足以看出差異，又不會把系統壓垮 | [5.2](05-measurement.md#52-rush-hour-的角色) |
| 對照組 | 不升級時是否真的 0 失敗；步驟 0 是否量得到問題 | [5.1](05-measurement.md#51-每次都量同一個方向) |
| 入口停止送流量的延遲 | 「開始終止 → 入口不再送流量」要多久、每次差多少 | [5.4](05-measurement.md#54-因果時間線) |
| 時間線的資料來源 | 已查（[env-findings](../env-findings.md)，依原始碼，待 koupa Gateway 建立後確認）：access log 用 `CiliumGatewayClassConfig` 開，輸出到各節點 `cilium-envoy` 的 stdout；admin 是 unix socket，用 `cilium-dbg envoy admin` 取得；入口預設不重試，HTTPRoute 不寫 `retry` 即可；Hubble 預設只開 agent 內建的 server，沒有 Relay 和主機 CLI，要進 agent Pod 執行。待確認：叢集當時不存在，所有項目都還沒用即時設定核對 | [5.4](05-measurement.md#54-因果時間線) |
| 主機資源 | 預設負載下主機會不會成為瓶頸 | [5.3](05-measurement.md#53-成績單的定義) |
| 送單時段 | 預設 60 秒、第 10 秒觸發變更，是否容納得下 rollout | [5.1](05-measurement.md#51-每次都量同一個方向) |
| 主機資源門檻、`preStop` 緩衝秒數 | 排程延遲和 CPU 多高算「主機資源吃緊」；校正值要加多少緩衝 | [5.3](05-measurement.md#53-成績單的定義)、[5.4](05-measurement.md#54-因果時間線) |
| drain、縮減 Pod 數量 | 在這兩種終止情況下，各項保證和量測結果是否成立 | [5.7](05-measurement.md#57-其他-pod-終止的情況) |

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
| 01 | Rolling update 時長連線和短連線的處理 | 下午茶尖峰時段系統要升級，點餐不能掉單，叫號不能漏號 |
| 02 | 程式升級、退版時，資料庫 schema 怎麼跟著變 | 每杯飲料要記錄「甜度、冰塊」，資料表要加欄位，出問題要能退版 |
| 03 | 用 Cilium Gateway API 分流做金絲雀部署（先把少量流量導到新版） | 夏季限定新品「旺來綠茶」上市，先開放給 10% 的客人 |

| 項目 | 內容 |
|---|---|
| 店名 | **Koupa 酷杯**：希臘文的「杯子」（κούπα），念起來像 Kube |
| 技術名稱 | `koupa`，用在映像檔、Deployment、Service、HTTPRoute 和 namespace |
| 口號 | 微糖、去冰、零停機 |
| 菜單 | 滾動珍珠奶茶、Pod 沫紅茶、微服務綠茶、容器冬瓜茶 |
| 03 新品 | 旺來綠茶（鳳梨綠茶）。黃色呼應金絲雀，台語「旺來」也有生意興旺的意思 |
| 量測工具 | **Rush Hour**，技術名稱 `rush-hour` |

## 6.7 保證的來源

每個結論都標明它靠的是什麼，讀者才知道換一個環境還成不成立。「規範」在任何符合規範的環境都成立；「App 設計」要靠實作計畫的必測案例證明；「環境實測」只代表 tk8s 和文件記錄的版本。

| 結論 | 來源類型 | 依據 | 前提或待確認 |
|---|---|---|---|
| `preStop` 執行完才送 SIGTERM；刪除 Pod 後，端點更新和 kubelet 關機各自進行 | 規範 | [Pod 生命週期：Pod 的終止](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-termination) | — |
| 終止中的 Pod 在 EndpointSlice 標成 `terminating`，`ready` 變成 false | 規範 | [Pod 終止與 EndpointSlice](https://kubernetes.io/docs/tutorials/services/pods-and-endpoint-termination-flow/) | — |
| `deletionTimestamp` 是預計刪除的時間 | 規範 | [ObjectMeta](https://pkg.go.dev/k8s.io/apimachinery/pkg/apis/meta/v1#ObjectMeta) | — |
| 入口多久才停止送流量給終止中的 Pod | 環境實測 | `measure.sh calibrate`（5.4） | Cilium 1.20.4、Envoy 1.39.5（tk8s `versions.yaml` 固定的版本；叢集實際版本待確認，見 [env-findings 1](../env-findings.md)） |
| 入口的逾時和重試設定 | 環境實測 | tk8s 上 Envoy 的即時設定；`cilium-dbg envoy admin config`（[env-findings 2.1、3](../env-findings.md)） | 目前只有 Cilium 1.20.4 原始碼的推論（預設不重試；route timeout 15 秒；stream idle 300 秒），即時 config dump 待確認（4.3） |
| `Shutdown` 先關 listener，再等進行中的請求；`RegisterOnShutdown` 在關機開始時執行 | 規範 | [Go `net/http` Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown) | 以實作使用的 Go 版本為準 |
| 沒有 `data:` 行的 SSE 事件不會觸發；重連時帶上 `Last-Event-ID`；`retry:` 設定重連間隔 | 規範 | [WHATWG Server-sent events](https://html.spec.whatwg.org/multipage/server-sent-events.html) | 用實際瀏覽器驗證（5.6） |
| `NOTIFY` 在交易提交後才送出；`LISTEN` 生效前的通知收不到 | 規範 | [PostgreSQL NOTIFY](https://www.postgresql.org/docs/current/sql-notify.html)、[LISTEN](https://www.postgresql.org/docs/current/sql-listen.html) | 以 CNPG 使用的 PostgreSQL 版本為準 |
| sequence 取號不會因 rollback 退回，所以 `ready_seq` 會跳號 | 規範 | [PostgreSQL 序列函式](https://www.postgresql.org/docs/current/functions-sequence.html) | — |
| 叫號編號照提交順序、看板交接不漏不重複 | App 設計 | 4.1、4.2；實作計畫 A1～A3 | 必測案例全部通過 |
| 重送不重複下單 | App 設計 | 4.8；實作計畫 A6 | 必測案例全部通過；key 提交後至少保留 24 小時 |
| 各步驟的成績差異 | 環境實測 | 5.1～5.7 | 只代表單一主機、單一資料庫實例 |
| 哪些操作會重啟資料庫 | 規範 | [CloudNativePG v1.30.1 文件](https://github.com/cloudnative-pg/cloudnative-pg/tree/v1.30.1/docs/src) | CNPG v1.30.1；官方支援 Kubernetes 1.34～1.36，tk8s 的 1.37 屬於「測過、不在支援清單」（[supported_releases](https://github.com/cloudnative-pg/cloudnative-pg/blob/v1.30.1/docs/src/supported_releases.md)） |
| 整筆交易的總時間上限（`transaction_timeout`），用來保證 key 提交後至少保留 24 小時 | 規範 | [PostgreSQL 用戶端連線設定](https://www.postgresql.org/docs/17/runtime-config-client.html) | 需要 PostgreSQL 17 以上；映像檔固定為 `ghcr.io/cloudnative-pg/postgresql:17.11-202611020810-system-trixie`，index digest `sha256:03a1c6a62b27e8ad31cae5c085131b5ac706b09403d6fcb6c58d16c8daa257cd`（[env-findings 5](../env-findings.md)） |

---

> [← 目錄](README.md) ｜ [上一篇](05-measurement.md)
