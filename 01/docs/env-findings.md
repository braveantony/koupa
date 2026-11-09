# 環境盤點（Task 1）

調查日期：2026-11-09。這份文件只記錄事實：每一項都附指令與輸出，或附來源網址（含 tag 或 commit）。查不到的標「待確認」。

**更新（Task 1b，同日）**：叢集 `tkha` 起來後，已在即時叢集上逐項重驗。標「**已實測**」的項目附指令與輸出原文；與原始碼推論不同的地方標「**更正**」。**最重要的兩個更正**：(1) Gateway 路由沒寫 `timeouts` 時，**沒有** 15 秒 route timeout（見 3.2）；(2) HTTPRoute `retry.attempts: N` 會變成 `num_retries: N`，上游嘗試次數最多 N+1（見 3.1）。原本依原始碼寫的內容保留在各節，以「原始碼推論」標示，和實測結果並列。

## 0. 第一次調查的限制（歷史紀錄；叢集已於 Task 1b 補驗）

以下是 Task 1 當時的狀況，保留作紀錄。叢集 `tkha`（context `tkha`）已起來，第 1～4、7 節的即時驗證結果見各節的「已實測」。

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
| Kubernetes（`tkctl` 預設） | 1.37.2 | **已實測**：叢集 server 端 v1.37.2（見 1.4） |
| CRI-O | v1.37.2（對應 Kubernetes 1.37） | `crio."1.37"`；**已實測**：`cri-o://1.37.2`（見 1.4） |
| Cilium | 1.20.4 | `cilium: "1.20.4"`；chart 在 `/home/bigred/tk8s/manifests/cilium-1.20.4`；**已實測**：agent 與 daemon 都是 1.20.4（見 1.4） |
| Gateway API CRD | v1.6.3，experimental channel | `/home/bigred/tk8s/manifests/gateway-api.1.6.3.yaml` 標頭寫明 experimental；其中 HTTPRoute 有 `retry` 欄位；**已實測**：CRD annotation `bundle-version: v1.6.3`、`channel: experimental`（見 1.4） |
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
- Envoy 以獨立 DaemonSet `cilium-envoy` 執行（chart `envoy.enabled` 註解：「This field is enabled by default for new installation」）。**已實測**：有，4 個 Pod（見 1.4）。
- 本文下面引用的 Envoy 文件與原始碼，一律用 tag `v1.39.5`。

### 1.4 即時叢集版本（已實測，Task 1b）

```console
$ kubectl config use-context tkha
Switched to context "tkha".
$ kubectl version
Client Version: v1.37.2
Server Version: v1.37.2
$ kubectl get nodes -o wide      # 1 control plane + 3 worker
NAME                   STATUS   ROLES           VERSION   INTERNAL-IP   OS-IMAGE                       KERNEL-VERSION             CONTAINER-RUNTIME
tkha-control-plane-1   Ready    control-plane   v1.37.2   172.22.0.1    Debian GNU/Linux 13 (trixie)   7.0.0-42-generic (amd64)   cri-o://1.37.2
tkha-worker-1          Ready    <none>          v1.37.2   172.22.0.2    ...                            ...                        cri-o://1.37.2
tkha-worker-2          Ready    <none>          v1.37.2   172.22.0.3    ...
tkha-worker-3          Ready    <none>          v1.37.2   172.22.0.4    ...

$ kubectl -n kube-system exec ds/cilium -c cilium-agent -- cilium version
Client: 1.20.4 beb276d2 2026-10-23T13:25:25Z go version go1.26.10 linux/amd64
Daemon: 1.20.4 beb276d2 2026-10-23T13:25:25Z go version go1.26.10 linux/amd64

$ kubectl -n kube-system get ds cilium-envoy -o jsonpath='{.spec.template.spec.containers[*].image}'
quay.io/cilium/cilium-envoy:v1.39.5-1792733054-8c73b2816bc8239e27f1fd53b271884688137459@sha256:547ab45f6092f681d9f54f3be9fa8fb1e705c9c5dc2df388efe3a52aa6965629

$ kubectl -n kube-system exec ds/cilium -c cilium-agent -- cilium-dbg envoy admin serverinfo | head -3
{
 "version": "8c73b2816bc8239e27f1fd53b271884688137459/1.39.5/Distribution/RELEASE/BoringSSL",
 "state": "LIVE",

$ kubectl get crd httproutes.gateway.networking.k8s.io -o jsonpath='{.metadata.annotations}'
{"api-approved.kubernetes.io":"https://github.com/kubernetes-sigs/gateway-api/pull/4530","gateway.networking.k8s.io/bundle-version":"v1.6.3","gateway.networking.k8s.io/channel":"experimental"}

$ kubectl -n kube-system get ds,deploy
daemonset.apps/cilium         4 4 4 4 4
daemonset.apps/cilium-envoy   4 4 4 4 4
deployment.apps/cilium-operator 1/1 ...

$ kubectl -n kube-system get cm cilium-config -o yaml | grep -E "datapath-mode|kube-proxy-replacement:|enable-l2-announcements|external-envoy-proxy"
  datapath-mode: netkit
  enable-l2-announcements: "true"
  external-envoy-proxy: "true"
  kube-proxy-replacement: "true"

$ kubectl get gatewayclass
NAME     CONTROLLER                     ACCEPTED   AGE
cilium   io.cilium/gateway-controller   True       6m31s
```

結論：Kubernetes 1.37.2、CRI-O 1.37.2、Cilium 1.20.4、Envoy 1.39.5、Gateway API v1.6.3（experimental）、datapath netkit，**全部和原始碼調查所用的版本相同**，所以第 2、3 節引用的 v1.20.4 / v1.39.5 原始碼行號仍然有效，不必改查別的 tag。Envoy 以獨立 DaemonSet `cilium-envoy` 執行，4 個節點各一個 Pod（`external-envoy-proxy: "true"`，`cilium-dbg status` 顯示 `Envoy: external`）。

## 2. Envoy 的取得方式（步驟 2）

### 2.1 admin 介面

**已實測（Task 1b）**：admin 只能用 `cilium-dbg envoy admin` 取得。精確指令如下，在 `tkha` 上都能直接執行：

```console
# 每個節點各有一個 cilium agent Pod 與一個 cilium-envoy Pod；admin 是「該節點」的 Envoy
$ kubectl -n kube-system get pods -l k8s-app=cilium -o wide        # agent
$ kubectl -n kube-system get pods -l k8s-app=cilium-envoy -o wide  # envoy

$ kubectl -n kube-system exec <agent-pod> -c cilium-agent -- cilium-dbg envoy admin serverinfo
$ kubectl -n kube-system exec <agent-pod> -c cilium-agent -- cilium-dbg envoy admin config      > cfg.json   # 約 94 KB，JSON
$ kubectl -n kube-system exec <agent-pod> -c cilium-agent -- cilium-dbg envoy admin clusters
$ kubectl -n kube-system exec <agent-pod> -c cilium-agent -- cilium-dbg envoy admin listeners
$ kubectl -n kube-system exec <agent-pod> -c cilium-agent -- cilium-dbg envoy admin metrics     # 即 /stats，Prometheus 文字格式，約 3200 行
# 子指令清單：certs, clusters, config, listeners, logging, metrics, serverinfo

$ kubectl -n kube-system exec ds/cilium -c cilium-agent -- ls -l /var/run/cilium/envoy/sockets/
srw-rw---- 1 root 1337  0 Nov  9 03:44 access_log.sock
srw-rw---- 1 root root  0 Nov  9 03:44 admin.sock
srw-rw---- 1 root 1337  0 Nov  9 03:44 xds.sock
$ kubectl -n kube-system exec ds/cilium -c cilium-agent -- sh -c 'which curl wget'
（沒有輸出：agent 容器內沒有 curl、wget）
$ kubectl -n kube-system exec ds/cilium-envoy -- sh -c 'which curl wget busybox'
（沒有輸出：cilium-envoy 容器也沒有）
```

注意：`ds/cilium` 會挑任一個 Pod，要指定節點時用 `--field-selector spec.nodeName=<node>` 取得 Pod 名稱。`cilium-dbg envoy admin config` 的內容是**該節點**的 Envoy 設定；Gateway 的 listener、route、cluster 在每個節點都一樣，但統計與 access log 只在實際收到請求的節點上。Prometheus 格式裡的 cluster 名稱把 `:` 換成 `_`（`envcheck/cilium-gateway-gw/envcheck_echo_80`），`cilium-dbg envoy admin clusters` 則保留 `:`（`envcheck/cilium-gateway-gw/envcheck:echo:80`）。

