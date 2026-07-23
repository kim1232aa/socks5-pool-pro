# SOCKS5 Pool Pro

SOCKS5 Pool Pro 是一个本地优先的多来源代理池：后台抓取公开或自定义订阅，保留完整候选目录，对其中一部分做真实连通性检测，再通过固定的本地 SOCKS5 入口完成选路、切换和分流。

它刻意把“来源里有多少地址”和“现在有多少可用代理”分开。公开列表中的地址不会因为被抓到就自动进入路由；只有 SOCKS5、HTTP、HTTPS 节点通过当前健康标准后才会成为可用上游。Cloudflare ProxyIP 则是另一类资源，始终与通用代理池隔离。

> `/api/status` 保留现有提取客户端依赖的兼容合同：顶层继续包含 `active_proxy`、`available_total` 和 `proxies`。`proxies` 只含当前健康节点，每项有 `proxy_url`，SOCKS5 节点额外有 `socks_url`。状态 API **不会返回 `telegram_url`**。

> `/api/status` 是现有注册服务依赖的兼容协议。它会继续返回 `active_proxy`、`available_total` 和仅包含健康节点的 `proxies`；每项包含带认证的 `proxy_url`、原始 `username`、`password`，SOCKS5 节点额外有 `socks_url`，不会返回 `telegram_url`。

## 架构与数据边界

```text
订阅来源
   │ 抓取、格式校验、协议感知去重
   ▼
完整候选目录 ── 有界轮转抽样 ── 真实 HTTP 健康检查 ── 已知转发池
   │                                                   │
   │ ProxyIP 仅浏览/专用验证                           │ 规则 + 分组策略
   └── 不进入 SOCKS/HTTP 路由                          ▼
                                               本地 SOCKS5 :1080
```

项目维护三个不同层次的数据：

| 层次 | 包含内容 | 是否可路由 | 持久化 |
|---|---|---:|---:|
| 候选目录 | 来源声明的全部去重记录，以及待检、失败、策略排除等状态 | 否 | `candidate_catalog.v1.bin.gz` |
| 已知转发池 | 曾成功通过检查的 SOCKS5/HTTP/HTTPS 节点；失败节点保留为不可用 | 是，优先当前健康节点 | `pool_cache.json`（gzip） |
| ProxyIP 资源 | Cloudflare Worker/VLESS/Trojan 类工具使用的外部反代地址 | 否 | 候选目录 |

配置和抽样游标分别保存在 `pool_config.json` 与 `candidate_sampler.json`。`pool_cache.json` 和候选目录缓存均使用 gzip 压缩写入。数据目录和状态文件使用私有权限；转发池及候选目录缓存会原样保留上游用户名和密码，备份应按敏感数据处理。

### 为什么界面里的数字不同

| 字段 | 含义 |
|---|---|
| `scrape.raw` | 本轮成功下载的原始记录数 |
| `scrape.candidates` | 本轮成功来源去重后的候选数 |
| `candidate_total` | 当前完整候选目录总数；失败来源可沿用上次成功目录 |
| `scrape.checked` | 本轮实际执行健康检查的有界子集 |
| `scrape.fresh_alive` | 本轮子集中通过健康检查的节点 |
| `total` | 跨刷新保留的已知转发节点数 |
| `available_total` | 当前健康且可由提取 API 返回的节点数 |
| `proxyip_total` | 非路由 ProxyIP 资源数 |

大型来源可能有数十万条记录。`-max-candidates` 只限制每轮检测量，不会把未抽到的候选从目录删除。抽样会优先未见节点，并在来源与协议之间轮转；已知不可用节点也会在后续复检中获得恢复机会。

## Cloudflare ProxyIP：只取纯 IP，固定 443

本项目的 `proxyip-json` 逻辑与普通 SOCKS/HTTP 订阅不是一回事：

- 只接受端口字段中包含 `443` 的记录；
- 目录中规范化为 `proxyip://IP:443`；
- 实际配置 ProxyIP 时只取纯 IP；
- 不对它执行通用 SOCKS5、HTTP CONNECT 健康检查；
- 不加入本地转发池，也不出现在 `/api/status.proxies`；
- 可在候选页调用固定的 ProxyIP 专用验证服务做单条参考检查，该结果不会改变普通代理状态。

下面两种数据表达的是不同概念：

