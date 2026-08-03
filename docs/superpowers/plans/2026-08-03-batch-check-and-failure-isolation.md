# 候选批量检测与失败隔离实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox syntax for explicit progress tracking.

**目标：** 把候选库存重构为“仅待检测”、把检测失败与策略排除节点迁入独立失败集合，并提供受现有并发/超时/批量配置约束的管理员批量检测、失败节点人工重测及管理后台操作入口。

**架构：** `CandidateCatalog` 继续保存来源去重所需的内部库存与 ProxyIP 资源，但增加独立、持久化的失败记录集合和运行时租约。候选批量检测、失败重测、完整来源刷新和单来源刷新都先原子领取租约，再调用同一正式单节点检查器；候选检查成功进入 `ProxyPool`，失败或策略排除进入失败集合。后台任务永远不能领取失败租约。人工任务由 `RefreshCoordinator` 维护一个短生命周期、不可排队且全局互斥的操作状态。

**技术栈：** Go 1.23、`net/http`、原子快照与互斥锁、gzip 二进制缓存、原生 HTML/CSS/JavaScript、Go `testing`/`httptest`、Docker Compose。

---

## 已确认的不可变语义

- 候选分页与候选计数只包含待检测、可转发协议节点；检测中的节点仍属于候选集合，但不能被第二个任务领取。
- 检测成功：进入转发池，不再出现在候选页；检测失败或 `require-ip-change` 策略排除：从候选页消失，进入独立失败集合。
- 失败集合的节点只能由管理员调用失败重测 API；来源刷新、定时器、启动恢复、轻量健康检查和完整全池复检都不能申请失败租约。
- 来源再次发现失败 key 时只合并来源与最后发现时间，不改变失败类型、错误摘要或最近检测时间，不创建候选，不自动重测。
- `Proxy.Key()` 的协议感知语义不变；相同 `host:port` 的 SOCKS5、HTTP、HTTPS 记录相互独立。
- 来源抓取失败不等于节点检测失败；未真正完成节点检测的记录不得进入失败集合。
- 人工候选批量检测与失败重测共用一个互斥边界；已有人工任务处于 queued/running 时，新启动请求返回 `409 Conflict`，不创建隐藏队列。
- `limit` 省略时使用有效 `MaxCandidates`，显式值限制在 `1..MaxCandidates`；并发和单节点超时复用有效 `MaxConcurrent` 与 `CheckTimeout`，不新增配置项。
- 检查 URL/策略代次变化或应用关闭导致的无结果节点只释放租约并留在原集合，不能误记失败。
- `ResetHealthOutcomes` 不清除、不迁移失败记录；已知转发池节点现有 health recheck 生命周期不改。
- 旧缓存 `checked_failed` 迁移为 `unreachable`，`policy_filtered` 迁移为同名失败类型；备用凭据、来源、地区、时间和 ProxyIP 资源必须保留。
- 不增加自动冷却恢复、失败自动重测、自动回候选、任务持久化、暂停/恢复或任务队列。
- 不修改 Kimi、Docker、系统代理、认证或其他安全设置。
- 本轮由主 agent inline 实现，不派子 agent。

## API 合同

### 候选批量检测

```http
POST /api/candidates/batch-check
Content-Type: application/json

{"limit": 3000}
```

- 空对象或省略 `limit` 使用有效 `MaxCandidates`。
- 成功返回 `202 Accepted`、操作对象及 `status_url`。
- 已有人工目录任务返回 `409`，错误码 `candidate_check_busy`。

```http
GET /api/candidates/batch-check/status
```

### 失败分页与人工重测

```http
GET /api/failed-candidates?page=1&page_size=50&source=Source%20A&protocol=socks5&failure_type=unreachable
```

```http
POST /api/failed-candidates/retry
Content-Type: application/json

{"keys":["socks5://1.2.3.4:1080"]}
```

```http
GET /api/failed-candidates/retry/status
```

- 两个 status 路由返回同一个人工目录任务状态机；`kind` 区分 `candidate_batch` 与 `failed_retry`，启动响应指向对应 status URL。
- 重测请求去重后必须至少包含一个 key，数量不得超过有效 `MaxCandidates`；任一 key 不在失败集合时整批返回 `400 failed_candidate_not_found`，不做部分领取。

### ProxyIP 兼容迁移

```http
GET /api/proxyip/page?page=1&page_size=50&source=Source%20A&country=US
```

- `/api/candidates/page` 不再返回 ProxyIP、已知转发池节点或失败节点。
- 新增只读 `/api/proxyip/page`，保留现有 ProxyIP 浏览、分页、来源/地区筛选、复制和 `/api/proxyip/verify` 能力。
- 管理后台给 ProxyIP 独立页签，避免把非路由资源重新伪装成候选。

