package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func dashboardClientSource() string {
	return dashboardHTML + "\n" + string(dashboardCSS) + "\n" + string(dashboardJS)
}

func TestDashboardUsesSeparateEmbeddedAssets(t *testing.T) {
	if strings.Contains(dashboardHTML, "<style>") || strings.Contains(dashboardHTML, "function fetchJSON") {
		t.Fatal("dashboard template still contains inline CSS or JavaScript")
	}
	for path, contentType := range map[string]string{
		"/assets/dashboard.css": "text/css; charset=utf-8",
		"/assets/dashboard.js":  "text/javascript; charset=utf-8",
	} {
		recorder := httptest.NewRecorder()
		NewStatusServer(NewProxyPool(), &ConfigStore{}).handler().ServeHTTP(recorder, localTestRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != contentType || recorder.Body.Len() == 0 {
			t.Fatalf("GET %s = %d type=%q bytes=%d", path, recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.Len())
		}
	}
}

func TestDashboardPaginationCountryControlsMatchScriptContract(t *testing.T) {
	for _, id := range []string{"f-country", "cf-country", "px-country"} {
		if !strings.Contains(dashboardHTML, `id="`+id+`"`) {
			t.Fatalf("dashboard template is missing pagination country state control %q", id)
		}
		if !strings.Contains(string(dashboardJS), "requiredControl('"+id+"')") {
			t.Fatalf("dashboard script is missing required pagination control reference %q", id)
		}
	}
	for _, want := range []string{
		"function requiredControl(id)",
		"Promise.resolve().then(function() { return fetchJSON(nodePageURL(), options); })",
		"Promise.resolve().then(function() { return fetchJSON(candidatePageURL(), options); })",
		"Promise.resolve().then(function() { return fetchJSON(failedPageURL(), options); })",
		"Promise.resolve().then(function() { return fetchJSON(proxyIPPageURL(), options); })",
	} {
		if !strings.Contains(string(dashboardJS), want) {
			t.Fatalf("dashboard script is missing diagnosable pagination request contract %q", want)
		}
	}
}

func TestDashboardCandidateCountsUpdateActiveSummary(t *testing.T) {
	for _, want := range []string{
		"setText('candidate-matching', formatCount(total));",
		"setText('stat-matching', formatCount(total));",
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing candidate count update %q", want)
		}
	}
}

func TestDashboardExplainsHTTPSConnectWithoutChangingConsumerURL(t *testing.T) {
	for _, want := range []string{
		`protocol === 'https' ? 'https（HTTP CONNECT）'`,
		`连接代理本身使用 http://`,
		`<option value="https">https（HTTP CONNECT）</option>`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing HTTPS CONNECT explanation %q", want)
		}
	}
}

func TestDashboardCandidatePageShowsOnlyPendingWithSimpleFilters(t *testing.T) {
	for _, want := range []string{
		`id="candidate-total"`,
		`id="candidate-matching"`,
		`id="cf-text"`,
		`id="cf-source"`,
		`id="cf-proto"`,
		`id="cf-pagesize"`,
		`正式检查失败的候选会移入失败节点页`,
	} {
		if !strings.Contains(dashboardHTML, want) {
			t.Fatalf("dashboard is missing pending-candidate filter contract %q", want)
		}
	}
	for _, removed := range []string{
		`id="cf-status"`,
		`data-action="choose-candidate-summary"`,
		`完整只读目录`,
		`value="proxyip"`,
	} {
		if strings.Contains(dashboardHTML, removed) {
			t.Fatalf("candidate page must not keep retired status/summary filter chrome %q", removed)
		}
	}
	for _, removed := range []string{
		`function chooseCandidateSummary(`,
		`case 'choose-candidate-summary':`,
		`function candidateStatusTotal(`,
	} {
		if strings.Contains(string(dashboardJS), removed) {
			t.Fatalf("dashboard script still drives retired candidate summary/status filters %q", removed)
		}
	}
}

func TestDashboardNodeBatchCopyContract(t *testing.T) {
	for _, want := range []string{
		`id="node-select-page"`,
		`data-action="node-select"`,
		`data-action="copy-selected-nodes"`,
		`function toggleNodeSelection(button)`,
		`function toggleNodePageSelection(button)`,
		`function copySelectedNodes(button)`,
		`urls.join('\n')`,
		`case 'node-select':`,
		`case 'node-select-page':`,
		`case 'copy-selected-nodes':`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing node batch copy contract %q", want)
		}
	}
}

