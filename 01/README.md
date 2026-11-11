# Koupa 酷杯 01：升級時不掉單、不漏號

> 微糖、去冰、零停機

## 先看一張圖

Pod 是 Kubernetes 執行程式的單位；這裡每個 koupa Pod 都跑一份 App。

```
主機（叢集外）：Rush Hour（量測工具，用 podman 執行）
  客人：每秒 20 張    廚房：按完成      3 台叫號看板
      │ 短連線          │ 短連線           │ 長連線
      └─────────────────┼──────────────────┘
                        ▼
              Cilium Gateway（系統入口）
                        │
       ┌────────────────┼────────────────┐
       ▼                ▼                ▼
  koupa Pod 1      koupa Pod 2      koupa Pod 3
       └────────────────┼────────────────┘
                        ▼
             PostgreSQL（存訂單和叫號）
```

升級時，Kubernetes 會把 Pod 一個一個換成新版。來看 Pod 1 被換掉的那幾秒：

```
1. 看板 1 連在 Pod 1，畫面顯示到 0057 號
2. 升級開始，Pod 1 要被關掉
3. 入口還不知道，又把一張新訂單送給 Pod 1 → 客人看到「送出失敗」
4. Pod 1 的程式結束，看板 1 斷線
5. 斷線這幾秒，0058、0059 號做好了
6. 看板 1 重新連上 Pod 3，從「現在」開始收 → 0058、0059 永遠不會出現在看板 1
```

**一句話**：Pod 被關掉時，「通知入口別再送過來」和「通知程式關機」是同時進行的，彼此不會等對方完成；這個階段用 5 個步驟一一補上漏洞，並用量測數字確認每一步的效果。

## 一頁看懂

你會套用不同的 Kubernetes YAML；程式端的修正已經寫好，各步驟會用環境變數打開。每一步都用 Rush Hour 量 5 次「升級 v1 → v2」，看成績單上三個數字怎麼變：

- **第一次送出沒有成功回應**（下面簡稱「送出失敗」）：客人第一次按送出，沒拿到號碼牌的張數；
- **大螢幕漏號**：每台看板少收到幾個叫號；
- **重複訂單**：同一次點餐在資料庫裡變成兩張以上。

| 步驟 | 改什麼 | 你應該看到（2026-11-10 實測，每個條件 5 次） |
|---|---|---|
| 小實驗 | 打開看板心跳（定期送出的短訊息） | 沒有心跳時，閒置的看板在第 300 秒被入口切斷；打開心跳就不會斷 |
| 0 | 什麼都不做 | 送出失敗中位數 51 張、每台看板漏號數十個 |
| 1 | `readinessProbe` | 送出失敗降到中位數 7 張；漏號沒有改善 |
| 2 | `preStop` 等 8 秒 | 送出失敗變成 0；漏號仍然每台約 80 個 |
| 3 | 優雅關閉（graceful shutdown：關機前先收尾） | 漏號降到每台約 22 個 |
| 4 | 重連後補號 | 漏號變成 0 |
| 5 | 重送不重複下單 | 成績單和步驟 4 一樣（這次預設負載下沒有確認到重複訂單）；效果由講師用關鍵時機實驗示範 |