下表是 Task 1 依原始碼寫的內容，結論已被上面的實測確認：

| 項目 | 結論 | 證據 |
|---|---|---|
| admin 位置 | 可以：unix socket `/var/run/cilium/envoy/sockets/admin.sock`，不是 TCP port | chart `install/kubernetes/cilium/files/cilium-envoy/configmap/bootstrap-config.yaml` 第 366 行 `admin: address: pipe: path: "/var/run/cilium/envoy/sockets/admin.sock"`、`mode: 0660` |
| 從哪裡連 | 可以：socket 所在的 hostPath 同時掛進 `cilium-envoy` 和 `cilium-agent` Pod | `templates/cilium-envoy/daemonset.yaml` 第 241、297 行（volume `envoy-sockets`）、`templates/cilium-agent/daemonset.yaml` 第 318 行 |
| config dump、clusters、metrics | 可以：agent 內建 `cilium-dbg envoy admin config / clusters / listeners / metrics / serverinfo / logging`（`cilium-dbg/cmd/envoy_admin*.go`）。範例：`kubectl -n kube-system exec ds/cilium -c cilium-agent -- cilium-dbg envoy admin config`；**已實測**可執行（見上） | 同左 |
| 直接打 admin API | 做不到（已實測）：Cilium 自己的 client 就是對 socket 發 HTTP GET（`cilium-dbg/cmd/envoy_adminclient.go`，`:authority` 為 `envoy-admin`）。**已實測**：容器內沒有 curl 也沒有 wget，所以不能直接打 socket，一律用 `cilium-dbg envoy admin` | 同左 |
| 另開 admin 的 TCP port | 預設沒有；要開 `envoy.debug.admin.enabled`（`cilium-configmap.yaml` 第 243 行 `proxy-admin-port`）。本任務不改 Cilium 設定 | 同左 |

要點：Cilium Gateway 的 Envoy 是**每個節點一個**（DaemonSet），不是每個 Gateway 一個。量測時要對**每一個節點**的 `cilium-envoy` 都收 log 與統計，否則 A11 要求的「涵蓋所有入口實例」就不成立。

### 2.2 access log

**已實測（Task 1b）**：`CiliumGatewayClassConfig.spec.telemetry.accessLogs` 在 1.20.4 的 CRD 裡存在，設定後 `cilium-envoy` 的 stdout 真的有輸出。三個和原始碼推論不同或要特別注意的地方：

1. **只能掛在 GatewayClass 上，不能掛在 Gateway 上。** 在 Gateway 寫 `spec.infrastructure.parametersRef` 會被拒絕：`Accepted=False, reason InvalidParameters, message: Invalid Gateway parameters: spec.infrastructure.parametersRef is not supported`。要用就必須有一個 GatewayClass 的 `spec.parametersRef` 指向它。`CiliumGatewayClassConfig` 是 **namespaced**，GatewayClass 的 `parametersRef` 要帶 `namespace`。
2. **叢集內建的 `cilium` GatewayClass 沒有 `parametersRef`**（`kubectl get gatewayclass cilium -o yaml`）。所以 koupa 若要開 access log，要嘛改內建 `cilium`（動到 Helm 管理的物件，本任務不做），要嘛自己建一個專屬 GatewayClass（`controllerName: io.cilium/gateway-controller`，`parametersRef` 指向自己 namespace 裡的 `CiliumGatewayClassConfig`）。本次實測用後者，驗完已刪除。
3. 輸出只出現在**收到請求的那個節點**的 `cilium-envoy` Pod（本次所有請求都進 `tkha-worker-1` 的 Pod `cilium-envoy-mmjm5`）。要涵蓋所有入口實例，必須對 4 個 `cilium-envoy` Pod 同時收 log。

測試用的設定（namespace `envcheck`，已刪除）：

```yaml
apiVersion: cilium.io/v2alpha1
kind: CiliumGatewayClassConfig
metadata: {name: envcheck-cfg, namespace: envcheck}
spec:
  telemetry:
    accessLogs:
    - format: Text
      targets: [HTTP]
      text: "ENVCHECK start=%START_TIME% path=%REQ(:PATH)% code=%RESPONSE_CODE% flags=%RESPONSE_FLAGS% details=%RESPONSE_CODE_DETAILS% upstream=%UPSTREAM_HOST% attempts=%UPSTREAM_REQUEST_ATTEMPT_COUNT% dur=%DURATION% koupa=%REQ(X-KOUPA-ATTEMPT)%"
---
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata: {name: envcheck-cilium}
spec:
  controllerName: io.cilium/gateway-controller
  parametersRef: {group: cilium.io, kind: CiliumGatewayClassConfig, name: envcheck-cfg, namespace: envcheck}
```

實際輸出（`kubectl -n kube-system logs <cilium-envoy-pod>`，原文）：

```text
ENVCHECK start=2026-11-09T03:53:01.598Z path=/hostname code=200 flags=- details=via_upstream upstream=10.244.2.31:8080 attempts=1 dur=0 koupa=3
ENVCHECK start=2026-11-09T03:58:39.925Z path=/drop code=503 flags=UC details=upstream_reset_before_response_started{connection_termination} upstream=10.244.1.186:8081 attempts=1 dur=2 koupa=7
ENVCHECK start=2026-11-09T03:58:39.933Z path=/retry code=503 flags=URX,UC details=upstream_reset_before_response_started{connection_termination} upstream=10.244.1.186:8081 attempts=3 dur=56 koupa=7
ENVCHECK start=2026-11-09T03:56:22.725Z path=/t30 code=200 flags=UT details=response_timeout upstream=10.244.2.189:8081 attempts=1 dur=30000 koupa=-
ENVCHECK start=2026-11-09T03:59:00.761Z path=/drop code=503 flags=UF details=upstream_reset_before_response_started{connection_timeout} upstream=10.244.1.186:8081 attempts=1 dur=5000 koupa=-
ENVCHECK start=2026-11-09T03:59:05.767Z path=/retry code=0 flags=DC details=downstream_remote_disconnect upstream=10.244.1.186:8081 attempts=2 dur=10002 koupa=-
```

觀察：`path` 用 `%REQ(:PATH)%` 時會帶 query string（第一次測試印出 `/t30/shell?cmd=...`）；要不含 query，用 `%REQUEST_HEADER(X-ENVOY-ORIGINAL-PATH?:PATH)%` 的行為這次沒另外測。`dur` 單位是毫秒。`X-Koupa-Attempt: 7` 標頭原樣出現在 `koupa=7`，沒帶時印 `-`。

下表是 Task 1 依原始碼寫的內容：

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
| `%UPSTREAM_REQUEST_ATTEMPT_COUNT%` | **可以**（依文件與原始碼；**已實測**：欄位有輸出，值為 1、2、3，見 2.2 的 log；「0」的情況這次沒有觸發，原始碼推論（沒設定也印 0）仍未實測） | 文件第 213 行：「Number of times the request is attempted upstream. An attempt count of `0` means that the request was never attempted upstream.」**但**原始碼 `source/common/formatter/stream_info_formatter.cc` 第 1686 行是 `stream_info.attemptCount().value_or(0)`：欄位沒設定時也印 `0`，不會印 `-`。所以 A11 說的三種情況（明確 0、依格式規格省略、沒設定或收集失敗）在 HTTP access log 裡分不出來：只要格式裡有這個欄位，輸出一定有數字。**單獨一個 `0` 有歧義**（可能是真的沒嘗試過上游，也可能是欄位沒設定），**判定規則：記錄到的 0 一律視為「未知」，除非同一行的回應旗標是 `UH` 或 `NR`（證明沒有嘗試過上游）才可判為「沒有上游嘗試」**。「欄位沒設定」要靠核對 log 格式本身，不能靠值判斷 |
| `%RESPONSE_FLAGS%` | **可以** | 文件第 489 行起。A11 列的旗標都在表內：`UH`（NoHealthyUpstream）、`UF`（UpstreamConnectionFailure）、`UC`（UpstreamConnectionTermination）、`UT`（UpstreamRequestTimeout）、`URX`（UpstreamRetryLimitExceeded）、`DC`（DownstreamConnectionTermination）；另有 `UR`（UpstreamRemoteReset）、`SI`（StreamIdleTimeout）、`UMSDR`（UpstreamMaxStreamDurationReached）、`DT`、`LR` 等 |
| `%RESPONSE_CODE_DETAILS%` | **可以** | 文件第 173 行 `%RESPONSE_CODE_DETAILS(X)%`，HTTP 才有，TCP 印 `-` |
| `%REQ(X-KOUPA-ATTEMPT)%` | **可以** | 文件第 939 行 `%REQUEST_HEADER(X?Y):Z%` / `%REQ(X?Y):Z%`；標頭不存在時印 `-` |
| 其他 A11 要的欄位 | **可以**：`%START_TIME%`、`%REQUEST_HEADER(X-ENVOY-ORIGINAL-PATH?:PATH)%`、`%RESPONSE_CODE%`、`%UPSTREAM_HOST%`、`%DURATION%` | Cilium 預設 JSON 範本本來就用到（types.go 第 186 行 `+kubebuilder:default`） |