func TestDashboardNodeBatchSpeedtestContract(t *testing.T) {
	for _, want := range []string{
		`data-action="speedtest-selected-nodes"`,
		`function speedtestSelectedNodes()`,
		`case 'speedtest-selected-nodes':`,
		`/api/nodes/speedtest/batch`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing node batch speedtest contract %q", want)
		}
	}
}

func TestDashboardNodeAvailabilityFilterContract(t *testing.T) {
	for _, want := range []string{
		`data-action="filter-node-availability"`,
		`data-availability="available"`,
		`data-availability="unavailable"`,
		`function filterNodeAvailability(`,
		`function updateNodeAvailabilityButtons()`,
		`case 'filter-node-availability':`,
		`q.push('unavailable=1')`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing node availability filter contract %q", want)
		}
	}
}

func TestDashboardCandidatePageSizeIsResponsive(t *testing.T) {
	for _, want := range []string{
		`<option value="10">每页10</option>`,
		`<option value="20">每页20</option>`,
		`<option value="50" selected>每页50</option>`,
		`<option value="100">每页100</option>`,
		"return compactViewport() ? 10 : 50;",
		"return compactViewport() ? 10 : 20;",
		"var nodePageSize = defaultNodePageSize();",
		"var candidatePageSize = defaultCandidatePageSize();",
		"candidatePageSize = Math.max(1, Math.min(100, candidatePageSize));",
		"syncNodePageSizeSelect();\nsyncCandidatePageSizeSelect();\nsyncFailedPageSizeSelect();\nsyncProxyIPPageSizeSelect();\nsyncTabFromHash();",
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing responsive candidate page-size contract %q", want)
		}
	}
}

func TestDashboardCandidatePagerIsAvailableAtTopOnMobile(t *testing.T) {
	for _, want := range []string{
		`id="candidate-pager-top"`,
		`id="node-pager-top"`,
		`.candidate-pager-top{display:none}`,
		`.candidate-pager-top:empty{display:none}`,
		`.candidate-pager-top,.node-pager-top{display:flex;position:sticky;top:8px`,
		"renderCandidatePagers(",
		"renderNodePagers(",
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing mobile candidate pager contract %q", want)
		}
	}
}

func TestDashboardUsesNewResponsiveApplicationShell(t *testing.T) {
	for _, want := range []string{
		`<div class="app-shell">`,
		`<aside class="sidebar" aria-label="主导航">`,
		`<main id="main-content" class="main-shell" tabindex="-1">`,
		`<a class="skip-link" href="#main-content">`,
		`.app-shell{display:grid;grid-template-columns:252px minmax(0,1fr)`,
		`.sidebar{position:fixed;top:auto;bottom:0;left:0;right:0`,
		`data-view="candidates"`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing application-shell contract %q", want)
		}
	}
}

func TestDashboardMobileCardsUseProgressiveDisclosure(t *testing.T) {
	for _, want := range []string{
		`var expandedNodeRows = Object.create(null);`,
		`var expandedCandidateRows = Object.create(null);`,
		`function toggleNodeDetails(button)`,
		`function toggleCandidateDetails(button)`,
		`tr:not(.mobile-expanded) td.mobile-secondary`,
		`class="mobile-detail-toggle"`,
		`data-action="details"`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing mobile progressive-disclosure contract %q", want)
		}
	}
}

func TestDashboardScheduleControlsAndTimelineContract(t *testing.T) {
	for _, want := range []string{
		`id="opt-source-refresh-interval"`,
		`id="opt-full-recheck-interval"`,
		`id="timeline-source-last"`,
		`id="timeline-source-next"`,
		`id="timeline-full-last"`,
		`id="timeline-full-next"`,
		`source_refresh_interval_seconds: sourceRefreshSeconds`,
		`full_recheck_interval_seconds: fullRecheckSeconds`,
		`overrides.source_refresh_interval_seconds`,
		`overrides.full_recheck_interval_seconds`,
		`d.last_source_refresh_at`,
		`d.next_source_refresh_at`,
		`d.last_full_recheck_at`,
		`d.next_full_recheck_at`,
		`连续 3 次完整全检失败后移出转发池，候选库记录仍保留`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing schedule-control contract %q", want)
		}
	}
}

