# 環境盤點（Task 1）

調查日期：2026-11-09。這份文件只記錄事實：每一項都附指令與輸出，或附來源網址（含 tag 或 commit）。查不到的標「待確認」。

## 0. 這次調查的限制（先讀這段）

調查當下，**tk8s 叢集不存在**，所以所有「要連到即時叢集才能看到」的項目（即時 config dump、`/stats`、實際跑出來的版本字串）都沒有做，改成從原始碼與 tk8s repo 內固定的版本檔推出，並標明「依原始碼，待 koupa Gateway 建立後確認」。

```console
$ kubectl get nodes
Unable to connect to the server: dial tcp 172.22.0.253:6443: connect: no route to host

$ tkctl get clusters
no clusters

$ podman ps -a --format '{{.Names}} {{.Status}}'      # 沒有任何節點容器
bench-grafana Exited (0) 2 weeks ago
...（其餘都是無關容器；只有 koupa-test-db 在跑）

$ kubectl config get-contexts
CURRENT   NAME                    CLUSTER   AUTHINFO           NAMESPACE
*         kubernetes-admin@tk8s   tk8s      kubernetes-admin
```

- `~/.kube/config` 還留著 `tk8s` 的 context（API server `https://172.22.0.253:6443`），但這個位址已經不通，`tkctl` 也說沒有任何叢集。
- 本任務規定不動叢集，所以沒有重建。**要補做即時驗證時**，先 `tkctl create cluster`（約 3 分鐘），建好 koupa Gateway 之後照第 7 節「待確認清單」逐項確認。
- 任務說明寫的 Kubernetes v1.37.1 是 `kubectl` 用戶端版本（見 1.1）；叢集端版本無法查證。tk8s 目前預設是 1.37.2（見 1.2）。

## 1. 版本（步驟 1）

### 1.1 主機

```console
$ kubectl version
Client Version: v1.37.1
Kustomize Version: v5.8.1

$ podman version | head -3
Client:        Podman Engine
Version:       5.7.0
API Version:   5.7.0

$ cilium version --client
cilium-cli: v0.19.7 compiled with go1.26.5 on linux/amd64
cilium image (default): v1.19.5
cilium image (stable): v1.20.4

$ tkctl version
tkctl v2026.10.13-2-g94250ef
kubernetes default 1.37.2
```

主機上的 `cilium` 是 cilium-cli v0.19.7，比 tk8s 固定的 `cilium_cli: v0.20.2` 舊。它只是用戶端，不影響叢集內的 Cilium。

### 1.2 tk8s 固定的元件版本

來源：`/home/bigred/tk8s/versions.yaml`（tk8s repo commit `94250ef04caaefcd3484d53d636a829b708e9171`）。

| 元件 | 版本 | 備註 |
|---|---|---|
| Kubernetes（`tkctl` 預設） | 1.37.2 | 叢集實際版本待確認（叢集不存在） |
| CRI-O | v1.37.2（對應 Kubernetes 1.37） | `crio."1.37"` |
| Cilium | 1.20.4 | `cilium: "1.20.4"`；chart 在 `/home/bigred/tk8s/manifests/cilium-1.20.4` |
| Gateway API CRD | v1.6.3，experimental channel | `/home/bigred/tk8s/manifests/gateway-api.1.6.3.yaml` 標頭寫明 experimental；其中 HTTPRoute 有 `retry` 欄位 |
| 主機 podman | 5.7.0 | 需求 >= 5.4 |

Cilium 開 Gateway API 的安裝參數（`/home/bigred/tk8s/src/tk8s/create/cni.py`）：

```text
kubeProxyReplacement=true
gatewayAPI.enabled=true
gatewayAPI.externalTrafficPolicy=Local
```

沒有任何 `hubble.*`、`envoy.*` 參數，所以這兩組都用 chart 預設（見第 4 節）。

### 1.3 Cilium 內建的 Envoy

來源：`cilium/cilium` tag `v1.20.4`（tag 物件 `6660d6eac7f93a2176ac039f03e7f2bf885bdb2e`，commit `beb276d2f38d4aadfda14710acf8d14a0f1ba723`），`images/cilium/Dockerfile` 第 11 行：

```text
ARG CILIUM_ENVOY_IMAGE=quay.io/cilium/cilium-envoy:v1.39.5-1792733054-8c73b2816bc8239e27f1fd53b271884688137459@sha256:547ab45f...
```

- Envoy 版本是 **1.39.5**（cilium/proxy commit `8c73b2816bc8239e27f1fd53b271884688137459` 的 `ENVOY_VERSION` 檔內容為 `envoy-1.39.5`）。
- Envoy 以獨立 DaemonSet `cilium-envoy` 執行（chart `envoy.enabled` 註解：「This field is enabled by default for new installation」）。實際有沒有這個 DaemonSet，待確認（叢集不存在）。
- 本文下面引用的 Envoy 文件與原始碼，一律用 tag `v1.39.5`。