```text
150.230.212.247
```

这是 ProxyIP 选择器使用的纯 IP。相反：

```text
8.220.201.169&port=1080&user=9&pass=9
```

这是带端口和凭据的 SOCKS/HTTP 链式代理参数，不是 ProxyIP。若该端点确实提供 SOCKS5，应在普通来源中写成标准代理 URL：

```text
socks5://9:9@8.220.201.169:1080
```

它随后会按普通转发节点进行真实健康检查。项目不会把链式参数误解析成 Cloudflare ProxyIP。

## 快速部署：Docker Compose

Compose 是推荐方式。默认把主 SOCKS5 端口 `1080`、附加端口范围 `1081-1180` 和管理端口 `8080` 发布到宿主机 `127.0.0.1`，数据保存在命名卷 `socks5-pool-pro-data`。

```bash
cp .env.example .env
# 按需编辑 .env；如要让其他人访问管理面，先设置 ADMIN_USER/ADMIN_PASS
docker compose up -d --build
docker compose ps
```

管理界面：

```text
http://127.0.0.1:8080/
```

基本检查：

```bash
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/readyz
curl http://127.0.0.1:8080/api/status?compact=1
curl --proxy socks5h://127.0.0.1:1080 https://api.ipify.org
```

- `/healthz` 只表示 HTTP 进程能响应，启动后应返回 `200`。
- `/readyz` 在首个候选快照发布前返回 `503`，之后返回 `200`。
- 第一次抓取和健康检查在后台运行；管理页面可以先启动。
- 如果设置了 `ADMIN_USER/ADMIN_PASS`，管理 API 的 `curl` 需要加 `-u 用户名:密码`。
- 如果设置了 `SOCKS_USER/SOCKS_PASS`，SOCKS 客户端也必须提供对应凭据。

### 直接运行二进制

`go.mod` 的语言基线是 Go 1.23；仓库 Dockerfile 当前使用 Go 1.26.5 构建。

```bash
go build -o socks5-pool .
./socks5-pool \
  -listen 127.0.0.1:1080 \
  -status 127.0.0.1:8080 \
  -data-dir ./data
```

## 管理界面

内置 Proxy Atlas 管理界面由 `web/dashboard.html`、`web/dashboard.css` 和 `web/dashboard.js` 组成，构建时嵌入二进制。主要页面包括：

- **转发代理池**：健康状态、实测出口、延迟、评分、测速、人工复检和节点切换；
- **候选库存**：服务端分页浏览完整去重目录，区分待检、失败、策略排除、池内可用/不可用和 ProxyIP；
- **来源订阅**：新增、启停、删除来源及 `allow_private`、`allow_empty` 高级选项；
- **分流规则**：按顺序维护域名、CIDR、GEOSITE 与兜底规则；
- **分组策略**：按国家、协议、来源或指定节点建立策略组；
- **监听端口**：热增删附加 SOCKS5 端口，每个端口可复用全局规则、按分组策略自动切换，或固定到一个协议感知节点 key。

节点和候选都使用服务端筛选、分页与快照令牌，浏览器不会一次下载完整大池。移动端与桌面端共用相同 API。

附加端口保存在 `pool_config.json`，重启后自动恢复。`rules` 模式对每条新连接使用当前全局规则；`group` 模式只在指定组内按该组策略选择和切换；`fixed` 模式只使用指定节点。`group` 和 `fixed` 都不会回退到 `ANY`，没有可用目标时直接返回 SOCKS5 失败。停用或删除端口会关闭对应监听器。

## 健康检查与节点生命周期

默认检查目标是：

```text
https://www.google.com/generate_204
```

健康判定规则：

- 必须通过候选代理完成真实 HTTP 请求；仅能建立 TCP 连接不算健康；
- 默认目标必须直接返回 `204`；
- 自定义目标接受直接返回的任意 `2xx`；
- 重定向和非 `2xx` 都失败；
- 出口 IP、地理信息和匿名性探测是尽力而为，附加探测失败不会抹掉已经通过的基础健康结果；
- `-require-ip-change=true` 时，只有在本机出口与代理出口都已测得且相同的节点才会被策略排除，未知状态不会被误删。
启动时会在第一次网络抓取之前校验并恢复该文件，因此管理页面可以先显示上次完整目录。缓存保留来源归属、候选状态、地区、时间、`has_auth` 标记以及上游用户名和密码，使来源级的“本轮失败则沿用上次成功目录”语义和完整连接信息跨进程重启继续成立。只有显式停用/删除来源、成功抓取到不同内容，或删除数据卷/缓存文件，目录才会按新事实变化。