func TestDashboardScheduleRoundTripPreservesNinetySeconds(t *testing.T) {
	for _, want := range []string{
		`function secondsFromMinutes(id)`,
		`return isFinite(minutes) ? Math.round(minutes * 60) : -1;`,
		`var sourceRefreshSeconds = secondsFromMinutes('opt-source-refresh-interval');`,
		`var fullRecheckSeconds = secondsFromMinutes('opt-full-recheck-interval');`,
		`id="opt-source-refresh-interval" type="number" min="1" max="10080" step="0.1"`,
		`id="opt-full-recheck-interval" type="number" min="1" max="10080" step="0.1"`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing lossless schedule conversion contract %q", want)
		}
	}
}

func TestDashboardExplainsBaselineAndAutomaticRecoveryAccurately(t *testing.T) {
	for _, want := range []string{
		`基线是本机直连出口 IP，用于与代理出口比较并判断是否真的换了 IP。`,
		`不会继续进入轻量自动复检，但仍会参加周期性的完整全检。`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing health explanation %q", want)
		}
	}
}

func TestDashboardHasVisibleAsyncAndErrorStates(t *testing.T) {
	for _, want := range []string{
		`id="node-notice"`,
		`id="candidate-notice"`,
		`id="failed-notice"`,
		`id="proxyip-notice"`,
		`function setListNotice(id, tone, message)`,
		`正在获取代理池分页数据`,
		`正在查询完整候选快照`,
		`已保留上一次成功加载的内容`,
		`id="toast-region"`,
		`id="result-overlay"`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing async/error-state contract %q", want)
		}
	}
}

func TestDashboardModalIsOutsideInertAppAndTrapsFocus(t *testing.T) {
	appClose := strings.Index(dashboardHTML, "</main>\n</div>")
	modal := strings.Index(dashboardHTML, `id="candidate-country-modal"`)
	if appClose < 0 || modal < 0 || modal < appClose {
		t.Fatal("country modal must be a sibling after the inert app shell")
	}
	for _, want := range []string{
		`app.inert = true`,
		`function trapModalFocus(event)`,
		`trapModalFocus(e);`,
		`data-action="proxyip-verify"`,
		`restoreCandidateFocus(savedFocus)`,
		`restoreNodeFocus(savedFocus)`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing modal/async focus contract %q", want)
		}
	}
}

func TestDashboardKeepsLargePoolPaginationOnStableSnapshots(t *testing.T) {
	for _, want := range []string{
		`var nodeSnapshotID = '';`,
		`var candidateSnapshotID = '';`,
		`var failedSnapshotID = '';`,
		`var proxyipSnapshotID = '';`,
		`q.push('snapshot_id=' + encodeURIComponent(nodeSnapshotID))`,
		`q.push('snapshot_id=' + encodeURIComponent(candidateSnapshotID))`,
		`q.push('snapshot_id=' + encodeURIComponent(failedSnapshotID))`,
		`q.push('snapshot_id=' + encodeURIComponent(proxyipSnapshotID))`,
		`err.code === 'snapshot_changed'`,
		`代理池已更新，正在从新快照第一页继续浏览`,
		`候选目录已生成新快照，正在从第一页继续浏览`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing stable-snapshot pagination contract %q", want)
		}
	}
}

func TestDashboardTracksObservableRefreshJobs(t *testing.T) {
	for _, want := range []string{
		`fetchJSON('/api/refresh/status')`,
		`function refreshJobFromState(state, id)`,
		`operation.status === 'queued'`,
		`operation.status === 'running'`,
		`部分来源失败，旧候选已保留`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing observable refresh-job contract %q", want)
		}
	}
}

func TestDashboardSeparatesDestructivePoolMaintenance(t *testing.T) {
	for _, want := range []string{
		`<details class="danger-zone">`,
		`维护与危险操作`,
		`永久清理全部不可用节点`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing separated destructive action %q", want)
		}
	}
}