## 2. Envoy 的取得方式（步驟 2）

### 2.1 admin 介面

| 項目 | 結論 | 證據 |
|---|---|---|
| admin 位置 | 可以：unix socket `/var/run/cilium/envoy/sockets/admin.sock`，不是 TCP port | chart `install/kubernetes/cilium/files/cilium-envoy/configmap/bootstrap-config.yaml` 第 366 行 `admin: address: pipe: path: "/var/run/cilium/envoy/sockets/admin.sock"`、`mode: 0660` |
| 從哪裡連 | 可以：socket 所在的 hostPath 同時掛進 `cilium-envoy` 和 `cilium-agent` Pod | `templates/cilium-envoy/daemonset.yaml` 第 241、297 行（volume `envoy-sockets`）、`templates/cilium-agent/daemonset.yaml` 第 318 行 |
| config dump、clusters、metrics | 可以：agent 內建 `cilium-dbg envoy admin config / clusters / listeners / metrics / serverinfo / logging`（`cilium-dbg/cmd/envoy_admin*.go`）。範例：`kubectl -n kube-system exec ds/cilium -c cilium-agent -- cilium-dbg envoy admin config`；實際可執行待確認 | 同左 |
| 直接打 admin API | 可以（待確認）：Cilium 自己的 client 就是對 socket 發 HTTP GET（`cilium-dbg/cmd/envoy_adminclient.go`，`:authority` 為 `envoy-admin`）。容器內有沒有 `curl --unix-socket` 待確認 | 同左 |
| 另開 admin 的 TCP port | 預設沒有；要開 `envoy.debug.admin.enabled`（`cilium-configmap.yaml` 第 243 行 `proxy-admin-port`）。本任務不改 Cilium 設定 | 同左 |

要點：Cilium Gateway 的 Envoy 是**每個節點一個**（DaemonSet），不是每個 Gateway 一個。量測時要對**每一個節點**的 `cilium-envoy` 都收 log 與統計，否則 A11 要求的「涵蓋所有入口實例」就不成立。

### 2.2 access log

| 項目 | 結論 | 證據 |
|---|---|---|
| 能不能開 | **可以**，不用改 Cilium 的 Helm 設定：建一個 `CiliumGatewayClassConfig`，並讓 `GatewayClass` 用 `spec.parametersRef` 指向它 | `pkg/k8s/apis/cilium.io/v2alpha1/gatewayclassconfig_types.go` 第 157～215 行（`Telemetry.AccessLogs`）；`Documentation/network/servicemesh/gateway-api/access-logs.rst`（v1.20.4） |
| 輸出到哪 | Envoy 的 stdout，也就是 `cilium-envoy` Pod 的 container log（`kubectl logs -n kube-system ds/cilium-envoy`，每個 Pod 各一條） | 同上：「Cilium configures Envoy to write access logs to stdout」；`operator/pkg/model/translation/helper.go` 的 `buildAccessLogs` 用 `envoy.access_loggers.stdout` |
| cilium-envoy 有沒有編進 stdout logger | 有 | `cilium/proxy` commit `8c73b281...` 的 `envoy_build_config/extensions_build_config.bzl` 第 16 行 `"envoy.access_loggers.stdout"` |
| 格式 | 可以：`format: Text` 配 `text`（Envoy 格式字串），或 `format: JSON` 配 `json`（欄位名稱對應 Envoy 格式運算子）。`targets` 預設只有 `HTTP` | 同 types.go |
| 副作用 | 這個設定是 **GatewayClass 層級**：套用在所有引用它的 Gateway，也會改動 `GatewayClass` 物件的 `parametersRef`（量測時要用專屬的 GatewayClass，或在課程裡明講要改它）。這個動作不在本任務範圍，沒有執行 | `access-logs.rst`：「any settings you define here will automatically apply to all Gateways using that specific Gateway Class」 |

### 2.3 能不能自訂欄位（Envoy 1.39.5）

來源：Envoy 文件 `docs/root/configuration/advanced/substitution_formatter.rst`（tag `v1.39.5`）；Cilium 文件明講格式運算子以「你實際跑的 Envoy 版本」的文件為準（types.go 第 181～183 行）。