自訂格式的 CRD 欄位上限：JSON 最多 64 個欄位、`text` 最長 4096 字元、`accessLogs` 最多 8 組。

### 2.4 這一節原本的待確認項目（Task 1b 逐項結果）

1. **已實測**：CRD 存在，`spec` 有 `description, envoy, httpOptions, service, telemetry`（`kubectl get crd ciliumgatewayclassconfigs.cilium.io -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties}'`），`telemetry.accessLogs` 有 `format/json/targets/text`。
2. **已實測**：有輸出，四個欄位都有值（見 2.2）。多個 Pod 用 `-l k8s-app=cilium-envoy` 時 `kubectl logs` 預設只印最後 10 行且要加 `--prefix`；實務上對每個 Pod 各收一條。
3. **已實測**：`X-Koupa-Attempt` 在 access log 原樣印出；**上游也收到**：後端用 agnhost `/header?key=<名稱>`（回傳該請求標頭的值）。`curl -H 'X-Koupa-Attempt: 9' http://<node>:<nodePort>/header?key=X-Koupa-Attempt` 回 `9`；`key=X-Forwarded-For` 回 `172.22.0.254`（Envoy 加的）；`key=X-Envoy-Original-Path` 回空（這個標頭只在 Envoy 改寫路徑時才有，沒有改寫就沒有）。原文見 3.6。
4. chart 預設 `envoy.log.accessLogEnabled: true`（configmap 的 `envoy-access-log-enabled: "true"`，**已實測**）是給 Hubble 用的 access log 轉送，和上面的 stdout 設定是兩回事，不要混在一起。Hubble 確實看得到 Gateway 的 HTTP 請求（見第 4 節）。

## 3. 重試與逾時（步驟 3）

來源：`cilium/cilium` tag `v1.20.4`（commit `beb276d2...`）`operator/pkg/model/translation/` 與 `operator/pkg/model/ingestion/gateway.go`。**Task 1b 已用即時 config dump 與實際請求逐項核對，結果見 3.4；3.1、3.2 兩節的表格是 Task 1 依原始碼寫的推論，有出入的地方以「更正」標在表後。**

### 3.1 重試

| 項目 | 結論 | 證據 |
|---|---|---|
| 預設會不會重試 | **不會**。只有 HTTPRoute 規則有寫 `retry` 才會產生 `RetryPolicy`；沒寫就沒有重試政策 | `envoy_virtual_host.go` 第 471～497 行 `retryMutation`：`if retry == nil { return route }`。`ingestion/gateway.go` 第 522、545 行 `Retry: toHTTPRetry(rule.Retry)`，來源只有 `rule.Retry`（HTTPRoute 的 `retry` 欄位） |
| 能不能關 | **可以**：HTTPRoute 不寫 `retry` 就是關，不需要改 Cilium 設定 | 同上。`operator/` 與 `pkg/envoy` 內，Gateway 路徑只有 `envoy_virtual_host.go` 一處會設 `RetryPolicy`（`pkg/envoy/model.go` 第 1030 行那組 `retryOn: 5xx` 屬於 L7 policy 的 listener，不是 Gateway 路由） |
| 有寫 `retry` 時的行為 | `retry_on = "retriable-status-codes,connect-failure,reset,refused-stream"`；次數用 `attempts`，沒寫 `attempts` 就是 Envoy 預設（1 次重試，即最多 2 次上游嘗試；這是 Envoy 的預設值，待用 config dump 確認）。程式碼註解明寫「applies to all HTTP methods, including non-idempotent ones」（POST 也會重試） | `envoy_virtual_host.go` 第 462～469 行 |
| 對 A11 的意義 | A11 說的「關閉入口重試，上游嘗試次數最多 1 次」**可做**：koupa 的 HTTPRoute 不要寫 `retry` | — |

**3.1 更正與實測（Task 1b）**

測試路由（namespace `envcheck`，已刪除）：`/` 無 `retry`；`/retry` 寫 `retry: {codes: [503], attempts: 2}`；`/drop` 無 `retry`。後端是會直接關閉連線的伺服器，用來製造 `UC`（上游連線被關閉）。

```console
$ kubectl -n kube-system exec <agent-pod> -c cilium-agent -- cilium-dbg envoy admin config   # 取 RoutesConfigDump
{"match": {"path_separated_prefix": "/retry"}, "route": {"cluster": "envcheck/cilium-gateway-gw/envcheck:echo:80", "retry_policy": {"retry_on": "retriable-status-codes,connect-failure,reset,refused-stream", "num_retries": 2, "retriable_status_codes": [503]}, "max_stream_duration": {"max_stream_duration": "0s"}}}
{"match": {"prefix": "/"}, "route": {"cluster": "envcheck/cilium-gateway-gw/envcheck:echo:80", "max_stream_duration": {"max_stream_duration": "0s"}}}
```

- **已實測：預設不重試。** 沒寫 `retry` 的路由（`/`、`/sse`、`/drop`）在 config dump 裡沒有 `retry_policy`。`/drop` 的 access log：`code=503 flags=UC ... attempts=1`（一次嘗試就失敗、不重試）。
- **更正：`attempts: N` 是 `num_retries: N`，不是總嘗試次數。** `attempts: 2` 產生 `num_retries: 2`；實際的 `/retry` 請求 access log 是 `flags=URX,UC attempts=3`（1 次原始 + 2 次重試），Envoy 統計 `upstream_rq_retry` 增加 2、`upstream_rq_retry_limit_exceeded` 增加 1。要把上游嘗試次數壓在 2，必須寫 `attempts: 1`；沒寫 `attempts` 時的值**這次沒測**（仍待確認）。
- **已實測：重試條件含 `reset`、`connect-failure`**（`retry_on` 字串如上），所以只要有人寫了 `retry`，連線被重置的 POST 也會被重試（`/retry` 就是 UC 後被重試）。
- 重試的 backoff：`upstream_rq_retry_backoff_exponential` 增加 2，表示用的是 Envoy 預設的指數退避。

另一個干擾因素：每個 cluster 都有 `outlierDetection: splitExternalLocalOriginErrors: true`（`envoy_cluster.go` 第 36 行 `withOutlierDetection(true)`；golden 檔 `operator/pkg/model/translation/gateway-api/testdata/basic_http_listener/cec-output.yaml`）。Envoy 的 outlier detection 在端點連續失敗時會暫時把它踢出負載平衡，這會讓「入口停止選某個 Pod」多一個和 EndpointSlice 無關的原因。**已實測**：config dump 裡 Gateway 產生的 cluster 確實有 `"outlier_detection": {"split_external_local_origin_errors": true}`（只有這一項，其餘用 Envoy 預設）。它造成的實際排除行為這次沒有觀察到（沒有觸發連續失敗），calibrate（A11）的推論仍要把它列為干擾因素。

### 3.2 逾時

**3.2 實測結果先講（Task 1b）：**