func TestDashboardPrivateSourceEscapeHatchIsExplicitAndOffByDefault(t *testing.T) {
	for _, want := range []string{
		`id="source-allow-private" name="allow_private" type="checkbox"`,
		`允许访问私网 / 保留地址（高风险）`,
		`公网来源必须保持关闭`,
		`allow_private: !!f.allow_private.checked`,
		`{{if .AllowPrivate}}<span class="private-source-badge"`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing explicit private-source opt-in contract %q", want)
		}
	}
	if strings.Contains(dashboardClientSource(), `id="source-allow-private" name="allow_private" type="checkbox" checked`) {
		t.Fatal("private-source opt-in must be off by default")
	}
}

func TestDashboardAuthoritativeEmptySourceOptInIsExplicitAndOffByDefault(t *testing.T) {
	for _, want := range []string{
		`id="source-allow-empty" name="allow_empty" type="checkbox"`,
		`允许“权威空列表”（可能清空该来源候选）`,
		`不再保留上一版可用候选`,
		`allow_empty: !!f.allow_empty.checked`,
		`{{if .AllowEmpty}}<span class="empty-source-badge"`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing explicit authoritative-empty opt-in contract %q", want)
		}
	}
	if strings.Contains(dashboardClientSource(), `id="source-allow-empty" name="allow_empty" type="checkbox" checked`) {
		t.Fatal("authoritative-empty opt-in must be off by default")
	}
}

func TestDashboardUsesTaskScopedResourceWorkflow(t *testing.T) {
	for _, want := range []string{
		`class="resource-rail"`,
		`class="catalog-workflow"`,
		`资源类型 → 来源地区 → 节点结果`,
		`Cloudflare ProxyIP`,
		`仅取纯 IP`,
		`不是 <code>host&amp;port=1080&amp;user=...&amp;pass=...</code>`,
		`.task-metrics-candidate`,
		`href="#failed-candidates"`,
		`href="#proxyip"`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing task-scoped resource workflow %q", want)
		}
	}
	for _, removed := range []string{
		`data-action="show-candidate-protocol"`,
		`完整只读目录`,
	} {
		if strings.Contains(dashboardClientSource(), removed) {
			t.Fatalf("dashboard still treats ProxyIP/failed nodes as candidate filters %q", removed)
		}
	}
	if strings.Contains(string(dashboardJS), `function showCandidateProtocol(`) {
		t.Fatal("ProxyIP must be reached through its own tab, not a candidate protocol shortcut")
	}
}

func TestDashboardProxyIPVerifyIsExplicitResourceOnlyAction(t *testing.T) {
	for _, want := range []string{
		`var proxyIPVerifyCache = Object.create(null);`,
		`fetchJSON('/api/proxyip/verify', {`,
		`body:JSON.stringify({key:key})`,
		`data-action="proxyip-verify"`,
		`if (String(protocol || '').toLowerCase() !== 'proxyip')`,
		`proxyIPVerifyCache[key].state === 'loading'`,
		`document.querySelectorAll('#proxyip-tbody tr[data-key]')`,
		`button.closest('#proxyip-tbody tr[data-key]')`,
		`IPv4：`,
		`IPv6：`,
		`仅供 Cloudflare Worker ProxyIP 参考 · 资源/代理池状态不变`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing ProxyIP verification contract %q", want)
		}
	}
	if got := strings.Count(dashboardClientSource(), "fetchJSON('/api/proxyip/verify'"); got != 1 {
		t.Fatalf("ProxyIP verify endpoint call count = %d, want one explicit action path", got)
	}
	if strings.Contains(string(dashboardJS), `proxyIPVerifyCellHTML(candidate.key, candidate.protocol)`) {
		t.Fatal("ordinary candidate rows must not render ProxyIP verify cells; verify lives on the ProxyIP tab only")
	}
}

func TestDashboardCandidateManagementKeepsPendingColumnShape(t *testing.T) {
	for _, want := range []string{
		`id="candidate-select-page"`,
		`<th><input id="candidate-select-page" type="checkbox" data-action="candidate-select-page" aria-label="选择本页候选"></th><th>协议</th><th>候选地址</th>`,
		`<th>测速结果</th><th>操作</th>`,
		`data-action="candidate-speedtest-selected"`,
		`data-action="candidate-delete-selected"`,
		`fetchJSON('/api/candidates/speedtest'`,
		`fetchJSON('/api/candidates/delete'`,
		`人工测速不受冷却限制；成功后立即加入转发池，正式检查失败的会移入失败节点页。`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing candidate management contract %q", want)
		}
	}
	// 用户名/密码 were dropped as separate columns: proxy_url already embeds
	// credentials when present (see TestDashboardShowsUpstreamCredentialsInURL),
	// so the dedicated columns were pure duplication that inflated the table
	// past its available width on ordinary desktop viewports. The 状态 column
	// is gone too: this page only lists pending candidates by definition.
	if got := strings.Count(dashboardClientSource(), `<td colspan="7" class="empty">`); got != 3 {
		t.Fatalf("candidate seven-column empty-state colspan count = %d, want 3", got)
	}
	if strings.Contains(dashboardHTML, `<th>状态</th><th>协议</th><th>候选地址</th>`) {
		t.Fatal("pending candidate table must not keep a per-row status column")
	}
}