候选目录 API、代理池 API、管理页面和导出会原样提供上游用户名、密码与带认证的 `proxy_url`。候选缓存也会写入这些凭据，缓存文件权限为 `0600`，并有压缩大小、解压大小、记录数和字符串长度限制；损坏或不兼容的文件会被拒绝，不会发布部分解码结果。启用管理接口认证后再将该接口暴露到非可信网络。

在界面或 `POST /api/settings/check-url` 修改目标后，旧标准下的所有健康结果立即失效，并触发保留池全量复检。检查结果带有健康标准代次；旧异步任务即使较晚完成，也不能把节点按旧标准重新标为可用。复检完成前这些节点属于“等待当前标准复检”，不会被路由选中，也不能执行永久清理；进度可从 `/api/health-recheck/status` 查询。重复保存同一个规范化 URL 返回 `changed:false`，不会让全池重复下线。

升级时，配置中精确等于历史默认值 `http://www.google.com/generate_204` 的目标会迁移到 HTTPS；用户自行设置的其他 HTTP 地址不会被改写。检查 URL 或出口变化策略改变都会使旧缓存标准失效，并安排复检。

新候选只有成功后才进入已知转发池。已知节点在普通后台检查中连续失败 3 次才标为不可用，下一次成功可以自愈；实际转发时若确认是上游连接、认证或协议故障，会立即停止选择该节点，而目标站自身拒绝连接不会污染全局节点健康。不可用节点默认只隐藏，不会自动删除；只有“永久清理不可用节点”操作会物理移除它们。

同一个协议与地址如果由来源声明了多组凭据，会保留有界的凭据候选，并在同一个节点超时预算内逐个尝试；验证成功的凭据会提升为当前主凭据。同一 `IP:port` 的 SOCKS5 与 HTTP 声明仍是两个独立节点，不共享错误状态。

### 测速不是健康检查

测速是按需操作，不会在每轮刷新中对全池自动下载。它完整读取固定 1 MiB 样本，拒绝重定向、非完整响应和异常 Range；主测速点失败后会在同一 18 秒总预算内尝试备用测速点。测速与人工验证分别允许最多 16 个不同节点并发执行，不进入隐藏队列；同一节点测速完成后冷却 5 分钟，人工验证完成后冷却 2 分钟。自动健康检测成功后 24 小时内不会重复检测；最终失败的节点立即从可路由池过滤，且不会再被自动复检，只有显式人工验证成功才能恢复。测速失败可能只是测速站链路问题，不会因此直接判定代理不健康。`speed` 策略只对已有有效测速样本的节点有参考意义。

## 来源、last-good 与空清单

支持的来源格式：

| `format` | 输入 | `protocol` |
|---|---|---|
| `text-regex` | 文本或 HTML 中的完整 `scheme://host:port` | 数据自带 |
| `edt-json` | EDT 风格对象数组 | 数据自带 |
| `proxyip-json` | `data[].ip/port/meta` | 固定为 `proxyip` |
| `plain-list` | 每行一个 `host:port` | 必须指定 `socks5`、`http` 或 `https` |
| `json-array` | `host:port` 字符串数组 | 必须指定 `socks5`、`http` 或 `https` |

初始配置包含 EDT-Pages、Proxifly、Monosans、Fyvri 和 socks5-proxy.github.io 等普通来源；内置 ProxyIP 来源默认关闭，可按需启用。

新增来源示例：

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

来源刷新采用 last-good 语义：

- 下载失败、解析失败或 HTTP 200 但没有有效记录时，默认把本轮视为失败；
- 候选目录会按来源保留该来源上一次成功快照，其他成功来源照常更新，刷新状态显示 `partial`；
- 只有显式设置 `allow_empty=true`，空清单才是“该来源确实为空”的权威结果；
- 停用或删除来源会立即让该来源曾声明过的转发节点保守退役，但不会自动清除已知转发库存、统计和池缓存；
- 同一端点的凭据候选目前不逐来源归属。为避免误用被停用来源的凭据，即使端点还出现在另一启用来源中，也会先退出路由；下一轮从启用来源重新抓取并通过健康检查后会解除退役；
- 后续候选快照按当前启用来源重建，停用来源的 last-good 不再参与新快照。