| 項目 | 原始碼推論（Task 1） | 即時實測 | 狀態 |
|---|---|---|---|
| 沒寫 `timeouts` 的路由，route timeout | 15 秒（Envoy 預設） | **沒有逾時。** `/slowhdr`（上游 25 秒才送出回應標頭）得到 `200`、`x-envoy-upstream-service-time: 25006`；`/sse`（串流）連續收了 100 秒（curl `-m 100` 才結束），access log `flags=DC`（是 curl 自己斷線） | **更正** |
| 寫了 `timeouts.request: 30s` | 產生 `timeout` | config dump：`"timeout": "30s"`；實測 30.0 秒被切斷，access log：`code=200 flags=UT details=response_timeout dur=30000`；收到 30 行 SSE 資料，curl 結束碼 18（回應不完整） | 已實測 |
| 寫了 `timeouts.request: 0s` | `timeout: 0s`（停用） | config dump：`"timeout": "0s"`；串流連續收 100 秒沒被切斷 | 已實測 |
| max stream duration | route 與 HCM 都是 `0s` | HCM 的 `common_http_protocol_options.max_stream_duration: 0s` 已實測；route 的 `max_stream_duration: {max_stream_duration: 0s}` **只有沒寫 `timeouts` 的路由才有**，寫了 `timeouts` 的路由沒有這個欄位 | 部分更正 |
| stream idle timeout | 300 秒 | HCM `stream_idle_timeout: "300s"` 已在 config dump 看到；**實際切斷行為沒有測**（要靜默 300 秒以上） | config 已實測；行為待確認 |
| 上游連線逾時 | `connect_timeout` 2 秒（Helm `connectTimeoutSeconds: 2`） | Gateway 動態 cluster 的 config dump **沒有 `connect_timeout` 欄位**（只有內建的 static cluster 是 `2s`）。實際連不上時 `UF` 在 **5000 ms**（`dur=5000`、`connection_timeout`），這是 Envoy 的預設 5 秒，不是 2 秒 | **更正** |
| 上游連線 idle timeout | 60 秒 | dynamic cluster：`common_http_protocol_options.idle_timeout: "60s"` | 已實測 |

**為什麼沒有 15 秒（原因，已對照 Envoy v1.39.5 原始碼）：** Cilium 對沒寫 `timeouts` 的路由會產生 `max_stream_duration: {max_stream_duration: 0s}`。在 Envoy 的 `source/common/router/config_impl.cc` 第 553 行，`using_new_timeouts_ = route.route().has_max_stream_duration()`；`source/common/router/router.cc` 第 191 行 `FilterUtility::finalTimeout` 只有在 `!route.usingNewTimeouts()` 時才採用 `route.timeout()`（15 秒預設值也在這個分支裡）。所以只要路由帶有 `max_stream_duration`，預設的 15 秒 route timeout 就**不會被採用**。這和 Task 1 只看 `DEFAULT_ROUTE_TIMEOUT_MS = 15000` 與 proto 註解得到的結論相反。

對 A11／4.3 的意義（取代下方依原始碼的推論）：預設情況下，入口**不會**在 15 秒切斷長連線；要有 route timeout 必須明確寫 `timeouts.request`。要讓 SSE 活著，**什麼都不用寫**（或寫 `0s` 讓意圖明確）；真正會切斷靜默連線的是 stream idle 300 秒（config 已確認，行為待測）。寫了 `timeouts.request: <T>` 的路由，T 秒後會被切斷，access log 是 `flags=UT details=response_timeout`。

**以下是 Task 1 依原始碼寫的表（其中 route timeout、連線逾時兩列已被上面更正）：**

Cilium 1.20.4 對 Gateway 產生的設定（以上述 golden 檔為準）：

| 逾時 | 預設值 | 在哪設 | 能不能改 | 證據 |
|---|---|---|---|---|
| route timeout | **沒設定**（Cilium 不寫 `route.timeout`）。Task 1 原推論「落到 Envoy 預設 15 秒」**已被實測推翻**：實際生效為**沒有逾時**（原因與實測見 3.2 開頭與 3.5） | `envoy_virtual_host.go` 第 501～518 行 `timeoutMutation`：`backend`、`request` 都沒寫時不設 `route.timeout` | 可以：HTTPRoute 的 `rules[].timeouts.request` / `backendRequest`（取兩者較小）；寫 `0s` 即停用（Cilium 轉成 `timeout: 0s`；Envoy：0 表示停用） | Envoy `source/common/router/config_impl.h` 第 976 行 `DEFAULT_ROUTE_TIMEOUT_MS = 15000`；`api/envoy/config/route/v3/route_components.proto` 第 1373～1376 行「If not specified, the default is 15s … A value of 0 will disable the route's timeout」 |
| max stream duration | route 與 HCM 都明確設成 `0s`（不限制） | `envoy_virtual_host.go` 第 504 行；`envoy_http_connection_manager.go` 第 148 行 | 無 | golden 檔 `maxStreamDuration: 0s` |
| stream idle timeout | **300 秒** | HCM `streamIdleTimeout`；Helm `envoy.streamIdleTimeoutDurationSeconds`，operator 旗標 `--proxy-stream-idle-timeout-seconds` | 可以改 Helm（本任務不改） | `operator/pkg/ciliumenvoyconfig/cell.go` 第 39～42 行；values.yaml 第 2815 行；golden 檔 `streamIdleTimeout: 300s` |
| 上游連線 idle timeout | **60 秒**（cluster 的 `commonHttpProtocolOptions.idleTimeout`） | Helm `envoy.idleTimeoutDurationSeconds` | 同上 | values.yaml 第 2812 行；golden 檔 `idleTimeout: 60s` |
| 連線上游逾時 | Helm `envoy.connectTimeoutSeconds: 2` | — | — | values.yaml 第 2791 行；**更正（已實測）**：Gateway 產生的動態 cluster 沒有沿用，實際是 Envoy 預設 5 秒（見 3.2 開頭的表） |
| 下游 TCP keepalive | idle 10 秒、間隔 5 秒、最多 10 次 | listener `socketOptions` | — | golden 檔 |

**（以下兩點是 Task 1 的原始碼推論，其中 15 秒的部分已被上方實測更正，請勿引用）** 對 A11／4.3 的意義：

- `/events`（SSE）如果 HTTPRoute 沒寫 `timeouts`，route timeout 會是 15 秒，長連線約 15 秒就被入口切斷，不是 stream idle 的 300 秒。4.3 的「入口切斷長連線是哪一種逾時」，原始碼給的最可能答案是 route timeout 15 秒；回應旗標會是什麼待確認。要讓 SSE 活著，koupa 的 HTTPRoute 對該規則寫 `timeouts: {request: 0s}`；此時 stream idle 300 秒仍然存在，所以伺服器要在 300 秒內送出資料（心跳）。
- config dump 裡的欄位名稱：route 的 `timeout`、`max_stream_duration`；HCM 的 `stream_idle_timeout`；cluster 的 `typed_extension_protocol_options` 下的 `common_http_protocol_options.idle_timeout`。

### 3.3 這一節原本的待確認項目（Task 1b 結果）

1. **已實測**：結果見 3.1、3.2。route timeout 沒設時實際上是「無逾時」；dynamic cluster 沒有 `connect_timeout`（Envoy 預設 5 秒生效）。
2. Cilium 對 terminating 端點在 Envoy EDS 裡的處理（calibrate 的上界 U 要靠它）：**仍待確認**（需要 `measure.sh calibrate` 級的實驗）。本次只觀察到一個相關現象：把 `sse` Deployment 縮到 0 副本、EndpointSlice 的 endpoints 已經是 `<unset>` 之後，Envoy 的 cluster 仍保留舊的上游位址 `10.244.1.186:8081`，23 秒後仍繼續把請求送去（`/drop` 在 `dur≈5000`、`flags=UF`、`connection_timeout` 失敗；`cilium-dbg envoy admin clusters` 仍列出該位址的 `cx_connect_fail::4`）。這是「縮到 0」的情況，不是「換一個新 Pod」，不能直接當作 calibrate 的答案，但顯示入口對「端點全部消失」可能不會立刻更新 EDS。

### 3.5 原始輸出（Task 1b 修正補充）

測試環境：namespace `envcheck`（2026-11-09 重建一次，用同一份設定；測完已再次刪除，見 7.2）。Gateway 的 NodePort 是 32125，從主機打 `172.22.0.2:32125`（`tkha-worker-1`）。`sse` 後端每秒送一行 `data: tick <序號> <時間>`，路徑含 `slowhdr` 時先等 25 秒才送回應標頭。對應的路由：`/sse`（無 `timeouts`）、`/slowhdr`（無 `timeouts`）、`/t30`（`timeouts.request: 30s`）。

#### 3.5.1 回應標頭延遲 25 秒（`/slowhdr`，沒寫 `timeouts`）

```console
$ date -u +%T; curl -s -m 60 -i -o /dev/null -D - -w 'time_total=%{time_total} http_code=%{http_code}\n' "http://172.22.0.2:32125/slowhdr"; date -u +%T
04:05:03
HTTP/1.1 200 OK
content-type: text/event-stream
cache-control: no-cache
x-envoy-upstream-service-time: 25005
date: Mon, 09 Nov 2026 04:05:28 GMT
server: envoy
transfer-encoding: chunked

time_total=60.002441 http_code=200
04:06:03
```