func TestDashboardWiresInventoryManagementActions(t *testing.T) {
	for _, want := range []string{
		`data-action="refresh-source"`,
		`fetchJSON('/api/sources/refresh'`,
		`data-action="save-source-auto-refresh"`,
		`fetchJSON('/api/sources/auto-refresh'`,
		`auto_refresh_enabled`,
		`refresh_interval_seconds`,
		`data-action="delete-node"`,
		`fetchJSON('/api/nodes/delete'`,
		`case 'verify': runVerify(actionElement);`,
		`case 'candidate-select': toggleCandidateSelection(actionElement);`,
		`最多选择 16 个不同 key`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing inventory management action %q", want)
		}
	}
}

func TestDashboardManualVerifyShowsImmediateFailureFiltering(t *testing.T) {
	for _, want := range []string{
		`function manualVerifyObservationSummary(result)`,
		`本次连通尝试：`,
		`当前节点状态：`,
		`健康失败观察：`,
		`本次手动复检未能连通目标。`,
		`内部最多尝试 3 次`,
		`最终失败后节点已立即从可路由池过滤`,
		`不会继续进入轻量自动复检`,
		`仍会参加周期性的完整全检`,
		`typeof result.attempts === 'number'`,
		`typeof result.available === 'boolean'`,
		`typeof result.consecutive_failures === 'number'`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing immediate manual-failure contract %q", want)
		}
	}
}

func TestDashboardDisablesSwitchForUnavailableButKeepsRecoveryAction(t *testing.T) {
	for _, want := range []string{
		`var switchAction = n.available === false`,
		`data-action="switch" disabled aria-label="节点 `,
		`当前不可用，不能切换`,
		`title="当前不可用；可先点击验证，恢复后再切换"`,
		`data-action="verify" title="立即重新拨号,查看真实出口IP/国家是否和标签一致"`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing unavailable-switch recovery contract %q", want)
		}
	}
}

func TestDashboardManualVerifyHandlesUnknownLabelMatch(t *testing.T) {
	unknownGuard := `if (j.label_match_known === false)`
	legacyGuard := `else if (!j.label_matched)`
	unknownText := `缺少可比较的有效地区标签，无法判断是否一致；若本次获取到新地区，已正常保存。`
	for _, want := range []string{unknownGuard, legacyGuard, unknownText, `✅ 与列表标签一致。`} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing manual verify label-match compatibility contract %q", want)
		}
	}
	unknownIndex := strings.Index(dashboardClientSource(), unknownGuard)
	legacyIndex := strings.Index(dashboardClientSource(), legacyGuard)
	if unknownIndex < 0 || legacyIndex < 0 || unknownIndex > legacyIndex {
		t.Fatal("label_match_known=false must take precedence over the legacy label_matched branch")
	}
}

// TestDashboardShowsUpstreamCredentialsInURL guards the shape that replaced
// dedicated 用户名/密码 columns: ConsumerURL/proxy_url already embeds
// user:pass@host:port when credentials exist (see proxy.go urlWithScheme),
// so a separate username/password column showed nothing a row without
// credentials didn't already show blank, and duplicated what a row with
// credentials already showed inline. Candidates additionally get an explicit
// 需认证 badge instead of a blank/filled password column.
func TestDashboardShowsUpstreamCredentialsInURL(t *testing.T) {
	for _, want := range []string{
		`String(candidate.proxy_url || candidate.addr || '')`,
		`candidate.has_auth ? '<span class="auth-badge"`,
		`escapeHtml(n.proxy_url || n.addr)`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing upstream credential display contract %q", want)
		}
	}
	for _, mustNotHave := range []string{
		`<th>用户名</th><th>密码</th>`,
		`escapeHtml(candidate.username || '')`,
		`escapeHtml(candidate.password || '')`,
		`escapeHtml(n.username || '')`,
		`escapeHtml(n.password || '')`,
	} {
		if strings.Contains(dashboardClientSource(), mustNotHave) {
			t.Fatalf("dashboard still has a redundant dedicated credential column %q (credentials already show via proxy_url)", mustNotHave)
		}
	}
}

