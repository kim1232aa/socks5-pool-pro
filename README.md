# SOCKS5 Pool Pro

一个本地优先、可自愈的 SOCKS5 入口与多协议上游代理池。项目会抓取多个订阅源，验证真实连通性与出口信息，保留暂时失效的历史节点等待恢复，并通过一个固定的本地 SOCKS5 地址完成自动选路、故障切换和分流。

项目同时维护两类完全不同的数据：

- **可转发代理**：SOCKS5、HTTP、HTTPS 标签的 HTTP CONNECT 节点，经过真实健康检查后才能进入路由池。
- **ProxyIP 资源**：Cloudflare Worker/VLESS/Trojan 类脚本使用的外部反代跳板，只进入候选目录，不会被误当成 SOCKS5/HTTP 上游。

> `/api/status` 是现有注册服务依赖的兼容协议。它会继续返回 `active_proxy`、`available_total` 和仅包含健康节点的 `proxies`；每项只有 `proxy_url`，SOCKS5 节点额外有 `socks_url`，不会返回 `telegram_url`。

## 主要能力

- 多订阅源并发抓取，支持文本、EDT JSON、ProxyIP JSON、纯地址列表和 JSON 字符串数组。
- 协议感知去重；同一 `IP:port` 的 HTTP 与 SOCKS5 不会共享错误状态。
- 有界、轮转式候选检测；几十万条来源数据不会一次性全部探测，也不会因为没抽到而被删除。
- 已知节点三次连续后台失败后才暂时下线，下一次成功会自动恢复。
- 真实出口 IP、国家/城市、匿名性、延迟、成功/失败次数及手动测速。
- `sticky`、`round-robin`、`random`、`latency`、`speed`、`score` 六种分组策略。
- DOMAIN、DOMAIN-SUFFIX、DOMAIN-KEYWORD、IP-CIDR、GEOSITE、MATCH 分流规则及 DIRECT 直连。
- 全新的 **Proxy Atlas** 管理界面：桌面侧栏、移动底栏、服务端分页、快照翻页、移动摘要详情、完整加载/错误/空状态。
- 可追踪的异步刷新任务、稳定 API 错误码、请求 ID、CSRF/SSRF 防护及长任务并发限制。
- 仅使用 Go 标准库；运行镜像内主进程为非 root 用户。

## 快速开始

推荐使用仓库提供的 Compose 配置。两个端口默认都只绑定宿主机 `127.0.0.1`，数据保存在命名卷 `socks5-pool-pro-data`。

```bash
cp .env.example .env
docker compose up -d --build
docker compose ps
```

打开管理界面：

```text
http://127.0.0.1:8080/
```

检查服务：

```bash
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/readyz
curl --proxy socks5h://127.0.0.1:1080 https://api.ipify.org
```

首次启动时 `/healthz` 会立即返回 `200`；候选目录第一次发布前 `/readyz` 返回 `503`，发布后返回 `200`。抓取和健康检查在后台继续进行，不会阻塞管理页面启动。

### 不使用 Docker

建议使用当前受支持的 Go 1.26.5；`go.mod` 保留 Go 1.23 语言基线：

```bash
go build -o socks5-pool .
./socks5-pool \
  -listen 127.0.0.1:1080 \
  -status 127.0.0.1:8080 \
  -data-dir ./data
```

## Proxy Atlas 管理界面

界面按任务而不是按内部结构组织为五个区域：

1. **代理池**：当前出口、健康统计、真实出口国家、延迟/速度、手动切换、复检和测速。
2. **候选目录**：服务端浏览完整抓取清单，包括待检测、检测失败、策略过滤、已知可用/不可用和 ProxyIP 资源。
3. **订阅来源**：新增、启停和删除来源；可信局域网来源可以显式开启 `allow_private`。
4. **分流规则**：编辑规则顺序和 MATCH 默认目标。
5. **分组策略**：按国家、协议、来源或固定节点建立独立策略组。

桌面端使用固定侧栏和数据表格；移动端使用底部导航，每页默认 10 条，节点与候选只展示摘要，按需展开详情。危险的“清理不可用节点”与普通筛选、导出操作分开显示。

## 节点、候选与计数

这些数字故意不相等：