| 欄位 | 結論 | 證據與注意事項 |
|---|---|---|
| `%UPSTREAM_REQUEST_ATTEMPT_COUNT%` | **可以**（依文件與原始碼；實際輸出待確認） | 文件第 213 行：「Number of times the request is attempted upstream. An attempt count of `0` means that the request was never attempted upstream.」**但**原始碼 `source/common/formatter/stream_info_formatter.cc` 第 1686 行是 `stream_info.attemptCount().value_or(0)`：欄位沒設定時也印 `0`，不會印 `-`。所以 A11 說的三種情況（明確 0、依格式規格省略、沒設定或收集失敗）在 HTTP access log 裡分不出來：只要格式裡有這個欄位，輸出一定有數字。`0` 的意思是「沒有往上游送過」（含 router 之前就回應的情況，例如找不到路由）。「欄位沒設定」要靠核對 log 格式本身，不能靠值判斷 |
| `%RESPONSE_FLAGS%` | **可以** | 文件第 489 行起。A11 列的旗標都在表內：`UH`（NoHealthyUpstream）、`UF`（UpstreamConnectionFailure）、`UC`（UpstreamConnectionTermination）、`UT`（UpstreamRequestTimeout）、`URX`（UpstreamRetryLimitExceeded）、`DC`（DownstreamConnectionTermination）；另有 `UR`（UpstreamRemoteReset）、`SI`（StreamIdleTimeout）、`UMSDR`（UpstreamMaxStreamDurationReached）、`DT`、`LR` 等 |
| `%RESPONSE_CODE_DETAILS%` | **可以** | 文件第 173 行 `%RESPONSE_CODE_DETAILS(X)%`，HTTP 才有，TCP 印 `-` |
| `%REQ(X-KOUPA-ATTEMPT)%` | **可以** | 文件第 939 行 `%REQUEST_HEADER(X?Y):Z%` / `%REQ(X?Y):Z%`；標頭不存在時印 `-` |
| 其他 A11 要的欄位 | **可以**：`%START_TIME%`、`%REQUEST_HEADER(X-ENVOY-ORIGINAL-PATH?:PATH)%`、`%RESPONSE_CODE%`、`%UPSTREAM_HOST%`、`%DURATION%` | Cilium 預設 JSON 範本本來就用到（types.go 第 186 行 `+kubebuilder:default`） |

自訂格式的 CRD 欄位上限：JSON 最多 64 個欄位、`text` 最長 4096 字元、`accessLogs` 最多 8 組。

### 2.4 這一節待確認（等叢集和 koupa Gateway 建好）

1. `CiliumGatewayClassConfig` CRD 在叢集裡確實有 `spec.telemetry`：`kubectl get crd ciliumgatewayclassconfigs.cilium.io -o yaml`。
2. 設定後 `kubectl logs -n kube-system -l k8s-app=cilium-envoy` 真的有輸出，且四個欄位都有值。
3. `X-Koupa-Attempt` 標頭穿過 Gateway 不被改寫（預期不會）。
4. chart 預設 `envoy.log.accessLogEnabled: true` 是給 Hubble 用的 access log 轉送，和上面的 stdout 設定是兩回事，不要混在一起。

## 3. 重試與逾時（步驟 3）

來源：`cilium/cilium` tag `v1.20.4`（commit `beb276d2...`）`operator/pkg/model/translation/` 與 `operator/pkg/model/ingestion/gateway.go`。**以下全部「依原始碼，待 koupa Gateway 建立後用 config dump 確認」。**

### 3.1 重試

| 項目 | 結論 | 證據 |
|---|---|---|
| 預設會不會重試 | **不會**。只有 HTTPRoute 規則有寫 `retry` 才會產生 `RetryPolicy`；沒寫就沒有重試政策 | `envoy_virtual_host.go` 第 471～497 行 `retryMutation`：`if retry == nil { return route }`。`ingestion/gateway.go` 第 522、545 行 `Retry: toHTTPRetry(rule.Retry)`，來源只有 `rule.Retry`（HTTPRoute 的 `retry` 欄位） |
| 能不能關 | **可以**：HTTPRoute 不寫 `retry` 就是關，不需要改 Cilium 設定 | 同上。`operator/` 與 `pkg/envoy` 內，Gateway 路徑只有 `envoy_virtual_host.go` 一處會設 `RetryPolicy`（`pkg/envoy/model.go` 第 1030 行那組 `retryOn: 5xx` 屬於 L7 policy 的 listener，不是 Gateway 路由） |
| 有寫 `retry` 時的行為 | `retry_on = "retriable-status-codes,connect-failure,reset,refused-stream"`；次數用 `attempts`，沒寫 `attempts` 就是 Envoy 預設（1 次重試，即最多 2 次上游嘗試；這是 Envoy 的預設值，待用 config dump 確認）。程式碼註解明寫「applies to all HTTP methods, including non-idempotent ones」（POST 也會重試） | `envoy_virtual_host.go` 第 462～469 行 |
| 對 A11 的意義 | A11 說的「關閉入口重試，上游嘗試次數最多 1 次」**可做**：koupa 的 HTTPRoute 不要寫 `retry` | — |

另一個干擾因素：每個 cluster 都有 `outlierDetection: splitExternalLocalOriginErrors: true`（`envoy_cluster.go` 第 36 行 `withOutlierDetection(true)`；golden 檔 `operator/pkg/model/translation/gateway-api/testdata/basic_http_listener/cec-output.yaml`）。Envoy 的 outlier detection 在端點連續失敗時會暫時把它踢出負載平衡，這會讓「入口停止選某個 Pod」多一個和 EndpointSlice 無關的原因。實際效果待確認，calibrate（A11）的推論要把它列為干擾因素。