### 操作状态结构

```go
type CandidateCheckOperation struct {
    ID             string `json:"id"`
    Kind           string `json:"kind"`   // candidate_batch | failed_retry
    Status         string `json:"status"` // queued | running | complete | cancelled | superseded | failed
    RequestedAt    string `json:"requested_at"`
    StartedAt      string `json:"started_at,omitempty"`
    CompletedAt    string `json:"completed_at,omitempty"`
    Total          int    `json:"total"`
    Completed      int    `json:"completed"`
    Alive          int    `json:"alive"`
    Failed         int    `json:"failed"`
    PolicyFiltered int    `json:"policy_filtered"`
    Error          string `json:"error,omitempty"`
}
```

---

## Task 0：建立可审计基线并保存批准计划

**Files:**
- Create after Plan mode: `docs/superpowers/plans/2026-08-03-batch-check-and-failure-isolation.md`
- Preserve: `docs/superpowers/specs/2026-08-03-batch-check-and-failure-isolation-design.md`

- [ ] 退出 Plan mode 后，先调用 `superpowers:executing-plans`，再把本计划原文写入项目 plan 文件；不覆盖未跟踪的设计文档。
- [ ] 读取项目根与适用子目录的 `AGENTS.md`；若没有更深规则，继续遵循当前根规则。
- [ ] 记录 `git status --short --branch`、`git diff --stat`、`git diff --check` 和最近提交；确认当前已知状态 `main...origin/main [ahead 1]` 及未跟踪设计文档未被意外改写。
- [ ] 运行基线检查并保存结果；失败时先判断是否为既有失败，不能把红灯当成新功能完成：

```bash
go test ./...
node --check web/dashboard.js
git diff --check
```

- [ ] 所有临时测试数据只放 Go `t.TempDir()` 或项目专用临时目录，不在仓库根生成诊断文件。

## Task 1：重构目录模型、归属视图与原子租约

**Files:**
- Modify: `candidate_catalog.go`
- Modify: `candidate_catalog_test.go`
- Modify: `status_inventory_manage.go`
- Modify: `status_inventory_manage_test.go`
- Modify: `status_util.go`
- Modify: `status_handlers.go`
- Modify: `status_views.go`

### 1.1 先写目录状态测试

- [ ] 把当前“候选 API 返回 deferred/known/checked_failed/policy/resource 全部状态”的断言改为新合同：候选页只返回待检测且不在 pool 的转发节点。
- [ ] 新增以下测试，并先运行确认失败：
  - `TestCandidatePageContainsOnlyPendingForwardingRecords`
  - `TestCandidateOutcomeMovesFailureOutOfCandidates`
  - `TestCandidateOutcomeHidesAliveRecordBehindPoolOwnership`
  - `TestFailedRediscoveryMergesSourcesWithoutRequeueing`
  - `TestCandidateFailureKeysRemainProtocolAware`
  - `TestResetHealthOutcomesPreservesFailures`
  - `TestCandidateLeasePreventsDuplicateClaims`
  - `TestFailedLeaseRequiresExplicitKeys`
  - `TestCancelledLeaseLeavesRecordInOriginalCollection`
  - `TestStaleLeaseCannotPublishIntoReplacementSnapshot`
  - `TestCatalogBudgetCountsInventoryAndFailuresWithoutEviction`
- [ ] 用 focused command 验证红灯来自缺失的新结构而不是测试拼写：

```bash
go test ./... -run 'Test(CandidatePageContainsOnlyPendingForwardingRecords|CandidateOutcomeMovesFailureOutOfCandidates|CandidateOutcomeHidesAliveRecordBehindPoolOwnership|FailedRediscoveryMergesSourcesWithoutRequeueing|CandidateFailureKeysRemainProtocolAware|ResetHealthOutcomesPreservesFailures|CandidateLeasePreventsDuplicateClaims|FailedLeaseRequiresExplicitKeys|CancelledLeaseLeavesRecordInOriginalCollection|StaleLeaseCannotPublishIntoReplacementSnapshot|CatalogBudgetCountsInventoryAndFailuresWithoutEviction)$'
```

### 1.2 增加失败记录与视图

- [ ] 在 `candidate_catalog.go` 保留 `CandidateStatus` 仅用于旧缓存解码/迁移和内部兼容；新失败类型独立定义：

```go
type CandidateFailureKind uint8

const (
    candidateFailureUnreachable CandidateFailureKind = iota + 1
    candidateFailurePolicyFiltered
)

type candidateFailureRecord struct {
    candidateRecord
    kind      CandidateFailureKind
    lastError string
}
```