成績單上的 0 只代表「這幾次沒觀察到」，不代表「不會發生」。數字只代表這個環境，詳見[預期結果](#預期結果)。

---

## 1. 準備

**指令都在哪裡執行**：在主機（跑 podman 節點、連得到 Gateway IP 的那台電腦）上。先把課程的 repo clone 到主機，再進入 `01/` 目錄：

```bash
git clone https://github.com/braveantony/koupa.git
cd koupa/01
```

下面的指令都在 `01/` 目錄執行。

**叢集**（課前由講師準備好）：

- 3 個 worker（執行 App 的叢集節點）。koupa 的 3 個 Pod 每個 worker 放一個；`drain` 模式要求每個 worker 剛好一個 Pod；
- Cilium，並啟用 Gateway API、LoadBalancer IP 池和 L2 廣播（讓主機連得到 Gateway 分配到的 IP）；
- StorageClass `local-path`（把資料存在節點本機磁碟上；資料庫的設定 `k8s/db/cluster.yaml` 指定用它）；
- `kubectl` 的目前 context（kubectl 要操作哪個叢集的設定）指向這個叢集。

確認方式：

```bash
kubectl config current-context    # 名稱要是講師給你的叢集
kubectl get nodes                  # 3 個 worker 都是 Ready
kubectl get storageclass local-path
kubectl get crd gateways.gateway.networking.k8s.io
```

**主機上的工具**：`kubectl`（內建 Kustomize）、`podman`、`skopeo`、`python3`（含 PyYAML）、`git`、`curl`。主機要連得到 `ghcr.io`、`docker.io` 和 `github.com`。不需要安裝 Go，因為要編譯的部分都在 podman container 裡跑。

**拉取 Rush Hour 的映像檔**（第一次量測前做一次；`measure.sh` 用主機上的這一份，不會自己拉取）：

```bash
podman pull ghcr.io/braveantony/rush-hour:0.1.0
```

只有修改 Rush Hour 程式的人才需要自己建置（`make images` 會連 App 映像檔一起建置）。

App 的映像檔是 ghcr 上公開的 `ghcr.io/braveantony/koupa`，節點會自己拉取，不用建置。用哪個版本寫在 [`images.env`](images.env)（v1、v2 各一個版本號，例如 `v1.2.0`、`v2.2.0`），講師發布後才會公告新版本；`setup.sh` 一開始會確認這兩個版本都已經在 ghcr 上，還沒發布就停下來，叢集不會被改動。版本號的規則見[設計文件 6.2](docs/design-v2/06-reference.md#62-映像檔)。

## 2. 部署

App 由哪些部分組成、部署後叢集裡有什麼、怎麼清除，見[Koupa 酷杯範例應用](docs/koupa-app.md)。

```bash
./setup.sh
```

`setup.sh` 分三段執行，任何一段失敗就停下來並印出原因：

1. 選第一個 worker 當 infra worker（貼上標籤 `koupa.dev/infra=true`），安裝 CNPG operator（在 Kubernetes 上管理 PostgreSQL 的程式，固定 v1.30.1），建立資料庫 `koupa-db`，兩者都固定在 infra worker 上；
2. 用 Kubernetes Job 執行 migration（照編號建立資料表、放入菜單）；
3. 部署 App、Service、Gateway 和 HTTPRoute（步驟 0 的設定），最後印出 Gateway 的 IP。

`setup.sh` 可以重跑：已經存在的資源會更新，migration Job 會先刪掉再重建。

部署完成後，在主機的瀏覽器打開三個頁面（`<IP>` 換成 `setup.sh` 最後印出的 IP）：

| 頁面 | 網址 |
|---|---|
| 點餐 | `http://<IP>/` |
| 廚房 | `http://<IP>/kitchen` |
| 叫號看板 | `http://<IP>/board` |

之後要再查 Gateway 的 IP：

```bash
kubectl -n koupa get gateway koupa -o jsonpath='{.status.addresses[0].value}'; echo
```

## 3. 量測

### 3.1 跑一次量測

先做[小實驗](#小實驗閒置的看板會不會斷)（它不用 `measure.sh`），再從 control 和步驟 0 開始量。每一步都只要執行 `./measure.sh step-N`：腳本會自己套用那一步的設定，不需要手動修改 YAML 或執行 `kubectl apply`。

```bash
./measure.sh control       # 對照組：不升級，必須全部是 0
./measure.sh step-0        # 步驟 0 的設定下升級 v1 → v2
./measure.sh step-1        # 依此類推，到 step-5
```

每個指令預設重複 5 次。每一次都會：

1. 套用這一步的 YAML，映像檔設成 v1，等 rollout（逐步把舊 Pod 換成新 Pod）完成；
2. 核對 Pod 真的帶著這一步的設定，不符合就不量；
3. Rush Hour 開始送單（60 秒），第 10 秒把映像檔換成 v2；
4. 收尾、對帳，印出成績單，存好證據包。

2026-11-10 實測，一次量測約 1.5 分鐘；步驟 0、1 收尾時有比較多「結果未知」的請求要等，一次約 3.5 分鐘。

常用參數：

| 參數 | 作用 |
|---|---|
| `--runs N` | 重複 N 次（預設 5） |
| `--verbose` | 成績單多印「診斷資訊」：每次失敗看得到的狀態和原因 |
| `--duration 秒數`、`--rate 每秒張數` | 調整送單時長（預設 60，至少 30）和速率（預設 20）。同一組比較要用相同的值 |

`control` 沒有通過（不是全部 0，或有「未知」）時，`measure.sh` 會以結束碼 3 停下來。這時不要比較數字；先看終端機的錯誤訊息，以及該次證據包的 `scorecard.txt` 和 `preflight.txt`，再請講師協助排查。

### 3.2 成績單

每次量測結束時都會印出成績單。以下是步驟 3 的一次量測結果（2026-11-10，`preStop` 8 秒）：

```
Rush Hour 成績單　量測編號 step-3-20261110-133434-r1-79fb
preStop：8 秒（已依本環境校正）

【主要成績】
訂單：1200 張
第一次送出沒有成功回應：0 張
重試後仍沒有成功回應：0 張
重複訂單（client_ref 數）：0
多出的訂單張數：0 張
大螢幕漏號（每台看板）：
  board-1：21
  board-2：22
  board-3：22
```

標題下一行是 `preStop` 有沒有經過校正（步驟 2 以後才有；這次量測時還沒有這一行，上面是現在的格式）。寫「preStop 未經校正」表示校正沒有成功，等待秒數是講師手冊的保守值，不是在這個環境量出來的；寫「未知」表示 Deployment 上沒有這個標記，請講師確認。

判斷不出來的結果不會算成 0，會寫成「已確認 N；另有 M 項未知」。5 次量完後，另外印一張彙整表（每一次的數字、中位數和範圍）。

### 3.3 證據包

每次量測存一份在 `evidence/<量測編號>/`（不進 git），5 次的彙整表在 `evidence/summary-<模式>-<時間>.txt`。先看這幾個檔案：

| 檔案 | 內容 |
|---|---|
| `scorecard.txt` | 成績單 |
| `timeline.txt` | 時間線：彙整可取得的紀錄，列出每個被關掉的 Pod 何時開始終止、何時收到 SIGTERM（Kubernetes 通知程式關機的訊號）、程式何時結束，以及入口最後一次把請求送給它的時間；缺少紀錄時會標示 |
| `failures.json` | 每一筆失敗或結果未知的嘗試 |
| `preflight.txt` | 量測前的設定核對結果 |
| `evidence-index.txt` | 證據包的檔案清單；缺少的部分標 `MISSING` |

其他檔案保留 App 和入口的原始紀錄、Pod 與可用後端清單（EndpointSlice）的變化、Rush Hour 每次嘗試的紀錄，以及套用的 Kubernetes 設定檔和版本資訊。需要深入排查時再查看。

## 4. 每一步改什麼、為什麼、你應該看到什麼

每一步都疊在前一步上面（Kustomize：在共用 YAML 上疊加差異的工具）。想看某一步的完整 YAML，或兩步之間差在哪裡：

```bash
kubectl kustomize k8s/steps/step-2
diff <(kubectl kustomize k8s/steps/step-1) <(kubectl kustomize k8s/steps/step-2)
```

下面「你應該看到」的數字都是 2026-11-10 在 tkha 上每個條件 5 次的中位數（括號內是範圍），只代表那個環境。

### 小實驗：閒置的看板會不會斷

**改什麼**：`k8s/idle-lab` 打開 `SSE_HEARTBEAT=15s`，App 每 15 秒對看板送一行心跳（瀏覽器會忽略的註解行）。

**為什麼**：看板連著但一直沒有叫號時，入口會因為「太久沒有資料」切斷連線，這和升級無關。先認識「長連線本身就會斷」，後面才不會把它和升級的問題混在一起。

**怎麼做**（不要同時跑 `measure.sh`，否則一直有叫號，連線不會閒置）：

```bash
GW=$(kubectl -n koupa get gateway koupa -o jsonpath='{.status.addresses[0].value}')
kubectl apply -k k8s/base && kubectl -n koupa rollout status deploy/koupa
curl -N --max-time 420 http://$GW/events; echo "curl 結束碼 $?"
```

收到 `hello` 之後就沒有任何資料。接著套用心跳設定，再做一次：

```bash
kubectl apply -k k8s/idle-lab && kubectl -n koupa rollout status deploy/koupa
curl -N --max-time 420 http://$GW/events; echo "curl 結束碼 $?"
```

查入口的紀錄（`response_flags` 是入口記下的結束原因）：

```bash
kubectl -n kube-system logs -l k8s-app=cilium-envoy --since=20m --tail=-1 --prefix | grep '"path":"/events"'
```

**你應該看到**：

- 沒有心跳：第 300 秒被切斷，curl 結束碼 18（回應不完整），入口紀錄 `response_flags` 是 `SI`（閒置逾時）；
- 有心跳：每 15 秒收到一行 `: hb`，420 秒都沒斷，最後是 curl 自己的 `--max-time` 到了（結束碼 28）。

做完後 `measure.sh` 會套用各步驟自己的設定，心跳會被拿掉。步驟 0～5 不開心跳，因為量測時每秒都有叫號，連線不會閒置。

### 步驟 0：什麼都不做

**改什麼**：`k8s/base`，Kubernetes 預設的 rolling update，沒有任何修正。

**為什麼**：先看問題有多嚴重，後面每一步才有比較的基準。

**你應該看到**：

- 第一次送出沒有成功回應：**51 張（8～53）**，重試後仍沒有成功回應 3 張（1～6）；
- 每台看板都有漏號（已確認 12～83 個，5 次中有 4 次另有未知）；
- 重複訂單：已確認 0，但每次另有 8～53 筆「伺服器端結果未知」。

數字每次差很多，原因在步驟 1。

### 步驟 1：新 Pod 準備好才接訂單

**改什麼**：`k8s/steps/step-1` 加上 `readinessProbe`（Kubernetes 判斷 Pod 能不能開始接收流量的檢查）：每 2 秒問一次 `/healthz`，HTTP 伺服器、資料庫和叫號通知都準備好才算 Ready。

**為什麼**：沒有它時，新 Pod 一啟動就被當成可以接收流量。tkha 上約 3 成的新 Pod 要晚 5 秒才開始 listen（開始在埠上接受連線；啟動時第一個 DNS 查詢被丟掉，要等 5 秒後重送），這段時間入口送過去的訂單全部失敗。步驟 0 的 5 次裡，這一類的中位數是 42 張；沒碰到慢 Pod 的那一次，總共只有 8 張。

**你應該看到**：

- 第一次送出沒有成功回應：**7 張（4～12）**。剩下的主要發生在**舊 Pod** 上：程式收到 SIGTERM 之後，入口還在把訂單送給它（中位數 5 張）；其餘是客人等太久先放棄（中位數 2 張）；
- 重試後仍沒有成功回應：0；
- 漏號沒有改善（每台已確認 25～164 個）。

### 步驟 2：被關掉前先等幾秒

**改什麼**：`k8s/steps/step-2` 加上 `preStop`（Pod 被關掉前先執行的動作）`sleep` 8 秒，程式晚 8 秒才收到 SIGTERM。

**為什麼**：Pod 開始終止時，入口需要一點時間才會收到更新，知道不該再把請求送給它。等 8 秒，讓入口先停止把新訂單送給它。8 秒是用 `measure.sh calibrate` 在 tkha 上量出來的：推估入口最晚在刪除後 2.127 秒就不再選它，取整數再加 5 秒緩衝。換了環境要重新量。

**你應該看到**：

- 第一次送出沒有成功回應：**0**；
- 漏號**沒有減少**：每台約 **80 個**（76～109）。看板還連在舊 Pod 上，8 秒後程式照樣直接結束，看板斷線、重連，斷線期間的叫號拿不到。

### 步驟 3：優雅關閉（graceful shutdown）

**改什麼**：`k8s/steps/step-3` 打開 `GRACEFUL_SHUTDOWN`，`SHUTDOWN_TIMEOUT` 15 秒，關機寬限時間（`terminationGracePeriodSeconds`，超過就強制結束）40 秒。

**為什麼**：程式收到 SIGTERM 後不再直接結束，而是開始等待處理中的請求收尾，同時通知每台看板「我要關了，請重新連線」（`shutdown` 事件）。超過 `SHUTDOWN_TIMEOUT` 就不保證全部完成。時間要符合 `preStop` 8 ＋ `SHUTDOWN_TIMEOUT` 15 ＋ 緩衝 5 ＜ 寬限 40，程式才不會收尾到一半被強制結束。

**你應該看到**：

- 送出失敗仍然是 0；
- 漏號從約 80 降到每台約 **22 個**（18～41）：看板收到通知後，依伺服器指定的間隔（預設 1 秒）重新連線，斷線時間變短，但重連後還是從「現在」開始收。

### 步驟 4：重連後補號

**改什麼**：`k8s/steps/step-4` 打開 `RESUME_FROM_LAST_ID`。

**為什麼**：瀏覽器的看板重連時，會自動帶上最後收到的叫號事件編號（`Last-Event-ID`，也就是 `ready_seq`，不是畫面上的號碼牌）。打開這個開關後，App 從那個編號之後補送叫號，斷線期間的叫號就不會漏掉。只有同一個頁面的自動重連才會帶這個編號；重新整理網頁是一次新的連線，會從「現在」開始收。

**你應該看到**：漏號 **0**（5 次、3 台看板都是 0）。

### 步驟 5：客人重送時，認出是同一張單

**改什麼**：`k8s/steps/step-5` 打開 `IDEMPOTENCY_KEY`。

**為什麼**：客人沒收到號碼牌就重送時，點餐頁會帶著同一個訂單代碼（`client_ref`）。打開開關後，App 用這個代碼認出是同一張單，回傳原本的號碼牌，不會多做一份。Kubernetes 的設定可以減少失敗，但不能保證客人一定收到回應，所以程式本身也要能安全地處理重送的請求。

**你應該看到**：成績單和步驟 4 一樣全部 0。這次預設負載的 25 次量測（步驟 0～4），已確認的重複訂單都是 0，其中步驟 0、1 仍有伺服器端結果未知。這一步的效果由講師用關鍵時機實驗示範：開關關閉時，同一次點餐變成 2 張訂單；打開後，重送拿回同一張號碼牌。

### 最後驗證：其他讓 Pod 被關掉的情況

步驟 5 做完後，在步驟 5 的設定下量三種情況，確認學到的是「Pod 被關掉」怎麼處理，不只是「升級」：

```bash
./measure.sh rollback      # 退版 v2 → v1
./measure.sh drain         # 讓一個 worker 暫停接收新 Pod，並移走上面的 Pod（infra worker 不會被選到）
./measure.sh scale-down    # Pod 從 3 個縮成 2 個
```

結束後會自動恢復：`drain` 會讓那個 worker 恢復接收新 Pod（uncordon），必要時刪掉擠在同一個 worker 上的 Pod 讓它重新排程，確認每個 worker 各一個 Pod；`scale-down` 會恢復成 3 個 Pod。2026-11-10 各量 5 次，15 次的成績單都是 0（當時 `preStop` 是 7 秒）。

## 預期結果

2026-11-10 在 tkha 上的實測（單一主機上的 3 個 control plane＋3 個 worker，都是 podman container；Kubernetes 1.37.2、Cilium 1.20.4）。control、步驟 0、1 是 `measure.sh all` 交錯量測的各 5 次；步驟 2～5 是 `preStop` 改成 8 秒後重跑、和 control 交錯的各 5 次。每次 1200 張訂單。

| 條件 | 第一次送出沒有成功回應 | 重試後仍沒有成功回應 | 漏號（每台看板） | 重複訂單 |
|---|---|---|---|---|
| control（不升級） | 0 | 0 | 0 | 0 |
| 步驟 0 | 51（8～53） | 3（1～6） | 已確認 12～83；4 次另有未知 | 已確認 0；另有未知 |
| 步驟 1 | 7（4～12） | 0 | 已確認 25～164；4 次另有未知 | 已確認 0；另有未知 |
| 步驟 2 | 0 | 0 | 約 80（76～109） | 0 |
| 步驟 3 | 0 | 0 | 約 22（18～41） | 0 |
| 步驟 4 | 0 | 0 | 0 | 0 |
| 步驟 5 | 0 | 0 | 0 | 0 |

你的數字不必和這張表一樣，要比的是**趨勢**：

- control 必須全部是 0 而且沒有未知，才適合比較；這次實測的步驟 0 每次都量到問題；
- 步驟 1 少掉「新 Pod 還沒準備好」那一類失敗；這次實測中，步驟 2 起沒有觀察到送出失敗；
- 漏號到步驟 2 都還在，步驟 3 變少；這次實測中，步驟 4 起沒有觀察到漏號；
- 這次預設負載下沒有確認到重複訂單，要看關鍵時機實驗。

不同環境的結果可能不同。

步驟 0 的數字特別不穩定，因為這取決於有沒有碰到晚 5 秒才開始 listen 的新 Pod。

**結論的邊界**：這些結果只代表單一主機、單一資料庫實例、[實測紀錄](docs/measurements/2026-11-10.md#1-環境與版本)列出的版本和設定。每個條件只量 5 次，「是 0」的意思是「這 5 次都沒觀察到」。

## 參考資料

- [設計文件](docs/design-v2/README.md)：問題從哪裡來、每一步的機制、量測方法
  - [每一步修正的機制](docs/design-v2/04-step-mechanisms.md)
  - [量測方法](docs/design-v2/05-measurement.md)
  - [環境需求、待實測項目的結果](docs/design-v2/06-reference.md)
- [實測紀錄 2026-11-10](docs/measurements/2026-11-10.md)：每一次的數字、證據包路徑
- [環境盤點](docs/env-findings.md)：入口的逾時和重試、新 Pod 晚 5 秒 listen 的調查
- [實作計畫](docs/plan.md)：正確性條件、必測案例
- [講師手冊](TEACHER.md)