| 名称 | API 字段 | 含义 |
|---|---|---|
| 来源原始量 | `scrape.raw` | 本轮成功来源返回的原始记录数 |
| 去重候选 | `scrape.candidates` | 本轮成功来源中去重后的协议+地址组合 |
| 完整候选目录 | `candidate_total` | 当前目录总量；来源失败时会保留该来源上次成功的数据 |
| 本轮检测 | `scrape.checked` | 受 `-max-candidates` 限制、实际探测的子集 |
| 本轮通过 | `scrape.fresh_alive` | 本轮检测后通过的转发节点 |
| 已知池 | `total` | 跨刷新保留的可转发节点库存 |
| 当前可用 | `available_total` | 现在允许参与路由的健康节点 |
| ProxyIP | `proxyip_total` | 仅供 Worker 类配置使用的资源数量，不参与 SOCKS 路由 |

当某些来源失败时，候选目录状态会是 `partial`。失败来源的旧目录被保留，因此 `scrape.candidates` 可能小于 `candidate_total`；这表示本轮抓取不完整，不表示节点被删除。

### 有界候选检测

大型公开源可能一次返回数十万条记录。项目只从中选取最多 `-max-candidates` 条进行当前轮探测，选择方式是：

- 未见过的节点优先；
- 来源和协议之间尽量平衡；
- 未检测部分通过持久化游标在后续刷新继续轮转；
- 旧的不可用节点也通过独立游标获得恢复机会。

完整候选仍可在“候选目录”中分页查看，浏览器不会一次下载全部数据。

## Cloudflare ProxyIP 的正确含义

内置 `zip.cm.edu.kg/all.json` 数据源中的地址是 Worker/VLESS/Trojan 类隧道所使用的外部反代跳板，并不是普通 Cloudflare 边缘优选 IP，也不保证支持 SOCKS5 或 HTTP CONNECT。

因此项目采用以下隔离逻辑：

- 解析为 `protocol=proxyip`；
- 只进入候选目录和 ProxyIP 计数；
- 永不加入通用转发池；
- 只能通过候选行上的 ProxyIP 专用验证按钮，委托固定验证服务进行单条检查；
- 验证结果不会改变普通代理健康状态。

像 `socks5://user:pass@host:1080` 这样的带认证 SOCKS URL 仍是正常转发节点，与 ProxyIP 无关。

## 订阅源

内置来源包括 EDT-Pages、Proxifly、Monosans、Fyvri、socks5-proxy.github.io，以及默认关闭的 ProxyIP 来源。所有来源都可在界面中启停，配置持久化在数据卷的 `pool_config.json`。

支持的格式：

| `format` | 数据形态 | 是否需要 `protocol` |
|---|---|---|
| `text-regex` | 文本/HTML 中的完整代理 URL | 否 |
| `edt-json` | EDT 风格对象数组 | 否 |
| `proxyip-json` | `data[].ip/port/meta` 资源结构 | 否 |
| `plain-list` | 每行一个 `host:port` | 是 |
| `json-array` | `host:port` 字符串数组 | 是 |

新增普通来源：

```bash
curl -X POST http://127.0.0.1:8080/api/sources \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "my-socks-feed",
    "url": "https://example.com/socks5.txt",
    "format": "plain-list",
    "protocol": "socks5"
  }'
```

默认拒绝指向 loopback、私网、link-local、CGNAT、保留地址或公私混合 DNS 结果的来源 URL，并在每次重定向和真实拨号前重新校验。确实需要可信局域网订阅时必须显式声明：

```json
{
  "name": "trusted-lan-feed",
  "url": "http://192.168.1.10/proxies.txt",
  "format": "plain-list",
  "protocol": "socks5",
  "allow_private": true
}
```

`allow_private` 只放行订阅文件地址，不会自动信任、删除或改写文件中声明的代理节点。不要对不受你控制的地址启用它。

来源 URL 可以包含私有订阅所需的 userinfo 或查询参数。管理响应和日志会脱敏全部 userinfo、查询值和 fragment；持久化配置文件权限为 `0600`。

## HTTP API

所有 JSON API 响应都带有：

- `X-Request-ID`
- `Cache-Control: no-store, private`
- `X-Content-Type-Options: nosniff`
- 点击劫持与 Referrer 防护头

错误结构保留旧的 `error` 字段，同时提供稳定机器码：

```json
{
  "error": "country must be a two-letter ASCII ISO code or __unknown__",
  "code": "invalid_country",
  "request_id": "7e5f..."
}
```

错误方法返回 `405` 和 `Allow`；未知 `/api/*` 返回 JSON `404`，不会再错误返回管理首页。
管理写接口会拒绝未知 JSON 字段，字段拼写错误不会再被静默忽略。

### 兼容提取接口

```text
GET /api/status
GET /api/status?compact=1
```

完整接口用于现有代理提取客户端：