- [ ] 在 `candidateSnapshot` 增加按 `protocol://addr` 排序的 `failedRecords []candidateFailureRecord`；失败记录复用同一 snapshot 的 source/protocol/country/city dictionaries、`sourceRefs` 与备用凭据表，避免重复保存完整字符串表。
- [ ] 错误摘要经过单行化和固定字节上限（例如 512 bytes），不得把响应正文或任意大错误写入缓存。
- [ ] snapshot builder、clone、过滤、来源合并、删除、facet 重建和预算校验同时处理普通记录与失败记录；总量预算按 `len(records)+len(failedRecords)` 计算。超预算时拒绝新快照并保留当前 last-good，不自动丢弃失败记录。
- [ ] `begin()` 在构建当前来源快照时执行双路有序合并：
  - 当前 key 已在失败集合：从普通 records 中剔除，只更新失败记录来源、连接声明补充字段与 `seenUnix`；保留原 `kind`、`lastError`、`checkedUnix`。
  - 当前 key 未失败：按现有完整/partial 来源语义进入内部 records。
  - 当前来源未再次声明的失败记录仍保留；来源停用/删除不会自动把失败记录迁回候选或删除。
- [ ] `ApplyHealthOutcomes` 不再给候选写 `checked_failed`/`policy_filtered`；已知转发池 health recheck 只修改 pool，不能产生失败候选。
- [ ] `ResetHealthOutcomes` 只重置普通 records 的 criterion-dependent 兼容元数据和 phase，不遍历或修改 `failedRecords`。

### 1.3 增加运行时租约与批量结果事务

- [ ] 在 `CandidateCatalog` 增加独立的运行时租约 mutex、单调 token、候选 lease map、失败 lease map 和公平游标；租约不进入磁盘缓存。

```go
type CandidateLease struct {
    Token uint64
    Key   string
    Proxy Proxy
    Kind  string // candidate | failed
}

func (c *CandidateCatalog) LeasePending(limit int, known candidateKnownIndex) []CandidateLease
func (c *CandidateCatalog) LeasePendingKeys(keys []string, known candidateKnownIndex) []CandidateLease
func (c *CandidateCatalog) LeaseFailed(keys []string) ([]CandidateLease, []string)
func (c *CandidateCatalog) ReleaseLeases(leases []CandidateLease)
func (c *CandidateCatalog) CommitLeaseOutcomes(leases []CandidateLease, outcomes map[string]candidateCheckOutcome) error
```

- [ ] `LeasePending` 从排序 records 的公平游标循环领取，只接受转发协议、`candidateDeferred`、不在当前 pool overlay 且未被租用的 key；资源、失败和 known pool keys 永不返回。
- [ ] `LeasePendingKeys` 用于来源刷新已经抽样出的 key；返回顺序与输入一致，未取得租约的 key被跳过。
- [ ] `LeaseFailed` 只能由失败重测入口调用；必须先验证全部 key 存在且未被租用，再一次性领取，避免半批执行。
- [ ] 租约 token 与当前规范化连接声明绑定；snapshot 更替可保留同一未变声明的 lease，但连接声明消失或凭据发生不兼容替换时，旧 token 提交失败并只释放租约。
- [ ] `CommitLeaseOutcomes` 在一个 persist-then-publish snapshot 事务中：
  - candidate + unreachable/policy → 从普通 records 移入/更新 failedRecords；
  - failed retry + unreachable/policy → 更新 kind/error/checkedUnix，留在 failedRecords；
  - alive → 目录不制造失败；pool ownership overlay 使内部去重记录不再可见/可租，failed retry 的成功记录从 failedRecords 删除；
  - 没有结果、取消、代次过期 → 不改集合，只释放 lease。
- [ ] pool 成功结果先通过现有健康代次和来源 revision 校验并持久加入 `ProxyPool`，再提交目录变更；若目录持久化失败，pool 中的已知 key仍由候选页 overlay 隐藏，并在下一次 `begin()` 对账，不能丢失已验证成功节点。
- [ ] `RemoveKeys` 只删除普通候选/资源内部记录；不把失败记录当候选删除。失败集合不新增自动/批量删除入口。

### 1.4 拆分候选、失败与 ProxyIP 分页

- [ ] `buildCandidatePage` 扫描普通 records 时只计入：转发协议、deferred、当前 pool overlay 不 known；移除 `status` 查询和 `Statuses` facet，`CandidateTotal` 改为真实待检测总数。
- [ ] 新增：

```go
type FailedCandidateView struct {
    CandidateView
    FailureType string `json:"failure_type"`
    LastError   string `json:"last_error"`
}

type FailedCandidatePageResponse struct {
    FailedCandidates []FailedCandidateView `json:"failed_candidates"`
    SnapshotID       string                `json:"snapshot_id"`
    Page             int                   `json:"page"`
    PageSize         int                   `json:"page_size"`
    PageCount        int                   `json:"page_count"`
    HasNext          bool                  `json:"has_next"`
    FilteredTotal    int                   `json:"filtered_total"`
    FailedTotal      int                   `json:"failed_total"`
    Sources          []CandidateFacet      `json:"sources"`
    Protocols        []CandidateFacet      `json:"protocols"`
    FailureTypes     []CandidateFacet      `json:"failure_types"`
}
```