起點 04:05:03，標頭的 `date` 是 04:05:28，`x-envoy-upstream-service-time: 25005`（毫秒），狀態 200：標頭等了 25 秒仍沒有被 15 秒 route timeout 打斷。（連線之後是串流，curl `-m 60` 在 60 秒結束，`time_total=60.002441` 是這個原因。）

#### 3.5.2 SSE 串流 100 秒（`/sse`，沒寫 `timeouts`）

```console
$ date -u +%T; curl -s -m 100 -N -D hdr_sse.txt http://172.22.0.2:32125/sse -o sse_body.txt; echo "curl exit=$?"; date -u +%T
04:05:03
curl exit=28
04:06:43
$ cat hdr_sse.txt
HTTP/1.1 200 OK
content-type: text/event-stream
cache-control: no-cache
x-envoy-upstream-service-time: 2
date: Mon, 09 Nov 2026 04:05:03 GMT
server: envoy
transfer-encoding: chunked
$ grep -c '^data' sse_body.txt;  head -1 sse_body.txt;  grep '^data' sse_body.txt | tail -1
100
data: tick 0 04:05:03
data: tick 99 04:06:42
```

04:05:03 到 04:06:43 共 100 秒、收到 100 筆資料，curl 結束碼 28（`-m 100` 逾時，是用戶端自己結束），沒有在 15 秒被切斷。對照 `/t30`（`timeouts.request: 30s`）：

```console
$ curl -s -m 60 -N -D hdr_t30.txt http://172.22.0.2:32125/t30 -o t30_body.txt; echo "curl exit=$?"
curl exit=18
$ grep -c '^data' t30_body.txt;  grep '^data' t30_body.txt | tail -1
30
data: tick 29 04:05:32
```

30 秒、30 筆，結束碼 18（回應不完整）。對應的 access log 行：

```text
ENVCHECK start=2026-11-09T04:05:03.505Z path=/slowhdr code=200 flags=DC details=downstream_remote_disconnect upstream=10.244.3.163:8081 attempts=1 dur=60003 koupa=-
ENVCHECK start=2026-11-09T04:05:03.505Z path=/t30 code=200 flags=UT details=response_timeout upstream=10.244.3.163:8081 attempts=1 dur=30000 koupa=-
```

（上一輪測試同樣行為：`/sse` `dur=100004 flags=DC`、`/t30` `dur=30000 flags=UT`。`/sse` 這輪的那一行已被 `--tail=40` 擠出視窗，原文見第一輪 log，不重貼。）

#### 3.5.3 CRD 原文（`CiliumGatewayClassConfig`）

```console
$ kubectl get crd ciliumgatewayclassconfigs.cilium.io -o yaml | sed -n 160,250p
                type: object
              telemetry:
                description: |-
                  Telemetry specifies observability options for Gateways using this
                  GatewayClass configuration.
                properties:
                  accessLogs:
                    description: |-
                      AccessLogs configures Envoy access logging for generated Gateway
                      listeners.
                    items:
                      description: |-
                        AccessLogs defines an Envoy access log configuration, including its output
                        format and the generated proxy components that should emit it.
                        Access logs are currently written to Envoy stdout.
                      properties:
                        format:
                          description: Format specifies the access log output format.
                          enum:
                          - JSON
                          - Text
                          type: string
                        json:
                          additionalProperties:
                            type: string
                          default:
                            authority: '%REQUEST_HEADER(:AUTHORITY)%'
                            bytes_received: '%BYTES_RECEIVED%'
                            bytes_sent: '%BYTES_SENT%'
                            duration: '%DURATION%'
                            method: '%REQUEST_HEADER(:METHOD)%'
                            path: '%REQUEST_HEADER(X-ENVOY-ORIGINAL-PATH?:PATH)%'
                            protocol: '%PROTOCOL%'
                            request_id: '%REQUEST_HEADER(X-REQUEST-ID)%'
                            response_code: '%RESPONSE_CODE%'
                            response_flags: '%RESPONSE_FLAGS%'
                            start_time: '%START_TIME%'
                            upstream_host: '%UPSTREAM_HOST%'
                            upstream_service_time: '%RESPONSE_HEADER(X-ENVOY-UPSTREAM-SERVICE-TIME)%'
                            user_agent: '%REQUEST_HEADER(USER-AGENT)%'
                            x_forwarded_for: '%REQUEST_HEADER(X-FORWARDED-FOR)%'
                          description: |-
                            JSON maps access log field names to Envoy command operators.
                            It is used when Format is "JSON".
                            For available format specifiers, see the Envoy documentation:
                            - https://www.envoyproxy.io/docs/envoy/latest/configuration/observability/access_log/usage
                            Note: Always refer to the documentation matching the specific Envoy version you are running.
                            The following Cilium-specific formatters are also supported:
                            - %CILIUM_GATEWAY_NAME% -- replaced with the Gateway resource name.
                            - %CILIUM_GATEWAY_NAMESPACE% -- replaced with the Gateway resource namespace.
                          maxProperties: 64
                          minProperties: 1
                          type: object
                        targets:
                          default:
                          - HTTP
                          description: |-
                            Targets specifies the generated Envoy proxy components where access logs
                            are emitted. If omitted, access logs are emitted for HTTP traffic only.
                            HTTP targets Envoy HTTP connection managers. TCP targets Envoy TCP proxies,
                            including TLS passthrough.
                          items:
                            description: AccessLogsTarget specifies where access logs
                              are emitted.
                            enum:
                            - HTTP
                            - TCP
                            type: string
                          minItems: 1
                          type: array
                          x-kubernetes-list-type: set
                        text:
                          default: '[%START_TIME%] "%REQUEST_HEADER(:METHOD)% %REQUEST_HEADER(X-ENVOY-ORIGINAL-PATH?:PATH)%
                            %PROTOCOL%" %RESPONSE_CODE% %RESPONSE_FLAGS% %BYTES_RECEIVED%
                            %BYTES_SENT% %DURATION% %RESPONSE_HEADER(X-ENVOY-UPSTREAM-SERVICE-TIME)%
                            "%REQUEST_HEADER(X-FORWARDED-FOR)%" "%REQUEST_HEADER(USER-AGENT)%"
                            "%REQUEST_HEADER(X-REQUEST-ID)%" "%REQUEST_HEADER(:AUTHORITY)%"
                            "%UPSTREAM_HOST%"'
                          description: |-
                            Text specifies the Envoy access log format string.
                            It is used when Format is "Text".
                            For available format specifiers, see the Envoy documentation:
                            - https://www.envoyproxy.io/docs/envoy/latest/configuration/observability/access_log/usage
                            Note: Always refer to the documentation matching the specific Envoy version you are running.
                            The following Cilium-specific formatters are also supported:
                            - %CILIUM_GATEWAY_NAME% -- replaced with the Gateway resource name.
                            - %CILIUM_GATEWAY_NAMESPACE% -- replaced with the Gateway resource namespace.
                          maxLength: 4096
                          minLength: 1
                          type: string
                      required:
```

`spec` 的第一層欄位（同一個 CRD，2.4 第 1 點的指令）：`description`、`envoy`、`httpOptions`、`service`、`telemetry`。完整 `jsonpath` 輸出另存於 Task 1b 的暫存檔，內容與上面的 YAML 一致，不重貼。

#### 3.5.4 統計計數器原文

```console
$ kubectl -n kube-system exec pod/cilium-7c9w4 -c cilium-agent -- cilium-dbg envoy admin metrics | grep -E 'envcheck_sse_80' | grep -E '^envoy_cluster_(upstream_rq_total|upstream_rq_retry|upstream_rq_retry_limit_exceeded|upstream_rq_timeout|upstream_cx_destroy_remote_with_active_rq|upstream_rq_pending_failure_eject|upstream_rq_xx|upstream_rq)\{'
envoy_cluster_upstream_cx_destroy_remote_with_active_rq{envoy_cluster_name="envcheck/cilium-gateway-gw/envcheck_sse_80"} 4
envoy_cluster_upstream_rq{envoy_response_code="200",envoy_cluster_name="envcheck/cilium-gateway-gw/envcheck_sse_80"} 3
envoy_cluster_upstream_rq{envoy_response_code="503",envoy_cluster_name="envcheck/cilium-gateway-gw/envcheck_sse_80"} 2
envoy_cluster_upstream_rq_pending_failure_eject{envoy_cluster_name="envcheck/cilium-gateway-gw/envcheck_sse_80"} 0
envoy_cluster_upstream_rq_retry{envoy_cluster_name="envcheck/cilium-gateway-gw/envcheck_sse_80"} 2
envoy_cluster_upstream_rq_retry_limit_exceeded{envoy_cluster_name="envcheck/cilium-gateway-gw/envcheck_sse_80"} 1
envoy_cluster_upstream_rq_timeout{envoy_cluster_name="envcheck/cilium-gateway-gw/envcheck_sse_80"} 1
envoy_cluster_upstream_rq_total{envoy_cluster_name="envcheck/cilium-gateway-gw/envcheck_sse_80"} 7
envoy_cluster_upstream_rq_xx{envoy_response_code_class="2",envoy_cluster_name="envcheck/cilium-gateway-gw/envcheck_sse_80"} 3
envoy_cluster_upstream_rq_xx{envoy_response_code_class="5",envoy_cluster_name="envcheck/cilium-gateway-gw/envcheck_sse_80"} 2
```