```json
{
  "active_proxy": "socks5://user:password@203.0.113.10:1080",
  "available_total": 2,
  "proxies": [
    {
      "proxy_url": "socks5://user:password@203.0.113.10:1080",
      "socks_url": "socks5://user:password@203.0.113.10:1080"
    },
    {
      "proxy_url": "http://198.51.100.20:8080"
    }
  ]
}
```

兼容保证：

- `active_proxy` 即使空池也存在，值为 `""`；
- `available_total == len(proxies)`，核心字段来自同一次池快照；
- `proxies` 只包含健康的 SOCKS5/HTTP/HTTPS 转发节点；
- 每项始终有 `proxy_url`；SOCKS5 额外有 `socks_url`；
- 不返回 `telegram_url`；
- `compact=1` 只返回计数和状态，不返回完整代理数组，适合界面轮询。

URL 中可能包含上游节点凭据，不要把完整状态响应发布到公共位置。

### v1 健康代理接口

新客户端应优先使用分页接口：

```text
GET /api/v1/proxies?page=1&page_size=20
GET /api/v1/proxies?protocol=socks5&country=JP&page_size=50
GET /api/v1/proxies/pick?protocol=socks5&country=JP
```

`/api/v1/proxies` 返回按稳定 key 排序的健康节点、`snapshot_id`、`page_count`、`has_next`、`filtered_total` 和 `available_total`。`page_size` 最大为 100；无效页码、协议或国家返回 `400`。

`/api/v1/proxies/pick` 从符合过滤条件的节点中按内部质量评分挑选一个；无匹配节点返回 `404 proxy_not_found`。

### 快照分页

节点页、候选页和 v1 代理页都会返回不透明的 `snapshot_id`。连续翻页时把它带回服务端：

```text
GET /api/candidates/page?page=2&page_size=50&snapshot_id=<上一页返回值>
```

当目录或影响结果的健康状态改变时，服务端返回：

```text
409 snapshot_changed
```

客户端应清除旧 token 并从第 1 页重新请求。token 包含当前进程标识，服务重启后旧 token 不会被错误接受。普通真实连接产生的成功计数不会无意义地使候选目录 token 失效。

### 异步刷新

```text
POST /api/refresh
GET  /api/refresh/status
```

触发接口返回 `202 Accepted`：

```json
{
  "id": "refresh-...",
  "status": "queued",
  "requested_at": "2026-07-11T09:00:00Z",
  "accepted": true,
  "coalesced": false,
  "status_url": "/api/refresh/status"
}
```

重复请求只保留一个待执行任务，并通过 `coalesced` 告知调用方。状态接口返回 `state=idle|queued|running`，以及当前、待执行和上一个任务；结束状态可能是 `complete`、`partial` 或 `skipped`。

### 管理接口索引

| 方法 | 路径 | 用途 |
|---|---|---|
| GET | `/healthz` | 数据无关的 liveness |
| GET | `/readyz` | 首次候选目录是否已发布 |
| GET | `/api/status` | 兼容状态与健康代理数组 |
| GET | `/api/v1/proxies` | 健康代理分页 |
| GET | `/api/v1/proxies/pick` | 选择一个健康代理 |
| POST | `/api/refresh` | 排队刷新任务 |
| GET | `/api/refresh/status` | 刷新任务状态 |
| GET | `/api/nodes/page` | 已知转发池分页、筛选、排序 |
| GET | `/api/candidates/page` | 完整候选目录分页 |
| GET | `/api/nodes` | 旧版完整节点数组；大池场景不推荐 |
| GET | `/api/nodes/export` | CSV 或 `format=tme` 导出 |
| POST | `/api/nodes/switch` | 固定 ANY 当前节点 |
| POST | `/api/nodes/auto` | 恢复 ANY 自动选择 |
| POST | `/api/nodes/verify` | 立即复检一个节点 |
| POST | `/api/nodes/speedtest` | 对一个节点执行完整下载测速 |
| POST | `/api/nodes/clear-unavailable` | 显式删除当前不可用节点 |
| POST | `/api/proxyip/verify` | 单条 ProxyIP 专用验证 |
| GET/POST | `/api/settings/check-url` | 读取或修改健康检查目标 |
| GET/POST | `/api/sources` | 列出或新增来源 |
| POST | `/api/sources/toggle` | 启停来源 |
| POST | `/api/sources/delete` | 删除来源 |
| GET/POST | `/api/rules` | 列出或新增规则 |
| POST | `/api/rules/delete` | 删除规则 |
| POST | `/api/rules/move` | 调整规则顺序 |
| POST | `/api/rules/default` | 修改 MATCH 默认组 |
| POST | `/api/rules/preset-gfw` | 写入 GFW 预设 |
| GET/POST | `/api/groups` | 列出或新增分组 |
| POST | `/api/groups/strategy` | 修改分组策略 |
| POST | `/api/groups/delete` | 删除分组 |