- [ ] 失败分页支持 `page/page_size/snapshot_id/source/protocol/failure_type`，保持快照冲突 `409 snapshot_changed` 与现有分页上限。
- [ ] 新增 ProxyIP page builder，仅扫描 resource records，沿用现有 search/source/country/page/snapshot 合同；`protocolTotal("proxyip")` 与 `proxyip_total` 继续来自资源记录。
- [ ] `compactStatusSummary` 新增 `failed_candidate_total`；`candidate_total` 改为待检测总数，不再代表完整来源 inventory。
- [ ] 运行 Task 1 focused tests和相关既有测试：

```bash
go test ./... -run 'Test(Candidate|Failed|ProxyIP|CompactStatus|Inventory)'
git diff --check
```

## Task 2：升级候选缓存 v4并迁移旧失败状态

**Files:**
- Modify: `candidatecache.go`
- Modify: `candidatecache_test.go`
- Modify: `candidate_catalog_test.go`

### 2.1 先写缓存兼容测试

- [ ] 新增并先跑红：
  - `TestCandidateCacheV4RoundTripsFailedRecordsAndCredentials`
  - `TestCandidateCacheV4RestoresFailuresOutsideCandidatePage`
  - `TestCandidateCacheV3MigratesCheckedFailureRecords`
  - `TestCandidateCacheV3MigratesPolicyFilteredRecords`
  - `TestCandidateCacheV3PreservesDeferredKnownResourceAndAlternates`
  - `TestCandidateCacheRejectsInvalidFailureKind`
  - `TestCandidateCacheRejectsFailureReferenceAndStringBudgets`
  - `TestCandidateCacheFiltersInvalidFailureAddressWithoutDroppingValidRows`

```bash
go test ./... -run 'TestCandidateCache(V4|V3|Rejects|Filters)'
```

### 2.2 编码、解码和迁移

- [ ] 把 `candidateCacheVersion` 从 3 升到 4，文件名和 magic 保持 `candidate_catalog.v1.bin.gz` / `SPCAND01`，继续读取 v1/v2/v3。
- [ ] v4 在普通 records、备用凭据表之后显式编码 failed record count及每条失败 kind/error；失败 record 继续使用普通 record 的完整地址、凭据、source refs、地区与时间字段。
- [ ] 解码 v1-v3 后执行一次内存迁移：
  - `candidateCheckedFailed` → `failedRecords(kind=unreachable)`；
  - `candidatePolicyFiltered` → `failedRecords(kind=policy_filtered)`；
  - 从普通 records 删除这两类；
  - `deferred`、known 兼容记录、resource 和 credential alternates 原样保留。
- [ ] v4 校验覆盖：失败 kind 枚举、排序/唯一 key、地址/协议、source range、credential alternate range、错误字符串上限、总 record/reference/string/decoded byte预算。
- [ ] `cloneCandidateSnapshot`、`filterNonPublicCandidateRecords`、`validateAndRebuildCandidateSnapshot` 和 facets 同时覆盖 failedRecords；过滤无效旧记录时逐条丢弃坏记录，不因一条坏 ProxyIP/失败地址清空整份 last-good。
- [ ] `LoadDiskCache` 日志分别报告 pending/inventory、failed、resource 计数；启动调用 `ResetHealthOutcomes` 时验证失败计数不变。
- [ ] 运行缓存套件与全目录回归：

```bash
go test ./... -run 'TestCandidate(Cache|Catalog)'
git diff --check
```

## Task 3：抽取正式单节点检查器并统一来源/人工行为

**Files:**
- Create: `candidate_check.go`
- Create: `candidate_check_test.go`
- Modify: `checker.go`
- Modify: `checker_test.go`
- Modify: `main.go`
- Modify: `status_candidate_speed.go`
- Modify: `status_candidate_speed_test.go`
- Modify: `pool.go`
- Modify: `pool_test.go`

### 3.1 先锁定现有正式检查行为

- [ ] 用本地 `httptest.Server` 和可替换 probe seams 写 characterization tests，覆盖凭据轮询、当前 check URL、单节点 deadline、出口策略、geo、匿名性及 parent cancellation；不访问公网。
- [ ] 新增并先跑红：
  - `TestCheckCandidateUsesCredentialExitGeoAndAnonymityChain`
  - `TestCheckCandidateClassifiesConnectionFailureWithSummary`
  - `TestCheckCandidateClassifiesIPChangePolicySeparately`
  - `TestCheckCandidateParentCancellationHasNoFailureOutcome`
  - `TestCheckCandidateDeadlineIsUnreachableWhenParentRemainsActive`
  - `TestCheckCandidateBatchNeverExceedsConfiguredConcurrency`