因此“来源停用/删除”和“永久清理不可用节点”是两件事。前者停止使用来源并保留历史库存，后者才是显式删除池内不可用记录。

### 私网来源与 SSRF 防护

默认拒绝来源 URL 指向 loopback、私网、link-local、CGNAT、保留地址以及公私混合 DNS 结果，并在重定向和实际拨号前重新校验。确实需要访问自己控制的局域网订阅时，可显式设置：

```json
{
  "name": "trusted-lan-feed",
  "url": "http://192.168.1.10/proxies.txt",
  "format": "plain-list",
  "protocol": "socks5",
  "allow_private": true
}
```

`allow_private` 只放行订阅文件地址，不会自动信任、删除或改写文件中声明的代理节点；不要对不受你控制的地址启用它。抓取到的字面 IP 必须是可公网路由地址；私网、CGNAT、环回、链路本地、文档、组播和保留网段会在进入候选目录前逐条过滤，不会让同一来源中的其他公网节点失效，域名形式的上游仍然支持。来源 URL 的 userinfo 和查询值会在管理响应与日志中脱敏，但原始值仍需写入私有配置文件才能完成抓取；URL fragment 会直接被拒绝。

单个来源响应体、解析记录数、字段长度、重试时间和并发抓取数都有硬边界。当前实现同时最多抓取 4 个来源，单个响应最多 16 MiB、最多解析 300,000 条记录，合并候选缓存最多 1,200,000 条。达到边界的来源按失败处理并沿用 last-good，不会发布半截结果。

## 路由与策略

本地入口只实现 SOCKS5 CONNECT。目标按规则从上到下匹配，再交给 `DIRECT`、`ANY`、`COUNTRY:XX` 或自定义分组：

- 规则：`DOMAIN`、`DOMAIN-SUFFIX`、`DOMAIN-KEYWORD`、`IP-CIDR`、`GEOSITE`、`MATCH`；
- 策略：`sticky`、`round-robin`、`random`、`latency`、`speed`、`score`；
- 自定义组可按国家、协议、来源或协议感知节点 key 筛选；
- `COUNTRY:JP` 这类动态组无需预先创建；
- 指定组没有普通健康成员时会回退到 `ANY`，连接同一目标时最多尝试不同上游 3 次；来源退役、健康标准失效和出口策略排除属于硬不可路由状态，不会通过回退重新选中。

来源标签 `https` 在本项目中表示能用 HTTP CONNECT 访问 HTTPS 目标的 HTTP 代理，并不表示客户端先用 TLS 连接代理本身；因此对外 `proxy_url` 可能规范化为 `http://...`。

## HTTP API

### 兼容提取接口

```text
GET /api/status
GET /api/status?compact=1
```

完整响应的核心合同：

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

保证如下：

- `active_proxy` 即使没有健康选择也保留，值为 `""`；
- `available_total == len(proxies)`，核心字段来自同一次池快照；
- `proxies` 只包含健康的 SOCKS5/HTTP/HTTPS 转发节点；
- 每项始终有带认证的 `proxy_url`、原始 `username`、`password`；SOCKS5 额外有 `socks_url`；
- 不返回 `telegram_url`；
- `compact=1` 省略大代理数组，增加候选总量、阶段、来源错误和更新时间，适合界面轮询。

代理 URL 可能带上游凭据。不要把完整 `/api/status` 暴露到公共缓存、日志或第三方页面。

### v1 健康代理接口

新客户端优先使用有界接口：

```text
GET /api/v1/proxies?page=1&page_size=20
GET /api/v1/proxies?protocol=socks5&country=JP&page_size=50
GET /api/v1/proxies/pick?protocol=socks5&country=JP
```

`/api/v1/proxies` 只返回健康节点，支持协议、国家、分页和不透明 `snapshot_id`，`page_size` 最大为 100。连续翻页时回传前一页的 `snapshot_id`；如果池已变化，服务返回 `409 snapshot_changed`，客户端应从第 1 页重新开始。