### 3.2 逾時

Cilium 1.20.4 對 Gateway 產生的設定（以上述 golden 檔為準）：

| 逾時 | 預設值 | 在哪設 | 能不能改 | 證據 |
|---|---|---|---|---|
| route timeout | **沒設定**，所以落到 Envoy 預設 **15 秒** | `envoy_virtual_host.go` 第 501～518 行 `timeoutMutation`：`backend`、`request` 都沒寫時不設 `route.timeout` | 可以：HTTPRoute 的 `rules[].timeouts.request` / `backendRequest`（取兩者較小）；寫 `0s` 即停用（Cilium 轉成 `timeout: 0s`；Envoy：0 表示停用） | Envoy `source/common/router/config_impl.h` 第 976 行 `DEFAULT_ROUTE_TIMEOUT_MS = 15000`；`api/envoy/config/route/v3/route_components.proto` 第 1373～1376 行「If not specified, the default is 15s … A value of 0 will disable the route's timeout」 |
| max stream duration | route 與 HCM 都明確設成 `0s`（不限制） | `envoy_virtual_host.go` 第 504 行；`envoy_http_connection_manager.go` 第 148 行 | 無 | golden 檔 `maxStreamDuration: 0s` |
| stream idle timeout | **300 秒** | HCM `streamIdleTimeout`；Helm `envoy.streamIdleTimeoutDurationSeconds`，operator 旗標 `--proxy-stream-idle-timeout-seconds` | 可以改 Helm（本任務不改） | `operator/pkg/ciliumenvoyconfig/cell.go` 第 39～42 行；values.yaml 第 2815 行；golden 檔 `streamIdleTimeout: 300s` |
| 上游連線 idle timeout | **60 秒**（cluster 的 `commonHttpProtocolOptions.idleTimeout`） | Helm `envoy.idleTimeoutDurationSeconds` | 同上 | values.yaml 第 2812 行；golden 檔 `idleTimeout: 60s` |
| 連線上游逾時 | Helm `envoy.connectTimeoutSeconds: 2` | — | — | values.yaml 第 2791 行；Gateway 產生的 cluster 有沒有沿用這個值待確認 |
| 下游 TCP keepalive | idle 10 秒、間隔 5 秒、最多 10 次 | listener `socketOptions` | — | golden 檔 |

對 A11／4.3 的意義（**待用即時 config dump 確認**）：

- `/events`（SSE）如果 HTTPRoute 沒寫 `timeouts`，route timeout 會是 15 秒，長連線約 15 秒就被入口切斷，不是 stream idle 的 300 秒。4.3 的「入口切斷長連線是哪一種逾時」，原始碼給的最可能答案是 route timeout 15 秒；回應旗標會是什麼待確認。要讓 SSE 活著，koupa 的 HTTPRoute 對該規則寫 `timeouts: {request: 0s}`；此時 stream idle 300 秒仍然存在，所以伺服器要在 300 秒內送出資料（心跳）。
- config dump 裡的欄位名稱：route 的 `timeout`、`max_stream_duration`；HCM 的 `stream_idle_timeout`；cluster 的 `typed_extension_protocol_options` 下的 `common_http_protocol_options.idle_timeout`。

### 3.3 待確認

1. 以上每一項用即時 `cilium-dbg envoy admin config` 的輸出核對（尤其 route timeout 沒設時實際生效值，以及 Cilium agent 在 xDS 推送時是否補上 `connect_timeout`）。
2. Cilium 對 terminating 端點在 Envoy EDS 裡的處理（calibrate 的上界 U 要靠它）：原始碼尚未追，**待確認**。

## 4. Hubble（步驟 4）