這是重建 namespace 之後、跑完 `/slowhdr`、`/sse`、`/t30`、`/drop`、`/retry` 各一次的累計值（`/slowhdr` 與 `/sse` 的串流被用戶端中斷，所以 `upstream_cx_destroy_remote_with_active_rq` 也有數字）。`upstream_rq_retry=2`、`upstream_rq_retry_limit_exceeded=1` 對應 `/retry` 的 `attempts: 2`；`upstream_rq_timeout=1` 對應 `/t30` 的 `UT`。

#### 3.5.5 對應的 access log 原文（`/drop`、`/retry`）

```text
ENVCHECK start=2026-11-09T03:58:39.925Z path=/drop code=503 flags=UC details=upstream_reset_before_response_started{connection_termination} upstream=10.244.1.186:8081 attempts=1 dur=2 koupa=7
ENVCHECK start=2026-11-09T03:58:39.933Z path=/retry code=503 flags=URX,UC details=upstream_reset_before_response_started{connection_termination} upstream=10.244.1.186:8081 attempts=3 dur=56 koupa=7
```

（第二輪重建後也再打了一次 `/drop`、`/retry`，結果相同：`/drop` 503、`/retry` 503，統計如 3.5.4。）

### 3.6 API server audit log 與上游標頭保留（Task 1b 修正補充）

#### 3.6.1 audit

```console
$ sudo podman exec tkha-control-plane-1 sh -c 'sed -n 40,48p /etc/kubernetes/manifests/kube-apiserver.yaml'
    - --tls-cert-file=/etc/kubernetes/pki/apiserver.crt
    - --tls-private-key-file=/etc/kubernetes/pki/apiserver.key
    - --audit-log-format=legacy
    - --audit-policy-file=/opt/taroko/logs/apiserver/audit-policy.yaml
    - --audit-log-path=/opt/taroko/logs/apiserver/audit.log
    - --audit-log-maxage=3
    - --audit-log-maxbackup=5
    - --audit-log-maxsize=10
    image: registry.k8s.io/kube-apiserver:v1.37.2

$ sudo podman exec tkha-control-plane-1 sh -c 'ls -l /opt/taroko/logs/apiserver/; cat /opt/taroko/logs/apiserver/audit-policy.yaml'
-rw-r--r-- 1 1000 1000    460 Nov  9 11:44 audit-policy.yaml
-rw------- 1 root root 910083 Nov  9 12:04 audit.log
apiVersion: audit.k8s.io/v1
kind: Policy
omitStages:
  - "RequestReceived"
rules:
  - level: Request
    verbs: ["create","update","patch","delete","watch","get","list","deletecollection"]
    resources:
      - group: ""  # core
        resources: ["pods", "services", "secrets", "configmaps"]
      - group: "apps"
        resources: ["daemonsets", "deployments", "replicasets", "statefulsets"]
      - group: "batch"
        resources: ["jobs", "cronjobs"]

$ kubectl -n envcheck delete pod echo-75579c9647-gbffx --wait=false
pod "echo-75579c9647-gbffx" deleted from envcheck namespace
$ sudo podman exec tkha-control-plane-1 sh -c 'grep "method=\"delete\"" /opt/taroko/logs/apiserver/audit.log | grep "namespaces/envcheck/pods" | tail -2'
2026-11-09T04:04:54.434051059Z AUDIT: id="808cc6fc-0cca-4044-8985-abae3fa770f5" stage="ResponseComplete" ip="172.22.0.253" method="delete" user="kubernetes-admin" groups="\"kubeadm:cluster-admins\",\"system:authenticated\"" as="<self>" asgroups="<lookup>" user-agent="kubectl/v1.37.2 (linux/amd64) kubernetes/c50e539" namespace="envcheck" uri="/api/v1/namespaces/envcheck/pods/echo-75579c9647-gbffx" response="200"
2026-11-09T04:04:56.409806603Z AUDIT: id="aade76ff-fe7a-4cab-8eb7-46900d0f6416" stage="ResponseComplete" ip="172.22.0.253" method="delete" user="system:node:tkha-worker-1" groups="\"system:nodes\",\"system:authenticated\"" as="<self>" asgroups="<lookup>" user-agent="kubelet/v1.37.2 (linux/amd64) kubernetes/c50e539" namespace="envcheck" uri="/api/v1/namespaces/envcheck/pods/echo-75579c9647-gbffx" response="200"
```

（`audit.log` 單行過長的部分我用 `cut` 截過一次做瀏覽，上面兩行 `delete` 是未截斷的原文。）兩筆刪除：04:04:54.43 是 kubectl 發的刪除請求（對應 grace period 開始），04:04:56.41 是 kubelet 在容器停掉後做的最終刪除。這可以當 A11 交叉核對 Pod 刪除時間的來源；`--audit-log-format=legacy` 是單行文字格式，不是 JSON，解析要用正規表示式。

#### 3.6.2 上游標頭保留

後端是 agnhost `netexec` 的 `/header?key=<名稱>`（回傳請求中該標頭的值）。

```console
$ curl -s -m5 -D - -H 'X-Koupa-Attempt: 9' "http://172.22.0.2:32125/header?key=X-Koupa-Attempt"
HTTP/1.1 200 OK
date: Mon, 09 Nov 2026 04:04:51 GMT
content-length: 1
content-type: text/plain; charset=utf-8
x-envoy-upstream-service-time: 0
server: envoy

9
$ curl -s -m5 -H 'X-Koupa-Attempt: 9' "http://172.22.0.2:32125/header?key=X-Envoy-Original-Path"
（空行：沒有這個標頭）
$ curl -s -m5 -H 'X-Koupa-Attempt: 9' "http://172.22.0.2:32125/header?key=X-Forwarded-For"
172.22.0.254
```

結論：自訂標頭 `X-Koupa-Attempt` 穿過 Gateway 原樣送到上游，Envoy 另外加了 `X-Forwarded-For`。

## 4. Hubble（步驟 4）