手动节点复检最多同时运行 4 个任务，ProxyIP 验证最多 8 个不同任务，测速最多 4 个任务；同节点重复任务会合并或返回 `429`，满载响应带 `Retry-After`。

## 安全模型

### 默认本地模式

未配置管理认证时，管理首页和全部 `/api/*` 只接受 `Host: localhost` 或 loopback IP；`/healthz` 和 `/readyz` 仍保持数据无关、无需认证。Compose 同时只把端口发布到宿主机 `127.0.0.1`，并使用独立 Docker 网络。

这意味着：

- `http://127.0.0.1:8080/api/status` 的现有本机提取方式保持可用；
- 浏览器 DNS rebinding 使用任意域名访问本地管理面会被拒绝；
- 另一个容器通过服务名、容器 IP 或 `host.docker.internal` 访问未认证管理面会得到 `403 untrusted_host`；
- 容器间调用必须先启用管理认证，再使用 Basic Auth。

### 管理认证

在 `.env` 中同时设置：

```dotenv
ADMIN_USER=operator
ADMIN_PASS=replace-with-a-long-random-password
```

启用后，管理首页和每个 `/api/*` 都需要 HTTP Basic Auth：

```bash
curl -u operator:password http://127.0.0.1:8080/api/status
```

启用认证前先更新所有机器提取客户端，否则它们会收到 `401`。SOCKS5 入站认证与管理认证彼此独立。

浏览器写请求还会检查 `Origin` 和 `Sec-Fetch-Site`；项目不会发送通配 CORS 头。

### SOCKS5 入站认证

```dotenv
SOCKS_USER=proxy-client
SOCKS_PASS=replace-with-another-password
```

然后客户端使用 RFC 1929 用户名密码连接本地 1080 端口。两项必须一起设置或一起留空。

### Secret file

除了直接环境变量，还支持：

- `SOCKS_USER_FILE`
- `SOCKS_PASS_FILE`
- `ADMIN_USER_FILE`
- `ADMIN_PASS_FILE`

这适合 Docker/Kubernetes secret，避免凭据出现在进程参数或普通环境配置中。直接值与对应 `_FILE` 不能同时设置；显式 CLI 参数优先于环境默认值。

## 配置参数

| CLI | 默认值 | 说明 |
|---|---:|---|
| `-listen` | `127.0.0.1:1080` | 本地 SOCKS5 监听地址 |
| `-status` | `127.0.0.1:8080` | 管理页面/API 监听地址 |
| `-data-dir` | `./data` | 持久化目录 |
| `-scrape-interval` | `20m` | 来源刷新周期 |
| `-check-timeout` | `10s` | 单节点健康检查超时 |
| `-max-concurrent` | `20` | 健康检查并发数 |
| `-max-candidates` | `3000` | 每轮最多检测的候选数 |
| `-require-ip-change` | `true` | 排除已确认没有改变出口 IP 的透明代理 |
| `-socks-user/-socks-pass` | 空 | SOCKS5 入站认证 |
| `-admin-user/-admin-pass` | 空 | 管理页面和 API Basic Auth |

当平台提供 `PORT` 环境变量且没有显式传 `-status` 时，HTTP 管理服务监听 `0.0.0.0:$PORT`；SOCKS5 仍使用 1080。

## Docker 运维

### 更新

```bash
git pull --ff-only
docker compose up -d --build
docker compose ps
```

命名卷不会因重新构建或普通 `docker compose down` 被删除。不要使用 `down -v`，除非你确实要清空节点池、来源和规则。

### 备份数据卷

```bash
mkdir -p backups
docker run --rm \
  -v socks5-pool-pro-data:/data:ro \
  -v "$PWD/backups:/backup" \
  alpine:3.24.1 \
  tar -czf /backup/socks5-pool-data.tar.gz -C /data .
```

### 直接 `docker run`

```bash
docker build -t socks5-pool-pro .
docker network create socks5-pool-pro-net
docker run -d --name socks5-pool-pro --restart unless-stopped \
  --network socks5-pool-pro-net \
  -p 127.0.0.1:1080:1080 \
  -p 127.0.0.1:8080:8080 \
  -v socks5-pool-pro-data:/app/data \
  socks5-pool-pro
```

