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

**一句話**：Pod 被關掉時，「通知入口別再送過來」和「通知程式關機」是同時進行的，彼此不會等對方完成。這個階段用 5 個步驟一一補上漏洞，並用量測數字確認每一步的效果。

## 一頁看懂

你會套用不同的 Kubernetes YAML；程式端的修正已經寫好，需要程式配合的步驟（3～5）會用環境變數打開。每一步都用 Rush Hour 量幾次「升級 v1 → v2」，看成績單上三個數字怎麼變：

- **第一次送出沒有成功回應**（下面簡稱「送出失敗」）：客人第一次按送出卻沒拿到號碼牌的訂單張數；
- **漏號**：每台叫號看板少收到幾個叫號（成績單上寫成「大螢幕漏號」）；
- **重複訂單**：同一次點餐在資料庫裡變成兩張以上。

| 步驟 | 改什麼 | 你應該看到（2026-11-10 實測，每個條件 5 次） |
|---|---|---|
| [小實驗](docs/steps/00-idle-lab.md) | 打開看板心跳（定期送出的短訊息） | 沒有心跳時，閒置的看板在第 300 秒被入口切斷；打開心跳就不會斷 |
| [0](docs/steps/step-0.md) | 什麼都不做 | 送出失敗中位數 51 張；每台看板漏號數十個 |
| [1](docs/steps/step-1.md) | `readinessProbe` | 送出失敗的中位數降到 7 張；漏號沒有改善 |
| [2](docs/steps/step-2.md) | `preStop` 等 8 秒 | 送出失敗變成 0；漏號仍然每台約 80 個 |
| [3](docs/steps/step-3.md) | 優雅關閉（graceful shutdown：關機前先收尾） | 漏號降到每台約 22 個 |
| [4](docs/steps/step-4.md) | 重連後補號 | 漏號變成 0 |
| [5](docs/steps/step-5.md) | 重送不重複下單 | 成績單和步驟 4 一樣（這次預設負載下沒有觀察到重複訂單）；效果由講師用關鍵時機實驗示範 |