| 項目 | 結論 | 證據 |
|---|---|---|
| 是否啟用 | **已實測：啟用**（原本依 chart 預設推論；`cilium-dbg status` 顯示 `Hubble: Ok  Current/Max Flows: 1760/4095 (42.98%)`，configmap `enable-hubble: "true"`）：agent 內建 Hubble server，socket `/var/run/cilium/hubble.sock` | chart `values.yaml` 第 1448～1450 行 `hubble: enabled: true`、第 1599 行 `socketPath`；`cilium-configmap.yaml` 第 1054 行 `enable-hubble`；tk8s 的 `cni.py` 沒有覆寫任何 `hubble.*` |
| Hubble Relay、UI | **預設關閉** | values.yaml 第 1761～1763 行 `relay.enabled: false`、第 2004～2006 行 `ui.enabled: false`；tk8s 沒開 |
| 主機上的 `hubble` CLI | **沒有**（已實測，`which hubble` 無輸出） | `which hubble` 沒有輸出 |
| `hubble observe` 在主機上能不能跑 | **不能直接跑**（沒有 CLI、也沒有 Relay）。**降級做法（已實測可行）**：在 agent Pod 內執行 `kubectl -n kube-system exec ds/cilium -c cilium-agent -- hubble observe -o json`。旁證：[`images/README.md`](https://github.com/cilium/cilium/blob/v1.20.4/images/README.md) 寫 cilium 映像檔含 `hubble-cli`，[`images/cilium/Dockerfile` 第 105 行](https://github.com/cilium/cilium/blob/v1.20.4/images/cilium/Dockerfile#L105)設了 `HUBBLE_SERVER=unix:///var/run/cilium/hubble.sock`。但 Dockerfile 沒有直接 COPY `hubble` 執行檔（是從 builder 階段整包複製），**已實測：執行檔在 `/usr/bin/hubble`**（見下）。做法有效的話，也只看得到**該節點**的流量，多節點要每個 agent Pod 各一條 | 同上 |
| 能看到 Gateway 的 L7 嗎 | **已實測：看得到。** `hubble observe --type l7` 印出 `http-request`／`http-response`，含方法、URL、狀態碼與耗時（見下）。但**沒有** `X-Koupa-Attempt`、上游位址、回應旗標、嘗試次數，所以做不到 A11 的對帳，只能當輔助 | values.yaml `envoy.log.accessLogEnabled` |

已實測輸出：

```console
$ kubectl -n kube-system exec ds/cilium -c cilium-agent -- sh -c 'which hubble; hubble version'
/usr/bin/hubble
hubble v1.20.4@HEAD-beb276d2 compiled with go1.26.10 on linux/amd64

$ kubectl -n kube-system exec ds/cilium -c cilium-agent -- hubble observe --last 5
Nov  9 03:50:52.801: 172.22.0.3:35984 (host) -> 172.22.0.2:4240 (remote-node) to-network FORWARDED (TCP Flags: SYN)
...（共 5 行，都是 TCP 流量）

$ kubectl -n kube-system exec pod/cilium-7c9w4 -c cilium-agent -- hubble observe --type l7 --last 5
Nov  9 03:59:05.761: 172.22.0.254:47422 (ingress) -> 172.22.0.2:32066 (host) http-request FORWARDED (HTTP/1.1 GET http://172.22.0.2:32066/drop)
Nov  9 03:59:05.761: 172.22.0.254:47422 (ingress) <- 172.22.0.2:32066 (host) http-response FORWARDED (HTTP/1.1 503 5000ms (GET http://172.22.0.2:32066/drop))

$ kubectl -n kube-system exec pod/cilium-7c9w4 -c cilium-agent -- hubble status
Healthcheck (via unix:///var/run/cilium/hubble.sock): Ok
Current/Max Flows: 3,948/4,095 (96.41%)
```

環形緩衝區只有 4095 筆（`Max Flows`），而且正在用 96%，流量大時舊紀錄很快被覆蓋；每個 agent Pod 只有自己節點的紀錄。

結論（Task 1b 更新）：Hubble 在 agent Pod 內**可用**，A11 流量那一列的降級做法成立。以下是 Task 1 的原結論：A11 流量那一列「`hubble observe -n koupa -o json`」在主機上**做不到**；改成「在每個 `cilium` agent Pod 內執行 `hubble observe`」是**待確認**的降級做法（執行檔存在與否未驗證），且 L7 內容待確認。Hubble 在本課程是輔助來源，不是 A14 對帳的依據。

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

標記：**可做**、**降級做法**、**做不到（判定一律未知）**。Task 1b 已在 `tkha` 上實測的項目標「已實測」；**更正**的項目見 3.1、3.2。

### 表 A：A11 入口紀錄欄位

| 欄位或項目 | 結論 | 做法或原因 |
|---|---|---|
| 開始時間 | 可做 | `%START_TIME%` |
| 路徑 | 可做 | `%REQUEST_HEADER(X-ENVOY-ORIGINAL-PATH?:PATH)%` |
| HTTP 狀態 | 可做 | `%RESPONSE_CODE%` |
| 上游位址 | 可做 | `%UPSTREAM_HOST%`；上游嘗試次數大於 1 時只代表最後一次 |
| 回應旗標 | 可做 | `%RESPONSE_FLAGS%`（`UH/UF/UC/UT/URX/DC` 都在 Envoy 1.39.5 的表內） |
| 回應細節 | 可做 | `%RESPONSE_CODE_DETAILS%` |
| 上游嘗試次數 | 可做（已實測輸出 1、2、3），但只能分兩種：數字 ≥ 1 可信；**記錄到的 0 一律視為未知**，除非旗標為 `UH` 或 `NR` 才判為「沒有上游嘗試」 | `%UPSTREAM_REQUEST_ATTEMPT_COUNT%`；原始碼是 `value_or(0)`，「明確 0」和「沒設定」都印 0。「依格式規格省略」不會發生（HTTP access log 欄位一律有值）。「欄位沒設定／收集失敗」靠核對 log 格式與每個節點的 `cilium-envoy` log 是否完整判斷 |
| 總耗時 | 可做 | `%DURATION%` |
| `X-Koupa-Attempt` | 可做 | `%REQ(X-KOUPA-ATTEMPT)%` |
| 入口重試 | 預設不會重試，可保持關閉（已實測） | 只有 HTTPRoute 有 `retry` 才會；koupa 路由不寫 `retry`，上游嘗試次數最多 1 次（`/drop`：`attempts=1`）。若寫了 `retry.attempts: N`，上游嘗試次數最多 **N+1**（已實測：N=2 時 `attempts=3`） |
| 每次上游嘗試的紀錄 | 做不到 | access log 每個下游請求只有一行；Cilium Gateway 沒有逐次嘗試的設定。在不寫 `retry` 時嘗試次數不會大於 1，這個缺口不影響；若有人打開 `retry`，嘗試次數大於 1 的請求「入口選了哪個 Pod」判定一律未知 |
| 統計計數器 | 可做（已實測，見 3.5.4） | admin 的 `/stats`：`cilium-dbg envoy admin metrics`（Prometheus 格式，每個節點各自計數）。**實際名稱**（已實測）：`envoy_cluster_upstream_rq_total`、`envoy_cluster_upstream_rq_retry`、`envoy_cluster_upstream_rq_retry_limit_exceeded`、`envoy_cluster_upstream_rq_timeout`、`envoy_cluster_upstream_cx_destroy_remote_with_active_rq`、`envoy_cluster_upstream_rq_pending_failure_eject`；回應碼用標籤形式：`envoy_cluster_upstream_rq{envoy_response_code="503"}` 與 `envoy_cluster_upstream_rq_xx{envoy_response_code_class="5"}`（**沒有** `upstream_rq_5xx` 這個名稱）。cluster 名稱標籤 `envoy_cluster_name="<ns>/cilium-gateway-<gw>/<ns>_<svc>_<port>"`（`:` 換成 `_`） |
| 涵蓋所有入口實例 | 降級做法（已實測：請求只進被連到的那個節點的 Envoy，log 與統計都只在該節點） | 每個節點一個 `cilium-envoy`，要對全部節點同時收 log 與統計；少一個，該節點相關的判定標未知 |

### 表 B：A11 其他來源

| 來源 | 結論 | 備註 |
|---|---|---|
| Envoy 端點／設定更新紀錄（calibrate 的直接證據上界 U） | 待確認，傾向做不到（Task 1b：config/clusters admin 可看當下端點，但沒有更新歷史；縮到 0 後 Envoy 仍保留舊端點，見 3.3） | 需要 Envoy 對 EDS 更新的紀錄（debug log 或 agent 後端列表快照）。沒有的話，A11 已規定退回「模型推估」，並標成估計 |
| 干擾因素 | 要記錄 | outlier detection 預設開（3.1 末段） |
| Hubble | 主機上做不到；降級做法已實測可行 | 第 4 節；在 agent Pod 內執行 `hubble observe` 可用（`/usr/bin/hubble` v1.20.4），看得到 Gateway 的 `http-request`/`http-response`（方法、URL、狀態碼、耗時），但沒有上游位址、旗標、嘗試次數、自訂標頭；只是輔助用途 |
| API server audit log（交叉核對刪除請求時間） | **已實測：可用** | 見 3.6。kube-apiserver 有開 audit，log 在控制平面節點 `/opt/taroko/logs/apiserver/audit.log`（root 權限，需 `sudo podman exec`）；policy 對 pods/services/secrets/configmaps 與 apps/batch 工作負載的 `get/list/watch/create/update/patch/delete` 記到 `Request` 層級（不含 `RequestReceived` 階段），pod 刪除請求實測有紀錄（時間戳為奈秒、UTC）。注意 policy 沒涵蓋 `endpointslices`、`events`，所以端點移除不會出現在 audit log |

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

1. A11「入口預設會不會重試」：改為「預設不會；HTTPRoute 不寫 `retry` 就是關閉（**已用 tkha 即時 config dump 與 access log 實測**）。若寫了 `retry.attempts: N`，上游嘗試次數最多 N+1」。
2. A11「上游嘗試次數要分清楚三種情況」：改為「HTTP access log 的 `%UPSTREAM_REQUEST_ATTEMPT_COUNT%` 在沒設定時也印 0（Envoy 1.39.5 原始碼 `value_or(0)`），所以只區分『印出數字』與『整行缺漏』」。
3. A11 流量那一列：主機上沒有 `hubble`；**已實測** agent Pod 內有 `/usr/bin/hubble`（v1.20.4），改成「在每個 `cilium` agent Pod 內執行 `hubble observe`（`--type l7` 看 HTTP）」，只當輔助來源，不含上游位址與嘗試次數。
4. 4.3 的長連線逾時：**更正（已實測）**：預設**沒有** 15 秒 route timeout（Cilium 產生的 `max_stream_duration: 0s` 讓 Envoy 不採用預設值）；沒寫 `timeouts` 的 SSE 連續 100 秒未被切斷。會切斷的只有 (a) 明確寫的 `timeouts.request`（`flags=UT`），(b) stream idle 300 秒（config 已確認，行為待測）。4.3 與 06-reference 的「15 秒」敘述要刪掉。
5. 補充：HTTPRoute `retry.attempts` 的意義是重試次數（N 次重試 = 最多 N+1 次上游嘗試）；access log 要開，必須用專屬 GatewayClass 掛 `CiliumGatewayClassConfig`（Gateway 層級的 `infrastructure.parametersRef` 不支援）；Gateway 的 LoadBalancer IP 在 tkha 上從主機不一定連得到（見 7.1），測試時用 `<node-ip>:<nodePort>`。

## 7. 待確認清單（Task 1b 實測結果）

| # | 項目 | 狀態 | 結果 |
|---|---|---|---|
| 1 | 叢集實際 Kubernetes、CRI-O、Cilium、Envoy 版本 | **已實測** | 1.37.2／1.37.2／1.20.4／1.39.5，Gateway API v1.6.3 experimental，與原始碼調查所用版本相同（1.4） |
| 2 | `cilium-envoy` DaemonSet 存在、admin socket 可用 | **已實測** | 有 4 個 Pod；`cilium-dbg envoy admin config/clusters/listeners/metrics/serverinfo` 可用，agent 容器無 curl（2.1） |
| 3 | 路由實際的 route timeout、retry、stream idle、max stream duration | **已實測（有更正）** | route timeout 預設無逾時（非 15 秒）；retry 預設無；`attempts: N` = N 次重試；stream idle 300 秒 config 已確認但**行為未測**；connect timeout 實際 5 秒（3.1、3.2） |
| 4 | `CiliumGatewayClassConfig.spec.telemetry` 可用、四個欄位實際輸出 | **已實測** | 可用，但只能經 GatewayClass `parametersRef`；四個欄位都有輸出（2.2） |
| 5 | `/stats` 的 cluster 名稱與計數器名稱 | **已實測** | 見表 A「統計計數器」列與 3.5.4 |
| 5b | 自訂標頭穿過 Gateway 到上游、API server audit log | **已實測** | 標頭保留、audit 可用（3.6） |
| 6 | Hubble 是否啟用、agent 內有無 `hubble` 執行檔 | **已實測** | 啟用；`/usr/bin/hubble` v1.20.4 可用；看得到 Gateway HTTP（第 4 節） |
| 7 | terminating 端點在 Envoy EDS 的處理 | **待確認** | 需要 calibrate 實驗；縮到 0 後 Envoy 仍保留舊端點（3.3） |
| 8 | CNPG 接受 `tag@digest`，且 `SHOW transaction_timeout` 可用 | **待確認** | 屬於建 Cluster 時才能驗，不在 Task 1b 範圍 |
| 9（新增） | stream idle 300 秒的實際切斷行為與回應旗標 | **待確認** | 要一條靜默超過 300 秒的連線；預期旗標 `SI`，未驗證 |
| 10（新增） | `retry` 沒寫 `attempts` 時的預設重試次數 | **待確認** | 本次只測了 `attempts: 2` |
| 11（新增） | `%UPSTREAM_REQUEST_ATTEMPT_COUNT%` 的 0 | **待確認** | 本次所有請求都有上游嘗試；規則：記錄到的 0 視為未知，除非旗標為 `UH` 或 `NR`（2.3） |

### 7.1 LoadBalancer IP 在主機上不一定連得到（測試過程的發現）

`tkha` 的 Cilium 開了 L2 announcement（`enable-l2-announcements: "true"`，有一個 `CiliumL2AnnouncementPolicy default-l2`），Gateway 拿到 LB IP `172.22.0.201`。但本次新建的 Gateway：`kubectl get leases -n kube-system | grep l2announce` 沒有對應的 lease，主機對 `172.22.0.201` 的 ARP 是 `FAILED`／`INCOMPLETE`，`curl` 出現 `No route to host`。唯一一次成功是靠主機 ARP 快取裡前一個 Gateway（`tk-verify`）留下的舊項目。**改用節點 IP 加 NodePort 可以**：

```console
$ kubectl -n envcheck get svc cilium-gateway-gw     # 80:32066/TCP
$ for n in 1 2 3 4; do curl -s -m3 -o /dev/null -w "172.22.0.$n:32066 %{http_code}\n" http://172.22.0.$n:32066/hostname; done
172.22.0.1:32066 200
172.22.0.2:32066 200
172.22.0.3:32066 200
172.22.0.4:32066 200
```

原因沒有深究（Gateway 的 Service 只有一個假端點 `192.192.192.192`，且 `externalTrafficPolicy: Local`，可能和 L2 leader 選舉有關），所以 koupa 的量測腳本不要依賴主機直連 LB IP，用 `<節點 IP>:<NodePort>` 或叢集內的 Pod 當流量來源。這點和「涵蓋所有入口實例」有關：請求打哪個節點，就只有那個節點的 Envoy 有 log。

### 7.2 測試資源與清理（Task 1b）

建立的物件：namespace `envcheck`（內含 `echo`、`sse` 兩個 Deployment 與 Service、`CiliumGatewayClassConfig envcheck-cfg`、Gateway `gw`、HTTPRoute `r`、`r2`）；叢集範圍的只有 GatewayClass `envcheck-cilium`。沒有修改任何 `kube-system` 物件、Cilium Helm 值或內建 `cilium` GatewayClass。

```console
$ kubectl delete gatewayclass envcheck-cilium
gatewayclass.gateway.networking.k8s.io "envcheck-cilium" deleted
$ kubectl delete ns envcheck --wait=true
namespace "envcheck" deleted

$ kubectl get ns envcheck
Error from server (NotFound): namespaces "envcheck" not found
$ kubectl get gatewayclass
NAME     CONTROLLER                     ACCEPTED   AGE
cilium   io.cilium/gateway-controller   True       15m
$ kubectl get gateway,httproute,ciliumgatewayclassconfigs.cilium.io -A
No resources found
$ kubectl get leases -n kube-system | grep -c envcheck
0
```

**第二輪（Task 1b 修正補充）**：為了補測 audit 與標頭保留，同一份設定重建過一次 `envcheck`，測完再次刪除，驗證輸出如下：

```console
$ kubectl delete gatewayclass envcheck-cilium
gatewayclass.gateway.networking.k8s.io "envcheck-cilium" deleted
$ kubectl delete ns envcheck --wait=true --timeout=120s
namespace "envcheck" deleted
$ kubectl get ns envcheck
Error from server (NotFound): namespaces "envcheck" not found
$ kubectl get gatewayclass
NAME     CONTROLLER                     ACCEPTED   AGE
cilium   io.cilium/gateway-controller   True       23m
$ kubectl get gateway,httproute,ciliumgatewayclassconfigs.cilium.io -A
No resources found
$ kubectl get leases -n kube-system | grep -c envcheck
0
$ kubectl get svc -A | grep -c envcheck
0
```

清理後 Envoy 的 listener `envcheck/cilium-gateway-gw/listener` 仍以 `draining_state` 留在 `cilium-dbg envoy admin config` 裡一段時間（Envoy 的 listener 排空，不是殘留的 Kubernetes 物件；先前別人的 `tk-verify` Gateway 也是同樣情形）。

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
| 預設沒有 15 秒 route timeout 的原因 | [`config_impl.cc#L553`](https://github.com/envoyproxy/envoy/blob/v1.39.5/source/common/router/config_impl.cc#L553)（`using_new_timeouts_ = route.route().has_max_stream_duration()`）；[`router.cc#L191`](https://github.com/envoyproxy/envoy/blob/v1.39.5/source/common/router/router.cc#L191)（`finalTimeout` 只在 `!usingNewTimeouts()` 時用 `route.timeout()`）。以及 Task 1b 的即時實測（3.2） |