`/api/v1/proxies/pick` 从过滤结果中选出内部评分最高的节点；没有匹配时返回 `404 proxy_not_found`。v1 同样只有 `proxy_url`/`socks_url`，不提供 Telegram URL。

### 常用管理接口

| 方法 | 路径 | 作用 |
|---|---|---|
| GET | `/healthz` | 无数据 liveness |
| GET | `/readyz` | 首个候选快照是否发布 |
| GET | `/api/status` | 兼容状态与健康代理数组 |
| GET | `/api/v1/proxies` | 健康代理分页 |
| GET | `/api/v1/proxies/pick` | 选择一个健康代理 |
| POST | `/api/refresh` | 排队一次异步来源刷新 |
| GET | `/api/refresh/status` | 查看运行中、待执行和最近刷新任务 |
| GET | `/api/health-recheck/status` | 查看健康标准全量复检进度 |
| GET | `/api/nodes/page` | 已知转发池分页、筛选、排序 |
| GET | `/api/candidates/page` | 完整候选目录分页 |
| GET | `/api/nodes` | 旧版完整节点数组，已标记弃用 |
| POST | `/api/nodes/verify` | 人工复检一个转发节点 |
| POST | `/api/nodes/speedtest` | 对一个节点执行按需测速 |
| POST | `/api/proxyip/verify` | 单条 ProxyIP 专用验证 |
| GET/POST | `/api/settings/check-url` | 读取或修改健康目标 |
| GET/POST | `/api/sources` | 列出或新增来源 |
| GET/POST | `/api/rules` | 列出或新增规则 |
| GET/POST | `/api/groups` | 列出或新增分组 |
| GET/POST | `/api/listeners` | 列出运行状态或新增附加 SOCKS5 端口 |
| POST | `/api/listeners/update` | 热更新端口、模式、目标和启用状态 |
| POST | `/api/listeners/delete` | 删除配置并关闭对应监听器 |

`POST /api/refresh` 返回 `202` 和任务 ID；重复请求最多合并成一个待执行任务，避免无界队列。节点页、候选页与 v1 页都使用快照分页。未知 `/api/*` 返回 JSON `404`，错误响应包含稳定 `code`、可读 `error` 和 `request_id`。

## 管理面安全

### 未配置管理认证

没有设置 `ADMIN_USER/ADMIN_PASS` 时，管理首页和 `/api/*` 同时要求：

1. `Host` 是 `localhost` 或 loopback IP；
2. TCP 对端是 loopback，或是通过 `-trusted-management-proxy` 明确列出的**单个精确 IP**。

转发头不会被信任，`-trusted-management-proxy` 也不接受 CIDR 或主机名。Compose 把端口绑定到宿主机 `127.0.0.1`，并只信任固定 Docker 网桥网关 `172.30.250.1`，从而让宿主机经发布端口访问仍然可用；这不是对整个容器网段放行。

若修改 Compose 的网段或网关，必须同步修改命令中的 `-trusted-management-proxy`。若 `172.30.250.0/24` 与现有网络冲突，也应成对调整这两处。

`/healthz` 和 `/readyz` 始终无认证且不泄露池数据，便于编排器探测。

### Basic Auth 与跨站写保护

需要从非本机访问时，至少同时设置：

```dotenv
ADMIN_USER=operator
ADMIN_PASS=replace-with-a-long-random-password
```

启用后，页面和所有 `/api/*` 都需要 HTTP Basic Auth；请同时在外层反向代理启用 TLS。启用前先更新自动提取客户端，否则 `/api/status` 会返回 `401`。

浏览器状态修改请求还会检查 `Origin` 和 `Sec-Fetch-Site`，不开放通配 CORS。响应默认带 `no-store`、请求 ID、`nosniff`、禁止嵌套和 Referrer 防护头。

SOCKS 入站认证与管理认证互相独立：

```dotenv
SOCKS_USER=proxy-client
SOCKS_PASS=replace-with-another-password
```

用户名与密码必须成对设置。也可使用 `SOCKS_USER_FILE`、`SOCKS_PASS_FILE`、`ADMIN_USER_FILE`、`ADMIN_PASS_FILE` 读取 Docker/Kubernetes secret；直接环境值与对应 `_FILE` 不能同时存在，显式 CLI 参数优先。

## 配置

### 二进制 CLI