成績單上的 0 只代表「這幾次沒觀察到」，不代表「不會發生」。數字只代表這個環境，詳見[預期結果](docs/steps/measure.md#預期結果)。

照順序做：[小實驗](docs/steps/00-idle-lab.md) → [量測說明（含 control，也就是不升級的對照組）](docs/steps/measure.md) → [步驟 0](docs/steps/step-0.md)～[5](docs/steps/step-5.md) → [最後驗證](docs/steps/final-checks.md)。每一步的文件都寫了要準備什麼、開哪些視窗、過程中看什麼、怎麼讀成績單。

---

## 1. 準備

**在哪裡執行指令**：主機，也就是叢集所在的那台電腦（叢集的節點是這台電腦上用 podman 跑的 container），而且連得到 Gateway IP。在主機開啟終端機，依序貼上並執行下面兩行：第一行下載課程檔案，第二行進入 `01/` 目錄。

```bash
git clone https://github.com/braveantony/koupa.git
cd koupa/01
```

下面的指令都在 `01/` 目錄執行。

**叢集**（課前由講師準備好）：

- 3 個 worker（執行 App 的叢集節點），koupa 的 3 個 Pod 會分散到每個 worker 各一個（最後驗證的 `drain` 量測會清空一個 worker，它要求每個 worker 剛好一個 Pod）；
- Cilium，並啟用 Gateway API、LoadBalancer IP 池和 L2 廣播（讓主機連得到 Gateway 分配到的 IP）；
- StorageClass `local-path`（把資料存在節點本機磁碟上；資料庫的設定 `k8s/db/cluster.yaml` 指定用它）；
- `kubectl` 目前使用的 context（決定 kubectl 操作哪個叢集的設定）指向這個叢集。

確認方式：

```bash
kubectl config current-context    # 名稱要是講師給你的叢集
kubectl get nodes                  # ROLES 欄是 <none> 的 3 個是 worker，STATUS 都要是 Ready
kubectl get storageclass local-path   # 有印出一行，沒有 NotFound
kubectl get crd gateways.gateway.networking.k8s.io   # 有印出一行，表示已安裝 Gateway API
```

**主機上的工具**：`kubectl`（內建 Kustomize）、`podman`、`skopeo`、`python3`（含 PyYAML）、`git`、`curl`。主機要連得到 `ghcr.io`、`docker.io`、`github.com` 和 `proxy.golang.org`（`measure.sh` 第一次執行時，會在 podman container 裡下載 Go 工具和模組）。不需要安裝 Go，因為要編譯的部分都在 podman container 裡跑。

**拉取 Rush Hour 的映像檔**（第一次量測前做一次；`measure.sh` 用主機上的這一份，不會自己拉取）：

```bash
podman pull ghcr.io/braveantony/rush-hour:0.1.0
```

只有修改 Rush Hour 程式的人才需要自己建置（`make images` 會連 App 映像檔一起建置）。

App 的映像檔是 ghcr 上公開的 `ghcr.io/braveantony/koupa`，節點會自己拉取，不用建置。用哪個版本寫在 [`images.env`](images.env)（v1、v2 各一個版本號，例如 `v1.2.0`、`v2.2.0`），講師發布後才會公告新版本；`setup.sh` 一開始會確認這兩個版本都已經在 ghcr 上，還沒發布就停下來，不會動到叢集。版本號的規則見[設計文件 6.2](docs/design-v2/06-reference.md#62-映像檔)。

## 2. 部署

App 由哪些部分組成、部署後叢集裡有什麼、怎麼清除，見[Koupa 酷杯範例應用](docs/koupa-app.md)。

```bash
./setup.sh
```

`setup.sh` 分三段執行，任何一段失敗就停下來並印出原因：

1. 選第一個 worker 當 infra worker（專門放資料庫的 worker；貼上標籤 `koupa.dev/infra=true`），安裝 CNPG operator（在 Kubernetes 上管理 PostgreSQL 的程式，版本固定為 v1.30.1），建立資料庫 `koupa-db`，兩者都只在 infra worker 上執行；
2. 用 Kubernetes Job 執行 migration（照編號建立資料表、放入菜單）；
3. 部署 App、Service、Gateway 和 HTTPRoute（步驟 0 的設定），最後印出 Gateway 的 IP。

`setup.sh` 可以重跑：已經存在的資源會更新，migration Job 會先刪掉再重建。

部署完成後，在主機的瀏覽器確認三個頁面都打得開（`<IP>` 換成 `setup.sh` 最後印出的 IP）：

| 頁面 | 網址 |
|---|---|
| 點餐 | `http://<IP>/` |
| 廚房 | `http://<IP>/kitchen` |
| 叫號看板 | `http://<IP>/board` |

之後要再查 Gateway 的 IP：

```bash
kubectl -n koupa get gateway koupa -o jsonpath='{.status.addresses[0].value}'; echo
```

量測時要開哪些視窗，寫在每一步文件的「開始之前」。

## 參考資料

- [每一步的操作說明](docs/steps/)：小實驗、量測說明、步驟 0～5、最後驗證
- [設計文件](docs/design-v2/README.md)：問題從哪裡來、每一步的機制、量測方法
  - [每一步修正的機制](docs/design-v2/04-step-mechanisms.md)
  - [量測方法](docs/design-v2/05-measurement.md)
  - [環境需求、待實測項目的結果](docs/design-v2/06-reference.md)
- [實測紀錄 2026-11-10](docs/measurements/2026-11-10.md)：每一次的數字、證據包路徑
- [環境盤點](docs/env-findings.md)：入口的逾時和重試、新 Pod 晚 5 秒才開始接受連線（listen）的調查
- [實作計畫](docs/plan.md)：正確性條件、必測案例
- [講師手冊](TEACHER.md)