### 3.2 抽取共用检查结果

- [ ] 在新文件定义：

```go
type candidateCheckKind uint8

const (
    candidateCheckNoResult candidateCheckKind = iota
    candidateCheckAlive
    candidateCheckUnreachable
    candidateCheckPolicyFiltered
)

type candidateCheckOptions struct {
    Timeout         time.Duration
    MaxConcurrent   int
    RequireIPChange bool
    TestURL         string
    BaselineIP      string
}

type candidateCheckOutcome struct {
    Key     string
    Proxy   Proxy
    Kind    candidateCheckKind
    Error   string
}

func checkCandidateContext(ctx context.Context, px Proxy, options candidateCheckOptions) candidateCheckOutcome
```

- [ ] `checkCandidateContext` 保持现有顺序：forwarding protocol guard → credential-aware check URL → latency → exit IP → IP-change policy → exit geo → anonymity；附加 exit/geo/anonymity 探测失败仍不推翻基础 alive。
- [ ] parent 已取消返回 `candidateCheckNoResult`；单节点 timeout 且 parent 仍有效返回 `unreachable`，错误摘要规范化后供失败记录保存。
- [ ] `checkProxiesDetailedContext` 改为在受 `MaxConcurrent` 限制的 worker 中调用单节点函数，再适配回现有 `alive/unreachable/policyFiltered` 返回合同，确保既有 recheck 调用方不漂移。
- [ ] 给批量调用增加 outcome callback/结果 map，使来源刷新和人工任务能取得每个失败 key 的摘要，而不是只得到布尔 map。

### 3.3 来源刷新必须先租约再检查

- [ ] `refreshPoolContext` 和 `refreshSourceContext` 在 `begin()` 发布/合并 inventory 后，仅从当前 pending records 构建或筛选检查工作集；failed、known pool和 ProxyIP 不消耗 `MaxCandidates`。
- [ ] 保留现有 `candidateSampler` 的 unseen-first、来源/协议平衡和持久游标；抽样后调用 `LeasePendingKeys`，真正网络请求只处理成功领取的 proxy。
- [ ] 任务完成时，在 `sourceLifecycleMu` + 当前 source revision + 当前 health generation 边界内先发布 pool alive，再一次性提交 catalog lease outcomes；未取得租约的 key和取消无结果的 key不记失败。
- [ ] `refreshPoolContext`/`refreshSourceContext` 的 `defer`/取消路径始终释放尚未提交的租约。
- [ ] `ApplyHealthOutcomes` 从保留池 recheck 路径移除；pool 节点失败继续按原有 terminal/full-recheck 规则处理，不进入 failed-candidates。
- [ ] 更新 scrape 日志：`scrape.candidates` 仍表示本轮来源协议感知去重数；`candidate_total` 表示当前 pending；新增失败计数只在 compact status/失败 API 中报告，避免数字重名。

### 3.4 收紧旧候选测速路径

- [ ] 候选页不再把“测速并加入池”作为主要检测入口；正式批量检查接管准入。
- [ ] 保留 `/api/candidates/speedtest` 兼容路由，但它必须使用同一 candidate lease：
  - 前置健康检查真正 unreachable/policy 时提交失败 outcome；
  - 健康通过但下载测速站失败时只释放 lease，仍为 pending，因为测速站失败不等于代理检查失败；
  - 测速成功仍可按现有 `PromoteCandidateSpeed` 准入，并使候选页通过 pool overlay 隐藏。
- [ ] 更新旧测试和注释，不再声称“所有失败项都留在候选”。
- [ ] 运行检查器、来源、pool、测速回归：

```bash
go test ./... -run 'Test(Check|Refresh|SourceRefresh|CandidateSpeed|PromoteCandidate|Pool)'
go test -race ./... -run 'Test(CheckCandidateBatchNeverExceedsConfiguredConcurrency|CandidateLeasePreventsDuplicateClaims|Refresh)'
git diff --check
```

## Task 4：实现人工任务状态机、API与关闭语义

**Files:**
- Create: `candidate_check_operation.go`
- Create: `candidate_check_operation_test.go`
- Create: `status_candidate_check.go`
- Create: `status_candidate_check_test.go`
- Modify: `refresh_coordinator.go`
- Modify: `main.go`
- Modify: `status.go`
- Modify: `status_api_contract_test.go`
- Modify: `background_lifecycle_test.go`
- Modify: `status_source_refresh_test.go`

### 4.1 先写 coordinator/API 红灯测试