func TestDashboardSelectionControlsAndColumnsContract(t *testing.T) {
	for _, want := range []string{
		`<th><input id="node-select-page" type="checkbox" data-action="node-select-page" aria-label="选择本页节点"></th><th>状态</th><th>协议</th>`,
		`case 'node-select': toggleNodeSelection(actionElement); return;`,
		`case 'node-select-page': toggleNodePageSelection(actionElement); return;`,
		`case 'candidate-select': toggleCandidateSelection(actionElement); return;`,
		`case 'candidate-select-page': toggleCandidatePageSelection(actionElement); return;`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard selection controls or column alignment violates contract: missing %q", want)
		}
	}
}

func TestDashboardManagementPagesMatchBackendDataContracts(t *testing.T) {
	for _, want := range []string{
		`<select id="default-group-select">`,
		`{{range .StatusSummary.Groups}}`,
		`data-label="成员数 / 当前"`,
		`fetchJSON('/api/nodes/page?page=1&page_size=100&available=1')`,
		`requestListenerNodeKeys()`,
		`Promise.all([fetchJSON('/api/listeners'), requestListenerGroups(), requestListenerNodeKeys()])`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard management page contract is missing %q", want)
		}
	}
}

func TestDashboardImportsLocalSourceWithoutLeakingFileMetadata(t *testing.T) {
	for _, want := range []string{
		`id="form-import-source"`,
		`id="source-import-name" name="name"`,
		`id="source-import-file" name="file" type="file"`,
		`最多 16 MiB`,
		`固定按 text-regex`,
		`不会保存或返回原始文件名和路径`,
		`每轮只按 MaxCandidates 有界抽样检查`,
		`var formData = new FormData();`,
		`formData.append('name', name);`,
		`formData.append('file', file);`,
		`fetchJSON('/api/sources/import', {method:'POST', body:formData})`,
		`id="source-import-status"`,
		`{{if eq .Kind "upload"}}本地导入{{else if .Builtin}}内置{{else}}远程自定义{{end}}`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing local source import contract %q", want)
		}
	}
	if strings.Contains(string(dashboardJS), `fetchJSON('/api/sources/import', {method:'POST', headers:`) {
		t.Fatal("local source import must let the browser set the multipart Content-Type boundary")
	}
	for _, leakedField := range []string{"filename", "filepath", "path", "proxy_url", "username", "password"} {
		if strings.Contains(string(dashboardJS), `result.`+leakedField) {
			t.Fatalf("local source import result renders sensitive field %q", leakedField)
		}
	}
}

func TestDashboardSeparatesPendingFailedAndProxyIPTabs(t *testing.T) {
	for _, want := range []string{
		`id="tab-link-failed-candidates" href="#failed-candidates"`,
		`id="tab-link-proxyip" href="#proxyip"`,
		`id="tab-failed-candidates" class="tab-panel"`,
		`id="tab-proxyip" class="tab-panel"`,
		`var validTabs = ['nodes','candidates','failed-candidates','proxyip','sources','rules','groups','listeners'];`,
		`'failed-candidates': ['失败节点'`,
		`'proxyip': ['Cloudflare ProxyIP'`,
		`if (validTabs.indexOf(requested) < 0)`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing separated pending/failed/proxyip tab contract %q", want)
		}
	}
}

func TestDashboardCandidateBatchCheckUsesAsyncStatusAPI(t *testing.T) {
	for _, want := range []string{
		`id="candidate-batch-limit"`,
		`data-action="candidate-batch-check"`,
		`id="candidate-operation-status"`,
		`fetchJSON('/api/candidates/batch-check', {`,
		`body:JSON.stringify({limit:limit})`,
		`function pollCandidateCheckOperation(`,
		`case 'candidate-batch-check': startCandidateBatchCheck(actionElement); break;`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing candidate batch-check async contract %q", want)
		}
	}
}