| 項目 | 結論 | 證據 |
|---|---|---|
| 是否啟用 | **預設啟用**（依 chart 預設，叢集不存在無法直接看）：agent 內建 Hubble server，socket `/var/run/cilium/hubble.sock` | chart `values.yaml` 第 1448～1450 行 `hubble: enabled: true`、第 1599 行 `socketPath`；`cilium-configmap.yaml` 第 1054 行 `enable-hubble`；tk8s 的 `cni.py` 沒有覆寫任何 `hubble.*` |
| Hubble Relay、UI | **預設關閉** | values.yaml 第 1761～1763 行 `relay.enabled: false`、第 2004～2006 行 `ui.enabled: false`；tk8s 沒開 |
| 主機上的 `hubble` CLI | **沒有** | `which hubble` 沒有輸出 |
| `hubble observe` 在主機上能不能跑 | **不能直接跑**（沒有 CLI、也沒有 Relay）。**降級做法（本身也待確認）**：在 agent Pod 內執行 `kubectl -n kube-system exec ds/cilium -c cilium-agent -- hubble observe -o json`。旁證：[`images/README.md`](https://github.com/cilium/cilium/blob/v1.20.4/images/README.md) 寫 cilium 映像檔含 `hubble-cli`，[`images/cilium/Dockerfile` 第 105 行](https://github.com/cilium/cilium/blob/v1.20.4/images/cilium/Dockerfile#L105)設了 `HUBBLE_SERVER=unix:///var/run/cilium/hubble.sock`。但 Dockerfile 沒有直接 COPY `hubble` 執行檔（是從 builder 階段整包複製），**執行檔是否真的在 `/usr/bin/hubble` 沒有驗證，待確認**。做法有效的話，也只看得到**該節點**的流量，多節點要每個 agent Pod 各一條 | 同上 |
| 能看到 Gateway 的 L7 嗎 | 待確認：`envoy.log.accessLogEnabled` 預設 true（Envoy access log 轉送給 Hubble），但欄位是否足夠做 A11 的對帳待確認 | values.yaml `envoy.log.accessLogEnabled` |

結論：A11 流量那一列「`hubble observe -n koupa -o json`」在主機上**做不到**；改成「在每個 `cilium` agent Pod 內執行 `hubble observe`」是**待確認**的降級做法（執行檔存在與否未驗證），且 L7 內容待確認。Hubble 在本課程是輔助來源，不是 A14 對帳的依據。

## 5. CNPG 與 PostgreSQL 17（步驟 5）

### 5.1 CNPG operator

以下指令用 python 取代 `jq` 格式化（API 回應內容未改，只取 `tag_name`、`published_at`、`prerelease`、`draft` 四欄，以 tab 分隔，2026-11-09 執行；可重現的指令是第一行的 URL）：

```console
$ curl -s "https://api.github.com/repos/cloudnative-pg/cloudnative-pg/releases?per_page=8" | python3 -I -c "import json,sys; [print('\t'.join(map(str,[r['tag_name'],r['published_at'],r['prerelease'],r['draft']]))) for r in json.load(sys.stdin)]"
v1.30.1	2026-09-23T13:35:42Z	False	False
v1.29.3	2026-09-23T13:00:24Z	False	False
v1.30.0	2026-06-29T16:36:13Z	False	False
v1.29.2	2026-06-29T16:06:03Z	False	False
v1.28.4	2026-06-29T15:38:44Z	False	False
v1.30.0-rc1	2026-06-19T14:46:47Z	False	False
v1.29.1	2026-05-08T14:33:15Z	False	False
v1.28.3	2026-05-08T14:06:59Z	False	False
```

- **選定：CloudNativePG v1.30.1**（最新穩定版，非 rc）。安裝清單：`https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v1.30.1/cnpg-1.30.1.yaml`（內含 `ghcr.io/cloudnative-pg/cloudnative-pg:1.30.1`）。
- 支援矩陣（`docs/src/supported_releases.md`，tag `v1.30.1`）：1.30.x 支援 Kubernetes 1.34、1.35、1.36；**1.37 列在「Tested, but not supported」**；支援 PostgreSQL 14～18。tk8s 預設是 Kubernetes 1.37.2，所以 CNPG 在這個叢集上是「測過、但不在官方支援清單」。這點要寫進 6.7 的前提欄。
- operator 預設的 PostgreSQL 映像檔是 `ghcr.io/cloudnative-pg/postgresql:18.6-system-trixie`（`pkg/versions/versions.go`，tag `v1.30.1`），所以 Cluster 一定要明確指定 `imageName`，不能用預設。

### 5.2 PostgreSQL 17 映像檔

```console
$ podman pull ghcr.io/cloudnative-pg/postgresql:17.11-202611020810-system-trixie
$ podman image inspect ghcr.io/cloudnative-pg/postgresql:17.11-202611020810-system-trixie --format '{{.Digest}} {{.Architecture}} {{.Os}} {{.Created}}'
sha256:314285da12534860b0c7693a4a9b896212c765c80c6b13ba82f29ba39212a58d amd64 linux 2026-11-02 08:10:53.809420933 +0000 UTC
$ podman run --rm --network none --entrypoint postgres ghcr.io/cloudnative-pg/postgresql:17.11-202611020810-system-trixie --version
postgres (PostgreSQL) 17.11 (Debian 17.11-1.pgdg13+2)
$ TOK=$(curl -sS "https://ghcr.io/token?scope=repository:cloudnative-pg/postgresql:pull" | python3 -I -c "import json,sys;print(json.load(sys.stdin)['token'])")
$ curl -sI -H "Authorization: Bearer $TOK" -H "Accept: application/vnd.oci.image.index.v1+json" https://ghcr.io/v2/cloudnative-pg/postgresql/manifests/17.11-202611020810-system-trixie | grep -i docker-content-digest
docker-content-digest: sha256:03a1c6a62b27e8ad31cae5c085131b5ac706b09403d6fcb6c58d16c8daa257cd
```

（`podman run` 不加 `--network none` 時，這台主機的 pasta 網路會出錯，與映像檔無關。）

- **選定映像檔**：`ghcr.io/cloudnative-pg/postgresql:17.11-202611020810-system-trixie`。
- **Digest（多架構 index）**：`sha256:03a1c6a62b27e8ad31cae5c085131b5ac706b09403d6fcb6c58d16c8daa257cd`。
- `linux/amd64` 單一平台 manifest digest：`sha256:314285da12534860b0c7693a4a9b896212c765c80c6b13ba82f29ba39212a58d`。
- 理由：17.x 的最新小版本（17.11）；`system` 風味和 operator 預設一致（`system-trixie`）；帶時間戳的 tag 加上 digest 固定。`imageName` 是否接受 `tag@digest` 寫法，待實作時對 CNPG 確認；不行就只用 digest。
- `transaction_timeout` 需要 PostgreSQL 17 以上；映像檔內是 17.11，滿足。實際可設定待實作時用 `SHOW transaction_timeout` 確認。

## 6. 結論表（步驟 6）

標記：**可做**、**降級做法**、**做不到（判定一律未知）**。所有依原始碼的項目，都要在 koupa Gateway 建立後用即時 config dump 與 log 確認。

### 表 A：A11 入口紀錄欄位

| 欄位或項目 | 結論 | 做法或原因 |
|---|---|---|
| 開始時間 | 可做 | `%START_TIME%` |
| 路徑 | 可做 | `%REQUEST_HEADER(X-ENVOY-ORIGINAL-PATH?:PATH)%` |
| HTTP 狀態 | 可做 | `%RESPONSE_CODE%` |
| 上游位址 | 可做 | `%UPSTREAM_HOST%`；上游嘗試次數大於 1 時只代表最後一次 |
| 回應旗標 | 可做 | `%RESPONSE_FLAGS%`（`UH/UF/UC/UT/URX/DC` 都在 Envoy 1.39.5 的表內） |
| 回應細節 | 可做 | `%RESPONSE_CODE_DETAILS%` |
| 上游嘗試次數 | 可做，但三種情況只能分兩種 | `%UPSTREAM_REQUEST_ATTEMPT_COUNT%`；原始碼是 `value_or(0)`，「明確 0」和「沒設定」都印 0。「依格式規格省略」不會發生（HTTP access log 欄位一律有值）。「欄位沒設定／收集失敗」靠核對 log 格式與每個節點的 `cilium-envoy` log 是否完整判斷 |
| 總耗時 | 可做 | `%DURATION%` |
| `X-Koupa-Attempt` | 可做 | `%REQ(X-KOUPA-ATTEMPT)%` |
| 入口重試 | 預設不會重試，可保持關閉 | 只有 HTTPRoute 有 `retry` 才會；koupa 路由不寫 `retry`，上游嘗試次數最多 1 次 |
| 每次上游嘗試的紀錄 | 做不到 | access log 每個下游請求只有一行；Cilium Gateway 沒有逐次嘗試的設定。在不寫 `retry` 時嘗試次數不會大於 1，這個缺口不影響；若有人打開 `retry`，嘗試次數大於 1 的請求「入口選了哪個 Pod」判定一律未知 |
| 統計計數器 | 可做（名稱待確認） | admin 的 `/stats`（`cilium-dbg envoy admin metrics`）。Envoy 1.39.5 文件（`configuration/upstream/cluster_manager/cluster_stats.rst`）有：`upstream_rq_total`、`upstream_rq_retry`、`upstream_rq_timeout`、`upstream_rq_rx_reset`、`upstream_rq_pending_failure_eject`、`upstream_cx_destroy_remote_with_active_rq`（上游連線帶著進行中請求被遠端關閉）；5xx 數用 `upstream_rq_5xx`（動態名稱，待確認）。cluster 名稱依 `getClusterName` 是 `<ns>:<service>:<port>`，加上 CEC 前綴後的實際名稱待確認 |
| 涵蓋所有入口實例 | 降級做法 | 每個節點一個 `cilium-envoy`，要對全部節點同時收 log 與統計；少一個，該節點相關的判定標未知 |

### 表 B：A11 其他來源

| 來源 | 結論 | 備註 |
|---|---|---|
| Envoy 端點／設定更新紀錄（calibrate 的直接證據上界 U） | 待確認，傾向做不到 | 需要 Envoy 對 EDS 更新的紀錄（debug log 或 agent 後端列表快照）。沒有的話，A11 已規定退回「模型推估」，並標成估計 |
| 干擾因素 | 要記錄 | outlier detection 預設開（3.1 末段） |
| Hubble | 降級做法 | 第 4 節；在每個 agent Pod 內執行，輔助用途 |
| API server audit log（交叉核對刪除請求時間） | 待確認 | tk8s 有 `manifests/audit-policy.yaml`；實際有無啟用與 log 位置，叢集不存在無法查證 |

### 表 C：A14 「和入口的紀錄對帳」三方

三方：Rush Hour 的嘗試紀錄、入口 access log、Envoy 統計計數器。

| 對帳項目 | 結論 | 原因 |
|---|---|---|
| Rush Hour 對 access log，依路徑與 `X-Koupa-Attempt` 逐筆 | 可做 | 兩邊都有路徑與嘗試編號；限 `/orders` 最終回應 |
| access log 對 Envoy 統計 | 降級做法 | 計數器是整個 cluster 的、沒有路徑維度；用總量比較，非 `/orders` 流量、入口自己產生的回應（`NR`、`UH` 等沒經過上游）、逾時另列 |
| Rush Hour 對統計 | 降級做法 | 同上，只能比總量 |
| 差額分類中的「入口內部重試」 | 可做 | 不寫 `retry` 時 `upstream_rq_retry` 的差值應為 0，可當自我檢查 |
| 沒有涵蓋全部入口實例時 | 做不到（判定一律未知） | 見表 A 最後一列 |

### 表 D：建議修正 A11／A14 的文字

1. A11「入口預設會不會重試」：改為「預設不會；HTTPRoute 不寫 `retry` 就是關閉（依 Cilium v1.20.4 原始碼，待用 config dump 確認）」。
2. A11「上游嘗試次數要分清楚三種情況」：改為「HTTP access log 的 `%UPSTREAM_REQUEST_ATTEMPT_COUNT%` 在沒設定時也印 0（Envoy 1.39.5 原始碼 `value_or(0)`），所以只區分『印出數字』與『整行缺漏』」。
3. A11 流量那一列：Hubble 改成在 agent Pod 內執行。
4. 4.3 的長連線逾時：預設 route timeout 15 秒是最可能的原因，待 config dump 確認。

## 7. 待確認清單（有叢集之後）

| # | 項目 | 怎麼確認 |
|---|---|---|
| 1 | 叢集實際 Kubernetes、CRI-O、Cilium、Envoy 版本 | `kubectl version`、`kubectl get nodes -o wide`、`cilium version`、`cilium-dbg envoy admin serverinfo` |
| 2 | `cilium-envoy` DaemonSet 存在、admin socket 可用 | `kubectl -n kube-system get ds`；`cilium-dbg envoy admin config` |
| 3 | koupa 路由實際的 route timeout、retry、stream idle、max stream duration | config dump，對照 3.1、3.2 |
| 4 | `CiliumGatewayClassConfig.spec.telemetry` 可用、四個欄位實際輸出 | 2.4 |
| 5 | `/stats` 的 cluster 名稱與計數器名稱 | `cilium-dbg envoy admin metrics` |
| 6 | Hubble 是否啟用、agent 內有無 `hubble` 執行檔（降級做法是否成立） | `cilium status`、`kubectl exec ... -- hubble observe` |
| 7 | terminating 端點在 Envoy EDS 的處理 | 追原始碼或實測 calibrate |
| 8 | CNPG 接受 `tag@digest`，且 `SHOW transaction_timeout` 可用 | 建 Cluster 時 |

## 8. 來源網址對照

所有網址固定在 tag 或 commit。Cilium：`https://github.com/cilium/cilium/blob/v1.20.4/`（commit `beb276d2f38d4aadfda14710acf8d14a0f1ba723`）；Envoy：`https://github.com/envoyproxy/envoy/blob/v1.39.5/`；cilium/proxy：`https://github.com/cilium/proxy/blob/8c73b2816bc8239e27f1fd53b271884688137459/`。行號是下載該 tag 原始碼時的行號。

| 主張 | 來源 |
|---|---|
| Envoy 版本 1.39.5 | [Cilium `images/cilium/Dockerfile#L11`](https://github.com/cilium/cilium/blob/v1.20.4/images/cilium/Dockerfile#L11)；[cilium/proxy `ENVOY_VERSION`](https://github.com/cilium/proxy/blob/8c73b2816bc8239e27f1fd53b271884688137459/ENVOY_VERSION) |
| admin socket 路徑 | [`bootstrap-config.yaml#L366`](https://github.com/cilium/cilium/blob/v1.20.4/install/kubernetes/cilium/files/cilium-envoy/configmap/bootstrap-config.yaml#L366)；[`cilium-dbg/cmd/envoy_adminclient.go`](https://github.com/cilium/cilium/blob/v1.20.4/cilium-dbg/cmd/envoy_adminclient.go) |
| access log 設定（`spec.telemetry.accessLogs`） | [`gatewayclassconfig_types.go#L157-L215`](https://github.com/cilium/cilium/blob/v1.20.4/pkg/k8s/apis/cilium.io/v2alpha1/gatewayclassconfig_types.go#L157-L215)；[`access-logs.rst`](https://github.com/cilium/cilium/blob/v1.20.4/Documentation/network/servicemesh/gateway-api/access-logs.rst)；[`translation/helper.go`（`buildAccessLogs`）](https://github.com/cilium/cilium/blob/v1.20.4/operator/pkg/model/translation/helper.go) |
| cilium-envoy 編入 stdout access logger | [`extensions_build_config.bzl#L16`](https://github.com/cilium/proxy/blob/8c73b2816bc8239e27f1fd53b271884688137459/envoy_build_config/extensions_build_config.bzl#L16) |
| `%UPSTREAM_REQUEST_ATTEMPT_COUNT%` 為 `value_or(0)` | [`stream_info_formatter.cc#L1681-L1688`](https://github.com/envoyproxy/envoy/blob/v1.39.5/source/common/formatter/stream_info_formatter.cc#L1681-L1688) |
| 格式運算子與回應旗標說明 | [`substitution_formatter.rst`](https://github.com/envoyproxy/envoy/blob/v1.39.5/docs/root/configuration/advanced/substitution_formatter.rst)（attempt count 第 213 行、`RESPONSE_CODE_DETAILS` 第 173 行、`RESPONSE_FLAGS` 第 489 行、`REQUEST_HEADER` 第 939 行） |
| 重試只在 HTTPRoute 有 `retry` 時產生 | [`envoy_virtual_host.go#L462-L497`](https://github.com/cilium/cilium/blob/v1.20.4/operator/pkg/model/translation/envoy_virtual_host.go#L462-L497)；[`ingestion/gateway.go#L522`](https://github.com/cilium/cilium/blob/v1.20.4/operator/pkg/model/ingestion/gateway.go#L522) |
| route timeout 沒設時的處理、max stream duration 0 | [`envoy_virtual_host.go#L501-L518`](https://github.com/cilium/cilium/blob/v1.20.4/operator/pkg/model/translation/envoy_virtual_host.go#L501-L518)；[`envoy_http_connection_manager.go#L148`](https://github.com/cilium/cilium/blob/v1.20.4/operator/pkg/model/translation/envoy_http_connection_manager.go#L148) |
| Envoy 預設 route timeout 15 秒、0 為停用 | [`config_impl.h#L976`](https://github.com/envoyproxy/envoy/blob/v1.39.5/source/common/router/config_impl.h#L976)；[`route_components.proto#L1373-L1376`](https://github.com/envoyproxy/envoy/blob/v1.39.5/api/envoy/config/route/v3/route_components.proto#L1373-L1376) |
| stream idle 300 秒、idle 60 秒 | [`ciliumenvoyconfig/cell.go#L39-L42`](https://github.com/cilium/cilium/blob/v1.20.4/operator/pkg/ciliumenvoyconfig/cell.go#L39-L42)；[`values.yaml#L2812-L2815`](https://github.com/cilium/cilium/blob/v1.20.4/install/kubernetes/cilium/values.yaml#L2812-L2815) |
| 實際產生的設定（golden） | [`testdata/basic_http_listener/cec-output.yaml`](https://github.com/cilium/cilium/blob/v1.20.4/operator/pkg/model/translation/gateway-api/testdata/basic_http_listener/cec-output.yaml) |
| outlier detection | [`envoy_cluster.go#L36`](https://github.com/cilium/cilium/blob/v1.20.4/operator/pkg/model/translation/envoy_cluster.go#L36) |
| Hubble 預設值 | [`values.yaml#L1448-L1450`](https://github.com/cilium/cilium/blob/v1.20.4/install/kubernetes/cilium/values.yaml#L1448-L1450)（enabled）、[`#L1761-L1763`](https://github.com/cilium/cilium/blob/v1.20.4/install/kubernetes/cilium/values.yaml#L1761-L1763)（relay） |
| Envoy cluster 統計名稱 | [`cluster_stats.rst`](https://github.com/envoyproxy/envoy/blob/v1.39.5/docs/root/configuration/upstream/cluster_manager/cluster_stats.rst) |
| CNPG 支援矩陣、預設映像檔 | [`supported_releases.md`](https://github.com/cloudnative-pg/cloudnative-pg/blob/v1.30.1/docs/src/supported_releases.md)；[`versions.go`](https://github.com/cloudnative-pg/cloudnative-pg/blob/v1.30.1/pkg/versions/versions.go) |
| tk8s 版本與安裝參數 | tk8s commit `94250ef`：`versions.yaml`、`src/tk8s/create/cni.py`（本機 `/home/bigred/tk8s`；是否有公開網址待確認） |
| Envoy 預設重試次數 1 次（有 `retry_on` 時） | 沒有查證來源，待確認 |