- [ ] 新增：
  - `TestCandidateBatchCheckDefaultsAndClampsLimit`
  - `TestCandidateBatchCheckRejectsInvalidLimit`
  - `TestCandidateBatchCheckReturnsAcceptedOperation`
  - `TestCandidateCheckRejectsSecondManualTaskWithConflict`
  - `TestCandidateCheckStatusTracksProgressAndOutcomeCounts`
  - `TestFailedRetryRejectsUnknownKeyBeforeStarting`
  - `TestFailedRetryUsesConfiguredCandidateLimitConcurrencyAndTimeout`
  - `TestFailedRetrySuccessPromotesAndRemovesFailure`
  - `TestFailedRetryFailureUpdatesAndRetainsFailure`
  - `TestCandidateTaskCancellationReleasesUnfinishedLeases`
  - `TestCandidateTaskCriterionChangeSupersedesWithoutFailure`
  - `TestCoordinatorShutdownCancelsCandidateOperationAndRejectsNewOne`
  - `TestAutomaticWorkersCannotLeaseFailedCandidates`
- [ ] 扩展 `TestAPIRoutesMethodsErrorsAndSecurityHeaders`，断言新 GET/POST 路由的 405/Allow、结构化错误、CSRF/management middleware和 JSON content type合同。

### 4.2 coordinator 中增加单人工任务槽

- [ ] `RefreshCoordinator` 增加 `candidateCheckChan chan struct{}`、operation mutex/sequence、pending/active/last和内部 request spec；channel 只作 wake-up，不作任务队列。
- [ ] `requestCandidateCheck(kind, limit, keys)` 在同一锁内保留唯一 pending operation；active 或 pending 已存在时返回当前 operation 与 typed busy error，handler 映射为 409。
- [ ] 独立 background lifecycle worker消费 wake-up；先取得 `healthCycleMu`，再按 kind领取 candidate/failed leases并把真实 lease 数写入 `Total`，随后以有效 `MaxConcurrent` 执行共用检查器。
- [ ] 每个完成 outcome 立即更新内存进度计数，但目录/pool在当前批次结束后一次性发布；一个节点失败不取消剩余节点。
- [ ] health criterion 变化、source revision 失效、context cancellation和 shutdown 进入 `superseded`/`cancelled`，释放无结果 lease；网络失败才计 `Failed`。
- [ ] `shutdown()`、`resetForTest()` 和 channel drain覆盖 candidate operation；关闭后 request直接返回 cancelled rejection。

### 4.3 注册 API handler

- [ ] 在 `status.go` 注册：

```go
mux.HandleFunc("/api/candidates/batch-check", requirePost(s.handleCandidateBatchCheck))
mux.HandleFunc("/api/candidates/batch-check/status", requireGet(s.handleCandidateCheckStatus))
mux.HandleFunc("/api/failed-candidates", requireGet(s.handleFailedCandidatesPage))
mux.HandleFunc("/api/failed-candidates/retry", requirePost(s.handleFailedCandidatesRetry))
mux.HandleFunc("/api/failed-candidates/retry/status", requireGet(s.handleCandidateCheckStatus))
mux.HandleFunc("/api/proxyip/page", requireGet(s.handleProxyIPPage))
```

- [ ] POST body继续使用严格 `decodeJSON`，拒绝未知字段、空 body语义错误、负数/溢出、重复 key；响应使用现有 `writeJSONStatus`/`writeErrCode`。
- [ ] batch start成功写 `Location` 到候选 status；failed retry成功写 `Location` 到 retry status；均返回 `202`。
- [ ] `GET /api/failed-candidates` 与 `/api/proxyip/page` 支持 HEAD、snapshot header和 stale snapshot 409。
- [ ] 运行：

```bash
go test ./... -run 'Test(CandidateBatchCheck|CandidateCheck|FailedRetry|CoordinatorShutdown|AutomaticWorkersCannotLeaseFailedCandidates|APIRoutesMethodsErrorsAndSecurityHeaders)'
go test -race ./... -run 'Test(CandidateCheck|FailedRetry|CoordinatorShutdown)'
git diff --check
```

## Task 5：重做管理后台的候选、失败与 ProxyIP 操作面

**Files:**
- Modify: `web/dashboard.html`
- Modify: `web/dashboard.js`
- Modify: `web/dashboard.css`
- Modify: `dashboard_html_test.go`

### 5.1 先写 DOM/客户端合同测试

- [ ] 更新旧候选“完整目录/状态筛选/失败留候选”断言，并新增：
  - `TestDashboardSeparatesPendingFailedAndProxyIPTabs`
  - `TestDashboardCandidateBatchCheckUsesAsyncStatusAPI`
  - `TestDashboardFailedPageFiltersSelectsAndRetries`
  - `TestDashboardCandidateAndFailedTasksDisableDuplicateSubmission`
  - `TestDashboardProxyIPUsesDedicatedPageAPI`
  - `TestDashboardRefreshesAllThreeCountsAfterManualTask`
