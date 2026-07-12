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
		"syncNodePageSizeSelect();\nsyncCandidatePageSizeSelect();\nsyncTabFromHash();",
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

func TestDashboardHasVisibleAsyncAndErrorStates(t *testing.T) {
	for _, want := range []string{
		`id="node-notice"`,
		`id="candidate-notice"`,
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
		`q.push('snapshot_id=' + encodeURIComponent(nodeSnapshotID))`,
		`q.push('snapshot_id=' + encodeURIComponent(candidateSnapshotID))`,
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
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing task-scoped resource workflow %q", want)
		}
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
	guard := strings.Index(dashboardClientSource(), "if (String(protocol || '').toLowerCase() !== 'proxyip')")
	button := strings.Index(dashboardClientSource(), `data-action="proxyip-verify"`)
	if guard < 0 || button < 0 || guard > button {
		t.Fatalf("ordinary candidates are not guarded before rendering the ProxyIP verify button")
	}
}

func TestDashboardCandidateVerifyColumnKeepsTableShape(t *testing.T) {
	if !strings.Contains(dashboardClientSource(), `<thead><tr><th>状态</th><th>协议</th><th>候选地址</th><th>来源标注地区</th><th>来源</th><th>专用验证</th></tr></thead>`) {
		t.Fatal("candidate verify header is missing")
	}
	if strings.Contains(dashboardClientSource(), `colspan="5"`) {
		t.Fatal("candidate empty rows still use the old five-column colspan")
	}
	if got := strings.Count(dashboardClientSource(), `colspan="6"`); got != 3 {
		t.Fatalf("candidate six-column empty-state colspan count = %d, want 3", got)
	}
}

func TestDashboardManualVerifyShowsDebouncedHealthObservation(t *testing.T) {
	for _, want := range []string{
		`function manualVerifyObservationSummary(result)`,
		`本次连通尝试：`,
		`当前节点状态：`,
		`连续失败观察：`,
		`本次手动复检未能连通目标。`,
		`内部最多 3 次连通尝试`,
		`只记为 1 次健康观察`,
		`连续 3 次失败观察才会下线`,
		`typeof result.attempts === 'number'`,
		`typeof result.available === 'boolean'`,
		`typeof result.consecutive_failures === 'number'`,
	} {
		if !strings.Contains(dashboardClientSource(), want) {
			t.Fatalf("dashboard is missing manual verify observation text/compatibility guard %q", want)
		}
	}
	if strings.Contains(dashboardClientSource(), `可能已失效`) {
		t.Fatal("dashboard still describes one failed manual verification as a possibly dead node")
	}
}

func TestDashboardDisablesSwitchForUnavailableButKeepsRecoveryAction(t *testing.T) {
	for _, want := range []string{
		`var switchAction = n.available === false`,
		`data-action="switch" disabled aria-label="节点 `,
		`当前不可用，不能切换`,
		`title="当前不可用；可先点击验证，恢复后再切换"`,
		`data-action="verify" onclick="runVerify(this)"`,
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