func TestDashboardFailedPageFiltersSelectsAndRetries(t *testing.T) {
	for _, want := range []string{
		`id="failed-tbody"`,
		`id="fc-text"`,
		`id="fc-source"`,
		`id="fc-proto"`,
		`id="fc-failure-type"`,
		`id="fc-pagesize"`,
		`id="failed-select-page"`,
		`data-action="failed-retry-selected"`,
		`id="failed-retry-all-button"`,
		`data-action="failed-retry-all"`,
		`id="failed-operation-status"`,
		`id="failed-notice"`,
		`<th>错误摘要</th>`,
		`失败节点不会自动重新检测`,
		`function failedPageURL()`,
		`return '/api/failed-candidates?' + q.join('&');`,
		`fetchJSON('/api/failed-candidates/retry', {`,
		`body:JSON.stringify({keys:keys})`,
		`body: JSON.stringify({all: true})`,
		`function retryFailedCandidates(`,
		`function retryAllFailedCandidates(`,
		`case 'failed-retry-selected': retryFailedCandidates(); break;`,
		`case 'failed-retry-all': retryAllFailedCandidates(); break;`,
		`case 'failed-select': toggleFailedSelection(actionElement); return;`,
		`case 'failed-select-page': toggleFailedPageSelection(actionElement); return;`,
		`case 'goto-failed-page':`,
		`populateCandidateFacetSelect('fc-failure-type', failedFacetList('failure_types'), '全部失败类型')`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing failed-candidate page contract %q", want)
		}
	}
	if strings.Contains(dashboardClientSource(), `data-action="failed-auto-retry"`) {
		t.Fatal("failed candidates must never get an automatic retry switch; retry is manual only")
	}
}

func TestDashboardExposesAutoCandidateCheckOptions(t *testing.T) {
	for _, want := range []string{
		`id="opt-auto-check"`,
		`id="opt-auto-check-interval"`,
		`auto_candidate_check:`,
		`auto_check_interval_seconds: autoIntervalSeconds`,
		`result.auto_candidate_check !== false`,
		`restoreInFlightCandidateCheck`,
		`['queued', 'running'].indexOf(operation.status) < 0`,
		`function cancelCandidateCheck()`,
		`case 'cancel-candidate-check': cancelCandidateCheck(); break;`,
		`task-panel-bar`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing automatic candidate check option contract %q", want)
		}
	}
}

func TestDashboardCandidateAndFailedTasksDisableDuplicateSubmission(t *testing.T) {
	for _, want := range []string{
		`err.status === 409 && err.code === 'candidate_check_busy'`,
		`['complete','cancelled','superseded','failed'].indexOf(operation.status) >= 0`,
		`function setCandidateCheckButtonsDisabled(`,
		`setTimeout(checkCandidateCheckOperation, 1200)`,
	} {
		if !strings.Contains(string(dashboardJS), want) {
			t.Fatalf("dashboard is missing duplicate-submission guard %q", want)
		}
	}
}

func TestDashboardProxyIPUsesDedicatedPageAPI(t *testing.T) {
	for _, want := range []string{
		`id="proxyip-tbody"`,
		`id="px-country"`,
		`data-action="open-proxyip-country-picker"`,
		`function proxyIPPageURL()`,
		`return '/api/proxyip/page?' + q.join('&');`,
		`function requestProxyIPs(`,
		`case 'open-proxyip-country-picker': openProxyIPCountryPicker(); break;`,
		`countryPickerScope = 'proxyip'`,
		`case 'goto-proxyip-page':`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing dedicated ProxyIP page contract %q", want)
		}
	}
}

func TestDashboardRefreshesAllThreeCountsAfterManualTask(t *testing.T) {
	for _, want := range []string{
		`function refreshAfterCandidateCheck()`,
		`requestStatus();`,
		`requestCandidates(true);`,
		`requestFailedCandidates(true);`,
		`requestProxyIPs(true);`,
		`typeof d.failed_candidate_total === 'number'`,
		`setText('tab-link-failed-candidates', '失败节点 (' + formatCount(failedTotal) + ')');`,
		`setText('tab-link-proxyip', 'ProxyIP (' + formatCount(proxyIPTotal) + ')');`,
	} {
		if !strings.Contains(string(dashboardJS), want) {
			t.Fatalf("dashboard is missing post-task three-count refresh %q", want)
		}
	}
}