- [ ] 先运行确认测试因缺少新 DOM/JS合同失败：

```bash
go test ./... -run 'TestDashboard(SeparatesPendingFailedAndProxyIPTabs|CandidateBatchCheckUsesAsyncStatusAPI|FailedPageFiltersSelectsAndRetries|CandidateAndFailedTasksDisableDuplicateSubmission|ProxyIPUsesDedicatedPageAPI|RefreshesAllThreeCountsAfterManualTask)$'
```

### 5.2 HTML 信息架构

- [ ] sidebar和 resource rail新增独立 `#failed-candidates` 与 `#proxyip`；`showTab`/hash allowlist/page metadata同步扩展，键盘 tab导航继续工作。
- [ ] 候选页文案改为“待检测节点”，删除 `cf-status`、known/failed/policy/resource汇总和“完整只读目录”描述；保留 search/source/protocol/country/page size。
- [ ] 候选页增加：有效 `MaxCandidates` 默认 limit输入、开始批量检测按钮、`role=status` 进度、alive/failed/policy计数；运行时禁用启动按钮。
- [ ] 失败页增加独立 metrics、failure type/source/protocol筛选、分页、全选当前页、多选重测、进度和错误摘要列；不提供自动重测开关。
- [ ] ProxyIP页迁移现有地区、来源、分页、复制、逐条专用验证组件；候选页不再出现 `proxyip` protocol选项。

### 5.3 JavaScript 状态和轮询

- [ ] 为 failed和proxyip分别维护 page/pageSize/snapshot/request/AbortController/queryGeneration；离开 tab或页面隐藏时取消对应 GET，不取消后台服务器任务。
- [ ] 抽取小型分页/快照 helper只减少三类 catalog重复请求样板，不改写无关 node/source/rule逻辑。
- [ ] 候选启动 POST后按返回 `id/status_url` 每 1–2 秒轮询；failed retry使用同一 operation renderer；只认相同 operation ID，不能被另一历史任务误判完成。
- [ ] queued/running显示 `completed/total` 与三个 outcome计数；complete/cancelled/superseded/failed停止 timer，恢复按钮并刷新：compact status、nodes、candidates、failed。
- [ ] 409 busy展示当前任务类型/状态并自动转为轮询，不重复 POST。
- [ ] 失败多选只提交当前选择 keys；snapshot 409时清空选择、回第一页并重新加载。
- [ ] 页面初始化和15秒 compact poll更新 pending/failed/pool/ProxyIP独立数字；大列表仍按现有节流，不在每次状态 poll扫描。

### 5.4 CSS与可访问性

- [ ] 复用现有 task metrics、filter bar、batch bar、notice、pager样式；只添加失败类型 badge、operation progress和必要 responsive规则。
- [ ] 新按钮有明确 `type="button"`，筛选控件有关联 label，任务进度使用 `aria-live`，表格空态和加载错误不只靠颜色。
- [ ] 运行：

```bash
node --check web/dashboard.js
go test ./... -run 'TestDashboard'
git diff --check
```

## Task 6：文档、全量验证、应用重建与发布门禁

**Files:**
- Modify: `README.md`
- Modify: `docs/superpowers/specs/2026-08-03-batch-check-and-failure-isolation-design.md`
- Verify: `docs/superpowers/plans/2026-08-03-batch-check-and-failure-isolation.md`
- Inspect only unless diagnostics exist: `temp/**`

### 6.1 更新行为文档

- [ ] 更新 README 架构图和数据边界表为四类：pending candidates、failed candidates、known pool、ProxyIP resources。
- [ ] 缓存说明改为内部 v4、兼容 v1-v3迁移；说明失败记录含凭据且仍受 `0600` 与缓存预算保护。
- [ ] 数字说明明确：`scrape.candidates` 是本轮来源去重数，`candidate_total` 是当前待检测数，`failed_candidate_total` 是独立失败数，`proxyip_total` 是资源数。
- [ ] 管理界面/API章节记录 batch check、status、failed page/retry/status与 `/api/proxyip/page`；明确失败不会由后台轮询。
- [ ] 健康生命周期章节区分“新候选失败进入失败栏”与“已知转发池节点既有 health recheck/terminal流程不变”。
- [ ] 修正设计文档资源浏览实现细节：内部 resource records通过独立 `/api/proxyip/page` 和 `#proxyip`页展示，不由候选 API返回；不改变其已确认语义。
- [ ] 搜索并修复仍声称“失败留候选”“候选包含 known/resource/failed”“失败自动重试”的注释、文档和 UI文案：