镜像入口只以 root 修复 `/app/data` 的历史权限，随后通过 `su-exec` 以 UID/GID 10001 运行服务。Compose 根文件系统为只读，只有数据卷和临时目录可写。

## Railway

仓库保留 `railway.toml`。Railway 的 `PORT` 主要用于 HTTP 管理页面与健康检查；普通 HTTP 公网入口不会自动暴露 SOCKS5 TCP 1080。如需公开 SOCKS5，应使用平台 TCP 入口、私网服务或 VPS。

公开部署必须设置 `ADMIN_USER/ADMIN_PASS`，并为管理入口提供 TLS。若还要公开 SOCKS5，同时设置独立的 `SOCKS_USER/SOCKS_PASS`。

## 排查指南

### 页面/API 返回 `403 untrusted_host`

当前没有启用管理认证，并且请求 Host 不是 localhost/loopback。使用宿主机发布的 `http://127.0.0.1:8080`；如果确实需要从反向代理、局域网或另一个容器访问，请启用 `ADMIN_USER/ADMIN_PASS`。

### 翻页返回 `409 snapshot_changed`

目录或影响筛选结果的健康状态在翻页期间改变。清除旧 `snapshot_id`，重新请求第 1 页。Proxy Atlas 会自动完成该恢复。

### 操作返回 `429`

测速、手动复检或 ProxyIP 验证达到并发上限。尊重 `Retry-After` 后重试，不要并发轰炸同一个节点。

### 候选很多，可用节点很少

候选只是来源声明的地址；只有实际通过当前 CheckURL 的节点才能成为可用代理。免费公开列表中大量地址过期是正常现象。

### 某个国家显示总数很多但可用数为 0

总数来自完整候选目录或已知库存，可用数来自当前真实健康状态。国家筛选不会把未验证或失败节点伪装成可用节点。

### `partial` 或来源错误

部分来源本轮超时/失败，成功来源已更新，失败来源上一次目录仍保留。下一轮会自动重试。

### 测速没有结果

测速会验证完整响应、拒绝重定向和不完整下载，并受 42 秒左右的任务预算约束。节点只通过轻量健康检查并不代表它能稳定完成测速文件下载。

## 开发与验证

本机有 Go：

```bash
gofmt -w *.go
go test ./...
go vet ./...
go test -race ./...
```

只使用 Docker：

```bash
docker run --rm -v "$PWD:/src" -w /src golang:1.26.5-alpine3.24 go test ./...
docker run --rm -v "$PWD:/src" -w /src golang:1.26.5-alpine3.24 go vet ./...
docker run --rm -v "$PWD:/src" -w /src golang:1.26.5-alpine3.24 \
  sh -c 'apk add --no-cache gcc musl-dev >/dev/null && CGO_ENABLED=1 go test -race ./...'
docker build -t socks5-pool-pro:test .
```

测试覆盖协议解析、SOCKS/HTTP 隧道、健康失败防抖、缓存权限、候选采样、分页快照、API 合同、认证/CSRF、来源 SSRF、ProxyIP 验证、测速、CSV 注入和响应式界面合同。

## 项目结构

```text
main.go                 抓取、检查、刷新任务与后台调度
server.go               入站 SOCKS5 服务和分流
dial.go                 SOCKS5/HTTP CONNECT 上游拨号
pool.go                 节点池、健康统计、策略和持久化调度
candidate_catalog.go    大候选目录、来源保留和快照分页
parser.go               来源抓取、SSRF 防护与格式解析
checker.go              健康检查、出口 IP 与地理信息
speedtest.go            严格按需测速
manual_verify.go        手动节点复检
proxyip_verify.go       ProxyIP 专用验证
sourcestore.go          来源/规则/分组配置
status.go               HTTP API、中间件与响应模型
dashboard_html.go       Proxy Atlas 单文件管理前端
compose.yaml            推荐本地部署
```

## 已知边界

- 免费来源质量和可用率会随时间剧烈变化，项目只能真实检测，不能让失效代理重新工作。
- 未自带国家信息的节点依赖第三方地理查询；被限流时显示未知国家，但不会因此直接判定代理不可用。
- `speed` 策略只有在节点完成过手动测速后才有可靠数据。
- 旧 `/api/nodes` 会返回完整数组，大池场景应使用 `/api/nodes/page` 或 `/api/v1/proxies`。
- 管理 API 中的代理 URL 可能携带上游凭据；即使有 `no-store`，也不应公开转发响应内容。