| 参数 | 默认值 | 说明 |
|---|---:|---|
| `-listen` | `127.0.0.1:1080` | 本地 SOCKS5 监听地址 |
| `-status` | `127.0.0.1:8080` | 管理页面/API 地址 |
| `-data-dir` | `./data` | 持久化目录 |
| `-scrape-interval` | `20m` | 刷新完成后到下一轮的间隔 |
| `-check-timeout` | `10s` | 单节点检查、出口与附加探测共享的总预算 |
| `-max-concurrent` | `20` | 并发健康检查数 |
| `-max-candidates` | `3000` | 每轮最多检查的候选数 |
| `-max-client-connections` | `512` | 入站 SOCKS5 并发连接上限 |
| `-require-ip-change` | `true` | 排除已确认未改变出口 IP 的节点 |
| `-trusted-management-proxy` | 空 | 可转发未认证本地管理请求的精确对端 IP；可重复或逗号分隔 |
| `-socks-user/-socks-pass` | 空 | SOCKS5 入站认证 |
| `-admin-user/-admin-pass` | 空 | 管理页面/API Basic Auth |

平台设置 `PORT` 且 CLI 未显式指定监听地址时，管理服务监听 `0.0.0.0:$PORT`，SOCKS5 保持 `0.0.0.0:1080`。

### Compose 环境变量

`.env.example` 提供：

| 变量 | Compose 默认值 | 说明 |
|---|---:|---|
| `SOCKS_PORT` | `1080` | 宿主机 loopback SOCKS 端口 |
| `SOCKS_AUX_PORTS` | `1081-1180` | 发布到宿主机 loopback 的附加 SOCKS 端口范围；管理界面创建的端口需落在已发布范围内 |
| `DASHBOARD_PORT` | `8080` | 宿主机 loopback 管理端口 |
| `MAX_CONCURRENT` | `50` | 健康检查并发数 |
| `MAX_CANDIDATES` | `1500` | 每轮检测候选数 |
| `MAX_CLIENT_CONNECTIONS` | `512` | 入站连接上限 |
| `SCRAPE_INTERVAL` | `20m` | 来源刷新间隔 |
| `CHECK_TIMEOUT` | `10s` | 健康检查预算 |
| `REQUIRE_IP_CHANGE` | `true` | 是否要求已知出口发生变化 |
| `GOMEMLIMIT` | `768MiB` | Go 运行时软内存目标 |
| `GOGC` | `100` | Go GC 百分比 |

Compose 还设置 1024 MiB 内存上限、384 MiB reservation、2 CPU、512 PID、`nofile=65536`、16 MiB 临时目录和日志轮转。这些是保护边界，不是性能或可用节点数量承诺；来源规模、网络质量和检查超时都会影响刷新耗时与资源使用。对于 1-2GB 内存的小机器，默认值已调低以适配；如内存充裕可适当上调 `GOMEMLIMIT` 和 `mem_limit`。

容器根文件系统只读，仅数据卷和 `/tmp` 可写；入口脚本只用 root 修复旧卷权限，随后以 UID/GID 10001 运行服务。镜像构建与运行基础镜像已固定 digest。

## 升级、备份与恢复

普通更新不会删除命名卷：

```bash
git pull --ff-only
docker compose build --pull
docker compose up -d
docker compose ps
```

`docker compose up -d` 会复用 `socks5-pool-pro-data`。普通 `docker compose down` 也保留命名卷；**不要执行 `docker compose down -v`**，除非确定要清空来源、规则、候选目录、转发池与统计。

早期版本的同名网络可能使用动态网段；新版固定为 `172.30.250.0/24`，用于精确限制未认证管理访问。第一次从旧网络升级时，先备份并确认该网络没有其他容器，再只重建容器和网络：

```bash
docker compose stop proxy-pool
docker rm -f socks5-pool-pro 2>/dev/null || true
docker network inspect socks5-pool-pro-net
# 仅当上一步的 Containers 为空、确认没有其他使用者时执行：
docker network rm socks5-pool-pro-net
docker compose up -d --build
```

这不会删除 `socks5-pool-pro-data`；不要为迁移网桥使用 `docker compose down -v`。如果固定网段与本机已有网络冲突，应同时修改 Compose 网段、网关和 `-trusted-management-proxy`，保持精确 IP 对齐。

