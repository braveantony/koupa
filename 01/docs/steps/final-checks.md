# 最後驗證：其他讓 Pod 被關掉的情況

## 這一步要解決什麼

確認學到的是「Pod 被關掉」怎麼處理，不只是「升級」：在步驟 5 的設定下，換三種方式讓 Pod 被關掉，成績單應該一樣是 0。

| 模式 | 第 10 秒做什麼 |
|---|---|
| `rollback` | 退版：先套用 v2，第 10 秒把映像檔改回 v1 |
| `drain` | 讓一個 worker 暫停接收新 Pod（cordon），並移走上面的 Pod：`kubectl drain <worker> --ignore-daemonsets` |
| `scale-down` | Pod 從 3 個縮成 2 個：`kubectl scale deploy/koupa --replicas=2` |

## 做之前

- 已經做完[步驟 5](step-5.md)。三種模式都用步驟 5 的設定，`measure.sh` 會自己套用。
- `drain` 量測前，3 個 koupa Pod 必須每個 worker 剛好一個；不符合時 `measure.sh` 會先刪掉擠在同一個 worker 上的 Pod 讓它重新排程，最多 5 次，還是不符合就停下來。要被清空的 worker 由 `scripts/drain-target.sh` 挑選，不會選 infra worker（放資料庫的那台）。

開好三個視窗（`<IP>` 的查法見 [README 2. 部署](../../README.md#2-部署)）：

| 視窗 | 內容 | 必要？ |
|---|---|---|
| 終端機 A | `./measure.sh rollback --runs 2`（依序換成 `drain`、`scale-down`） | 必要 |
| 終端機 B | `kubectl -n koupa get pods -w`；`drain` 時另外開 `kubectl get nodes -w` | 建議 |
| 瀏覽器（例如 desktop container 裡的 Firefox） | 只開 `http://<IP>/board?debug=1` | 選用，只看不操作 |

- 看板要在終端機 A 印出 `sending started` 之前打開。量測前 `measure.sh` 套用設定時 Pod 也會換一輪，看板先斷線再自動連上是正常的。
- 客人、廚房和 3 台看板都由 Rush Hour 模擬，瀏覽器只是讓你看。**量測期間只看看板，不要操作點餐頁和廚房頁**：Rush Hour 的店員會自己按「完成」；自己下的單不在 Rush Hour 的紀錄裡，不會算進成績單。

## 改了什麼

YAML 和步驟 5 一樣，沒有新的修改。差別只在第 10 秒的動作（見上表）。

## 怎麼做

在 `01/` 目錄依序執行：

```bash
./measure.sh rollback --runs 2
./measure.sh drain --runs 2
./measure.sh scale-down --runs 2
```

`--runs 2` 是課堂上的建議次數；不加時預設 5 次，講師的正式紀錄用 5 次。2026-11-10 三種各 5 次合計約 21 分鐘，各 2 次約 9 分鐘。

結束後會自動恢復，不用手動處理：`drain` 會讓那個 worker 恢復接收新 Pod（uncordon），必要時刪掉擠在同一個 worker 上的 Pod 讓它重新排程，確認每個 worker 各一個 Pod；`scale-down` 會恢復成 3 個 Pod。量測中途按 Ctrl-C，也會讓 worker 恢復接收新 Pod、把 Pod 數恢復成 3 個。

## 過程中看什麼

這三種在 2026-11-11 沒有逐秒觀察，下面是依 `measure.sh` 的動作和 2026-11-10 的紀錄整理的。

**終端機 A**：第 10 秒那一行分別是：

```
second 10: set image ghcr.io/braveantony/koupa:v1.2.2
second 10: drain <worker 名稱> --ignore-daemonsets
second 10: scale deploy/koupa --replicas=2
```

`drain` 開始前還會印出 `drain target: <worker 名稱>`；結束後印出 `cluster restored: …; 3 Pods available`。2026-11-10 的變更完成時間：`rollback` 第 12.9～21.7 秒、`drain` 第 18.3～19.3 秒、`scale-down` 第 10.7 秒。

**終端機 B**：

- `rollback`：和步驟 5 一樣，新 Pod（這次是 v1）先 `0/1` 再 `1/1`，舊 Pod 停在 `Terminating` 約 8 秒；
- `drain`：被清空的 worker 上那個 Pod 進入 `Terminating`。worker 暫停接收新 Pod 的期間，替代的 Pod 可能一直 `Pending`，到 uncordon 才排上去，所以這段時間可能只有 2 個 Pod；`kubectl get nodes -w` 會看到那個 worker 變成 `SchedulingDisabled`，結束後恢復；
- `scale-down`：只有 1 個 Pod 進入 `Terminating`，沒有新 Pod。

**瀏覽器的看板**：連著的 Pod 被關掉時，一樣先斷線再約 1 秒後重連；`rollback` 時資訊列的服務版本會從 v2 變回 v1。看板連著的 Pod 沒被關掉時，就不會斷線。

## 你應該看到

講師的結果（2026-11-10，各 5 次，當時 `preStop` 是 7 秒，改成 8 秒之後沒有重跑）：15 次的成績單都是 0。

| 模式 | 送出失敗 | 重試後仍沒有成功回應 | 重複訂單 | 漏號 |
|---|---|---|---|---|
| `rollback` | 0 | 0 | 0 | 0 |
| `drain` | 0 | 0 | 0 | 0 |
| `scale-down` | 0 | 0 | 0 | 0 |

你的每一次都是 0，就算吻合。`drain` 的成績單最後會多一段註記，說明 drain 期間可能只有 2 個 Pod。

## 為什麼會這樣

這三種情況和升級一樣，最後都是「刪除 Pod」：Kubernetes 一樣先通知入口、執行 `preStop`，再送 SIGTERM。步驟 1～5 補的空隙和觸發的原因無關，所以同樣有效。

完整的實測紀錄見[實測紀錄第 9 節](../measurements/2026-11-10.md#9-rollbackdrainscale-down)。

## 出問題時

- **`drain` 停下來，訊息是「沒有符合 A15 條件的 worker」**：每個 worker 都不能被安全清空，例如上面有資料庫的 Pod，或有用 emptyDir（Pod 的暫存目錄）的其他 Pod。`measure.sh` 不會加 `--delete-emptydir-data`，以免刪掉資料。看證據包的 `drain-target.json` 裡每個 worker 被排除的原因。
- **`drain` 停下來，訊息是「drain 前 koupa Pod 不是每個 worker 剛好一個」**：重新平衡 5 次後還是不符合。用 `kubectl -n koupa get pods -o wide` 看 Pod 在哪個 worker，請講師確認。
- **終端機 A 印出「cluster not restored」或「停止：叢集沒有恢復」**：結束碼 1，自動恢復失敗。照訊息手動恢復，例如 `kubectl uncordon <worker 名稱>` 或 `kubectl -n koupa scale deploy/koupa --replicas=3`，再用 `kubectl get nodes` 和 `kubectl -n koupa get pods -o wide` 確認。細節在證據包的 `restore.json`。
- **成績單不是 0**：加上 `--verbose` 看診斷資訊，並把證據包交給講師。

查看上一個指令的結束碼：`echo $?`。其他狀況見[量測說明的結束碼](measure.md#結束碼和常見錯誤)。

---

[← 步驟 5](step-5.md)｜[回 README](../../README.md)