```bash
rg -n "checked_failed|policy_filtered|失败.*候选|完整候选目录|失败项仍留|cf-status|candidate_total|内部 v3" --glob '!docs/superpowers/plans/*.md' .
```

### 6.2 格式化与全量静态/测试验证

- [ ] 格式化所有改动 Go文件，再执行完整验证；任何失败都先修复并重跑对应命令，不把部分通过描述为完成：

```bash
gofmt -w candidate_catalog.go candidatecache.go candidate_check.go checker.go pool.go refresh_coordinator.go candidate_check_operation.go status_candidate_check.go status.go status_util.go status_handlers.go status_views.go status_candidate_speed.go main.go \
  candidate_catalog_test.go candidatecache_test.go candidate_check_test.go checker_test.go pool_test.go candidate_check_operation_test.go status_candidate_check_test.go status_candidate_speed_test.go status_inventory_manage_test.go status_api_contract_test.go background_lifecycle_test.go status_source_refresh_test.go
git diff --check
node --check web/dashboard.js
go test ./...
go test -race ./...
go vet ./...
go build ./...
docker compose config
```

- [ ] 审核最终 diff只包含本功能、已存在设计文档和项目 plan；不得回滚用户既有改动或顺手改无关文件。
- [ ] 再次 `Glob temp/**`。当前计划阶段实测没有任何匹配项；若实现/诊断过程中产生且能明确识别为本任务诊断产物，按用户已给出的删除指令删除这些具体文件；不删除名称/归属不确定的文件。

### 6.3 重建并验证运行应用

用户已明确要求修复后重启应用，因此实现和测试通过后可重建当前项目 Compose 服务；不执行 image prune、volume删除或其他破坏性 Docker 操作。

- [ ] 先记录当前服务和容器状态：

```bash
docker compose config --services
docker compose ps
```

- [ ] 重建并启动当前 Compose 项目：

```bash
docker compose up -d --build
```

- [ ] 等待当前服务进入运行态，检查日志中没有 panic/cache migration错误，再验证：

```bash
docker compose ps
curl --fail --silent --show-error http://127.0.0.1:8080/healthz
curl --fail --silent --show-error http://127.0.0.1:8080/readyz
curl --fail --silent --show-error 'http://127.0.0.1:8080/api/status?compact=1'
curl --fail --silent --show-error 'http://127.0.0.1:8080/api/candidates/page?page_size=1'
curl --fail --silent --show-error 'http://127.0.0.1:8080/api/failed-candidates?page_size=1'
curl --fail --silent --show-error 'http://127.0.0.1:8080/api/proxyip/page?page_size=1'
```

- [ ] 只读确认启动恢复后失败记录没有进入 candidate page，后台来源刷新没有让失败数下降或 candidate数因失败 key回升；不为验证而擅自触发失败重测。

### 6.4 Git commit/push门禁

- [ ] 所有验证通过后，展示 `git status --short --branch`、`git diff --stat`、关键测试结果和建议 commit message；**停下并请求本次 `git commit` 的一次性明确授权**。未获这次授权不得执行 add/commit。
- [ ] commit完成后再次确认工作区与 commit内容；**另行请求本次 `git push` 的一次性明确授权**。commit授权不自动包含 push授权。
- [ ] push获批并成功后，用只读远端查询核对本地 HEAD 与 upstream，不通过猜测声称一致：

```bash
git rev-parse HEAD
git ls-remote origin refs/heads/main
git status --short --branch
```

- [ ] 最终报告包括：实现范围、测试/构建/Compose证据、运行 API计数、是否发现/删除 temp诊断文件、commit hash、远端一致性，以及仍存在但不属于本需求的问题；没有证据的项目标为“未验证”。

---

## 完成判定

只有同时满足下列条件才可宣告完成：

- 候选页/API只含待检测转发节点，ProxyIP与失败记录都有独立 API/UI。
- 所有候选与失败重测网络请求都持有正确租约；后台代码没有失败租约入口。
- 候选失败/策略排除进入失败集合；来源重发现不自动回候选；管理员重测成功才离开失败集合。
- 缓存 v4 round-trip与 v1-v3迁移测试通过，重启恢复不改变失败归属。
- 人工任务受 `MaxCandidates`/`MaxConcurrent`/`CheckTimeout`控制，重复启动409，取消不制造失败。
- 旧转发池 health recheck行为、ProxyIP专用验证、候选测速兼容边界没有回归。
- `node --check`、`go test ./...`、`go test -race ./...`、`go vet ./...`、`go build ./...`、`docker compose config`全部成功。
- Compose重建后 health/readiness和三个目录 API实测成功。
- Git commit与push仅在各自新授权后执行，并以远端查询证实一致。