建议停服务后做一致性备份：

```bash
mkdir -p backups
docker compose stop proxy-pool
docker run --rm \
  -v socks5-pool-pro-data:/data:ro \
  -v "$PWD/backups:/backup" \
  alpine:3.24.1 \
  sh -c 'umask 077; tar -czf /backup/socks5-pool-data.tar.gz -C /data .'
chmod 600 backups/socks5-pool-data.tar.gz
docker compose start proxy-pool
```

恢复会覆盖当前卷，请先另做备份并保持服务停止：

```bash
docker compose stop proxy-pool
docker run --rm \
  -v socks5-pool-pro-data:/data \
  -v "$PWD/backups:/backup:ro" \
  alpine:3.24.1 \
  sh -c 'find /data -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +; tar -xzf /backup/socks5-pool-data.tar.gz -C /data'
docker compose start proxy-pool
```

进程收到 SIGINT/SIGTERM 时会停止接收新连接，给 HTTP/SOCKS 会话有限时间退出，并刷新池缓存。Compose 的 `stop_grace_period` 为 45 秒。

## Railway

仓库保留 `railway.toml`，使用 Dockerfile 构建，`/healthz` 用作健康检查。Railway 的 `PORT` 面向 HTTP 管理服务；普通 HTTP 域名不会自动提供 SOCKS5 TCP 1080。

在 Railway 上部署时：

- 给 `/app/data` 挂载持久卷，否则重建实例会丢失本地状态；
- 设置 `ADMIN_USER/ADMIN_PASS`，并只通过 HTTPS 使用管理面；
- 若需要外部 SOCKS5，必须另行配置平台 TCP 入口，并设置独立的 `SOCKS_USER/SOCKS_PASS`；
- 不要把公开 HTTP 域名当作 SOCKS5 入口。

## 开发与验证

```bash
gofmt -w *.go
go test ./...
go vet ./...
go test -race ./...
node --check web/dashboard.js
docker compose config
docker build -t socks5-pool-pro:test .
```

没有本地 Go 时可使用构建镜像：

```bash
docker run --rm -v "$PWD:/src" -w /src golang:1.26.5-alpine3.24 go test ./...
docker run --rm -v "$PWD:/src" -w /src golang:1.26.5-alpine3.24 go vet ./...
docker run --rm -v "$PWD:/src" -w /src golang:1.26.5-alpine3.24 \
  sh -c 'apk add --no-cache gcc musl-dev >/dev/null && CGO_ENABLED=1 go test -race ./...'
```

测试覆盖来源解析与预算、SOCKS/HTTP 隧道、认证、健康失败防抖、凭据变体、缓存权限、候选采样与分页、API 合同、CSRF/SSRF、ProxyIP 验证、测速、优雅关闭和前端合同。

## 主要文件

```text
main.go                 刷新、抽样、健康任务与进程生命周期
parser.go               来源抓取、SSRF 防护与格式解析
checker.go              健康检查、出口 IP 与地理信息
pool.go                 已知转发池、健康状态、策略与持久化
candidate_catalog.go    完整候选目录、last-good 合并与分页
candidatecache.go       候选目录压缩缓存
server.go               本地 SOCKS5 入口与规则路由
dial.go                 SOCKS5/HTTP CONNECT 上游拨号
status.go               管理 API、安全中间件与响应模型
sourcestore.go          来源、规则、分组和检查目标配置
speedtest.go            按需完整样本测速
web/                    嵌入式 Proxy Atlas 前端
compose.yaml            推荐的本地容器部署
```

## 已知边界

- 免费代理列表变化快，候选多不等于健康节点多；项目只能检测和管理，不能修复失效上游。
- 第三方出口 IP、地理信息、测速和 ProxyIP 验证服务都可能超时或限流；这些附加结果不应与基础健康状态混为一谈。
- 全部健康节点的 `/api/status` 可能较大；新客户端应使用 `/api/v1/proxies`，界面轮询应使用 `compact=1`。
- 管理 API、CSV 与池缓存可能含上游凭据；即使响应设置了 `no-store`，也不应公开转发或上传到不可信系统。
- 大型来源仍会在解析和建立紧凑目录时占用显著内存。应按实际来源规模调整容器资源、启用来源数量和每轮检测预算，不要把默认值当作容量保证。
