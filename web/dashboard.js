function fetchJSON(url, options) {
  options = options || {};
  var method = String(options.method || 'GET').toUpperCase();
  if (['POST', 'PUT', 'DELETE', 'PATCH'].indexOf(method) >= 0) {
    var token = (document.querySelector('meta[name="csrf-token"]') || {}).content;
    if (token) {
      var headers = new Headers(options.headers || {});
      headers.set('X-CSRF-Token', token);
      options = Object.assign({}, options, {headers:headers});
    }
  }
  return fetch(url, options).then(function(r) {
    return r.text().then(function(text) {
      var data = {};
      if (text) {
        try { data = JSON.parse(text); }
        catch (e) {
          if (r.ok) throw new Error('服务器返回了无法解析的数据');
        }
      }
      if (!r.ok) {
        var detail = data && data.error;
        if (detail && typeof detail === 'object') detail = detail.message || detail.code || JSON.stringify(detail);
        var requestError = new Error(detail || ('请求失败 (HTTP ' + r.status + ')'));
        requestError.status = r.status;
        requestError.code = data && data.code ? data.code : '';
        requestError.requestId = data && data.request_id ? data.request_id : '';
        throw requestError;
      }
      return data;
    });
  });
}

function postJSON(url, body, cb) {
  fetchJSON(url, {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify(body)})
    .then(function(){ cb(null); })
    .catch(function(err){ cb(String(err)); });
}
function notify(message, tone, duration) {
  var region = document.getElementById('toast-region');
  if (!region) return;
  var toast = document.createElement('div');
  toast.className = 'toast ' + (tone || '');
  toast.textContent = String(message || '操作完成');
  region.appendChild(toast);
  setTimeout(function(){ if (toast.parentNode) toast.parentNode.removeChild(toast); }, duration || 4500);
}

var resultDialogFocus = null;
var activeModalOverlay = null;

function modalFocusableElements(overlay) {
  if (!overlay) return [];
  var selector = 'a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])';
  return Array.prototype.slice.call(overlay.querySelectorAll(selector)).filter(function(el) {
    return !el.hidden && el.getAttribute('aria-hidden') !== 'true' && el.getClientRects().length > 0;
  });
}

function activateModal(overlay, initialFocus) {
  activeModalOverlay = overlay;
  var app = document.querySelector('.app-shell');
  if (app) {
    app.setAttribute('aria-hidden', 'true');
    app.inert = true;
  }
  document.body.classList.add('modal-open');
  setTimeout(function() {
    var focusTarget = initialFocus || modalFocusableElements(overlay)[0];
    if (focusTarget && focusTarget.focus) focusTarget.focus();
  }, 0);
}

function deactivateModal(overlay) {
  if (activeModalOverlay !== overlay) return;
  activeModalOverlay = null;
  var app = document.querySelector('.app-shell');
  if (app) {
    app.inert = false;
    app.removeAttribute('aria-hidden');
  }
  document.body.classList.remove('modal-open');
}

function trapModalFocus(event) {
  if (!activeModalOverlay || event.key !== 'Tab') return;
  var focusable = modalFocusableElements(activeModalOverlay);
  if (!focusable.length) {
    event.preventDefault();
    return;
  }
  var first = focusable[0], last = focusable[focusable.length - 1];
  if (event.shiftKey && (document.activeElement === first || !activeModalOverlay.contains(document.activeElement))) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault();
    first.focus();
  }
}

function showResultDialog(title, message) {
  var overlay = document.getElementById('result-overlay');
  if (!overlay) { alert(message); return; }
  resultDialogFocus = document.activeElement;
  setText('result-dialog-title', title || '操作结果');
  setText('result-dialog-body', message || '');
  overlay.hidden = false;
  var close = overlay.querySelector('.result-dialog-close');
  activateModal(overlay, close);
}

function closeResultDialog() {
  var overlay = document.getElementById('result-overlay');
  if (!overlay || overlay.hidden) return;
  overlay.hidden = true;
  deactivateModal(overlay);
  if (resultDialogFocus && typeof resultDialogFocus.focus === 'function') resultDialogFocus.focus();
  resultDialogFocus = null;
}

function resultDialogBackdrop(event) {
  if (event && event.target && event.target.id === 'result-overlay') closeResultDialog();
}

function reloadOrAlert(err) { if (err) { notify(err, 'error', 7000); } else { location.reload(); } }

function setListNotice(id, tone, message) {
  var el = document.getElementById(id);
  if (!el) return;
  el.hidden = !message;
  el.dataset.tone = tone || '';
  el.textContent = message || '';
}

function escapeHtml(s) { var d = document.createElement('div'); d.textContent = s == null ? '' : s; return d.innerHTML; }

function renderGroups(groups) {
  var container = document.getElementById('group-cards-container');
  if (!container) return;
  var html = '';
  groups.forEach(function(g) {
    var cur = g.current ? ('当前: ' + escapeHtml(g.current) + (g.dynamic ? ' <span class="cn-meta">每连接轮换</span>' : '')) : '暂无可用节点';
    html += '<div class="group-card"><div class="gc-name">' + escapeHtml(g.name) + '</div>' +
      '<div class="gc-strategy">' + escapeHtml(g.strategy) + '</div>' +
      '<div class="gc-count">' + g.count + ' 节点</div>' +
      '<div class="gc-current">' + cur + '</div></div>';
  });
  html += '<div class="group-card direct"><div class="gc-name">DIRECT</div><div class="gc-strategy">直连,不经过代理</div></div>';
  container.innerHTML = html;
}

function protoBadge(p) {
  var protocol = String(p || '').toLowerCase();
  var label = protocol === 'https' ? 'https（HTTP CONNECT）' : protocol;
  var title = protocol === 'https' ? '来源协议标签为 https；连接代理本身使用 http://' : '';
  return '<span class="proto proto-' + escapeHtml(protocol) + '"' + (title ? ' title="' + escapeHtml(title) + '"' : '') + '>' + escapeHtml(label) + '</span>';
}

function anonBadge(a) {
  var label = {elite:'高匿', anonymous:'普通', transparent:'透明'}[a] || '未知';
  var cls = a && ['elite','anonymous','transparent'].indexOf(a) >= 0 ? a : 'unknown';
  return '<span class="anon anon-' + cls + '">' + label + '</span>';
}
function scoreCell(s) {
  var v = Math.round(s || 0);
  var cls = v >= 70 ? 'score-hi' : (v >= 45 ? 'score-mid' : 'score-lo');
  return '<span class="score ' + cls + '">' + v + '</span>';
}

function formatBytes(bytes) {
  var n = Number(bytes || 0);
  if (!isFinite(n) || n <= 0) return '0 B';
  if (n >= 1024 * 1024) return (n / (1024 * 1024)).toFixed(1) + ' MB';
  if (n >= 1024) return Math.round(n / 1024) + ' KB';
  return Math.round(n) + ' B';
}

function formatCount(value) {
  var n = Number(value || 0);
  return isFinite(n) ? Math.max(0, Math.round(n)).toLocaleString('zh-CN') : '0';
}

function speedCell(n) {
  var testedAt = Number(n.speed_tested_at || 0);
  if (!isFinite(testedAt) || testedAt <= 0) return '<span class="small">未测速</span>';
  var date = new Date(testedAt * 1000);
  var testedText = isNaN(date.getTime()) ? '时间未知' : date.toLocaleString('zh-CN', {hour12:false});
  var bytesText = formatBytes(n.speed_bytes);
  var duration = Number(n.speed_duration_ms || 0);
  var durationText = isFinite(duration) && duration > 0 ? Math.round(duration) + ' ms' : '耗时未知';
  var speed = Number(n.speed_kbps || 0);
  var speedText = (isFinite(speed) ? Math.round(speed) : 0) + ' kbps';
  var title = '最近测速：' + testedText + '；样本：' + bytesText + '；耗时：' + durationText;
  return '<span title="' + escapeHtml(title) + '">' + speedText + '</span>';
}

function addressHost(addr) {
  addr = String(addr || '');
  if (addr.charAt(0) === '[') {
    var close = addr.indexOf(']');
    return close > 0 ? addr.slice(1, close) : addr;
  }
  var colon = addr.lastIndexOf(':');
  return colon > 0 ? addr.slice(0, colon) : addr;
}

// The dashboard deliberately keeps only the current server-provided page.
// Large retained pools are filtered/sorted by /api/nodes/page, rather than
// downloading every node into the browser.
var nodePageData = null;
var nodePage = 1;
var nodePageSize = defaultNodePageSize();
var nodePageSizeTouched = false;
var nodeSnapshotID = '';
var anyPinned = false;
var nodesLoaded = false;
var currentTab = 'nodes';
var statusRequest = null;
var nodesRequest = null;
var nodesAbortController = null;
var pollTimer = null;
var refreshPollTimer = null;
var healthRecheckPollTimer = null;
var nodeFilterTimer = null;
var nodeQueryGeneration = 0;
var lastNodesFetchAt = 0;
var candidatePageData = null;
var candidatePage = 1;
var candidatePageSize = defaultCandidatePageSize();
var candidatePageSizeTouched = false;
var candidateSnapshotID = '';
var candidatesLoaded = false;
var candidatesRequest = null;
var candidatesAbortController = null;
var candidateFilterTimer = null;
var candidateQueryGeneration = 0;
var lastCandidatesFetchAt = 0;
var failedPageData = null;
var failedPage = 1;
var failedPageSize = defaultCandidatePageSize();
var failedPageSizeTouched = false;
var failedSnapshotID = '';
var failedLoaded = false;
var failedRequest = null;
var failedAbortController = null;
var failedFilterTimer = null;
var failedQueryGeneration = 0;
var lastFailedFetchAt = 0;
var proxyipPageData = null;
var proxyipPage = 1;
var proxyipPageSize = defaultCandidatePageSize();
var proxyipPageSizeTouched = false;
var proxyipSnapshotID = '';
var proxyipLoaded = false;
var proxyipRequest = null;
var proxyipAbortController = null;
var proxyipFilterTimer = null;
var proxyipQueryGeneration = 0;
var lastProxyipFetchAt = 0;
var candidateCheckPollTimer = null;
var candidateCheckOperationID = '';
var candidateCheckActive = false;
var proxyIPVerifyCache = Object.create(null);
var expandedNodeRows = Object.create(null);
var expandedCandidateRows = Object.create(null);
var candidateContinentFilter = '';
var candidateCountryTrigger = null;
var countryPickerScope = 'candidates';
var selectedCandidateKeys = Object.create(null);
var selectedFailedKeys = Object.create(null);
var selectedNodeURLs = Object.create(null);
var nodeSpeedResults = Object.create(null);
var nodeSpeedtestPending = false;
var candidateSpeedResults = Object.create(null);
var candidateOperationPending = false;
var lastKnownScrape = '';
var lastKnownNextScrape = '';
var lastCompactViewport = compactViewport();
var viewportPageSizeTimer = null;

function compactViewport() {
  return typeof window.matchMedia === 'function' && window.matchMedia('(max-width:700px)').matches;
}

function defaultNodePageSize() {
  return compactViewport() ? 10 : 20;
}

function defaultCandidatePageSize() {
  return compactViewport() ? 10 : 50;
}

function syncNodePageSizeSelect() {
  var select = document.getElementById('f-pagesize');
  if (select) select.value = String(nodePageSize);
}

function syncCandidatePageSizeSelect() {
  var select = document.getElementById('cf-pagesize');
  if (select) select.value = String(candidatePageSize);
}

function syncFailedPageSizeSelect() {
  var select = document.getElementById('fc-pagesize');
  if (select) select.value = String(failedPageSize);
}

function syncProxyIPPageSizeSelect() {
  var select = document.getElementById('px-pagesize');
  if (select) select.value = String(proxyipPageSize);
}

function applyResponsiveCatalogPageSizes() {
  var compact = compactViewport();
  if (compact === lastCompactViewport) return;
  lastCompactViewport = compact;
  if (!nodePageSizeTouched) {
    nodePageSize = defaultNodePageSize();
    nodePage = 1;
    nodeSnapshotID = '';
    nodeQueryGeneration++;
    syncNodePageSizeSelect();
    if (currentTab === 'nodes') requestNodes(true);
  }
  if (!candidatePageSizeTouched) {
    candidatePageSize = defaultCandidatePageSize();
    candidatePage = 1;
    candidateSnapshotID = '';
    candidateQueryGeneration++;
    syncCandidatePageSizeSelect();
    if (currentTab === 'candidates') requestCandidates(true);
  }
  if (!failedPageSizeTouched) {
    failedPageSize = defaultCandidatePageSize();
    failedPage = 1;
    failedSnapshotID = '';
    failedQueryGeneration++;
    syncFailedPageSizeSelect();
    if (currentTab === 'failed-candidates') requestFailedCandidates(true);
  }
  if (!proxyipPageSizeTouched) {
    proxyipPageSize = defaultCandidatePageSize();
    proxyipPage = 1;
    proxyipSnapshotID = '';
    proxyipQueryGeneration++;
    syncProxyIPPageSizeSelect();
    if (currentTab === 'proxyip') requestProxyIPs(true);
  }
}

// inFlightOps tracks per-node async button state (key -> {speedtest?:true,
// verify?:true}) so a node-data refresh rebuilding the table (applyNodeView
// replaces tbody.innerHTML wholesale) doesn't silently reset a "测速中.../
// 验证中..." button back to its default clickable state mid-request - the
// row re-renders itself as disabled again on every rebuild as long as the
// operation is still in flight.
var inFlightOps = {};
function markOp(key, op, on) {
  if (on) {
    inFlightOps[key] = inFlightOps[key] || {};
    inFlightOps[key][op] = true;
  } else if (inFlightOps[key]) {
    delete inFlightOps[key][op];
    if (!Object.keys(inFlightOps[key]).length) delete inFlightOps[key];
  }
}

// flagEmoji converts a 2-letter ISO country code to its flag emoji via the
// regional-indicator-symbol algorithm (each letter maps to U+1F1E6 plus its
// offset from 'A') - no per-country lookup table needed, works for any
// valid ISO 3166-1 alpha-2 code. Same trick EDT-Pages' own admin panel
// effectively achieves via a static country_emoji field in its data feed;
// computing it means we don't depend on that field being present.
function flagEmoji(cc) {
  if (!cc || cc.length !== 2) return '🏳️';
  var upper = cc.toUpperCase();
  var c0 = upper.charCodeAt(0), c1 = upper.charCodeAt(1);
  if (c0 < 65 || c0 > 90 || c1 < 65 || c1 > 90) return '🏳️';
  return String.fromCodePoint(0x1F1E6 + (c0 - 65), 0x1F1E6 + (c1 - 65));
}

function normalizedCountry(country) {
  var c = String(country || '').trim().toUpperCase();
  return /^[A-Z]{2}$/.test(c) ? c : '';
}

var regionDisplayNames = null;
try {
  if (typeof Intl === 'object' && typeof Intl.DisplayNames === 'function') regionDisplayNames = new Intl.DisplayNames(['zh-CN'], {type:'region'});
} catch (e) {}
var countryNameFallback = {
  CN:'中国',HK:'中国香港',MO:'中国澳门',TW:'中国台湾',JP:'日本',KR:'韩国',SG:'新加坡',IN:'印度',ID:'印度尼西亚',MY:'马来西亚',TH:'泰国',VN:'越南',PH:'菲律宾',KH:'柬埔寨',BD:'孟加拉国',PK:'巴基斯坦',AE:'阿联酋',SA:'沙特阿拉伯',TR:'土耳其',IL:'以色列',IR:'伊朗',IQ:'伊拉克',KZ:'哈萨克斯坦',
  US:'美国',CA:'加拿大',MX:'墨西哥',BR:'巴西',AR:'阿根廷',CL:'智利',CO:'哥伦比亚',PE:'秘鲁',
  GB:'英国',DE:'德国',FR:'法国',NL:'荷兰',BE:'比利时',CH:'瑞士',AT:'奥地利',ES:'西班牙',PT:'葡萄牙',IT:'意大利',PL:'波兰',CZ:'捷克',RO:'罗马尼亚',UA:'乌克兰',RU:'俄罗斯',SE:'瑞典',NO:'挪威',FI:'芬兰',DK:'丹麦',IE:'爱尔兰',GR:'希腊',
  AU:'澳大利亚',NZ:'新西兰',ZA:'南非',EG:'埃及',NG:'尼日利亚',KE:'肯尼亚',MA:'摩洛哥'
};
function countryNameZH(country) {
  var c = normalizedCountry(country);
  if (!c) return '未知';
  if (regionDisplayNames) {
    try { var named = regionDisplayNames.of(c); if (named && named !== c) return named; } catch (e) {}
  }
  return countryNameFallback[c] || c;
}
function countryLabel(country) {
  var c = normalizedCountry(country);
  return c ? (flagEmoji(c) + ' ' + c + ' ' + countryNameZH(c)) : '🏳️ 国家未知';
}

// continentInfo maps ip-api.com's continentCode (AS/NA/EU/AF/SA/OC/AN,
// stamped on every node's .continent by the same LookupGeo call that sets
// .country) to a display emoji+name - the same 7-continent scheme
// EDT-Pages' own admin panel groups its region picker by.
var continentInfo = {
  AS: { emoji: '🌏', name: '亚洲' },
  NA: { emoji: '🌎', name: '北美' },
  EU: { emoji: '🌍', name: '欧洲' },
  AF: { emoji: '🌍', name: '非洲' },
  SA: { emoji: '🌎', name: '南美' },
  OC: { emoji: '🌏', name: '大洋洲' },
  AN: { emoji: '❄️', name: '南极洲' }
};
var continentOrder = ['AS', 'EU', 'NA', 'SA', 'OC', 'AF', 'AN', ''];

// countryToContinent is a static ISO 3166-1 alpha-2 -> continent-code
// fallback, used only when a node's .continent is empty (its Country came
// straight from a source feed like EDT-Pages/ProxyIP, which supplies a
// country but not a continent, so it never went through our own LookupGeo
// call). Covers the UN member states plus common territories; anything
// missing just falls into the "未知地区" group instead of erroring.
var countryToContinent = {
  // Asia
  CN:'AS',HK:'AS',MO:'AS',TW:'AS',JP:'AS',KR:'AS',KP:'AS',MN:'AS',
  IN:'AS',PK:'AS',BD:'AS',LK:'AS',NP:'AS',BT:'AS',MV:'AS',
  ID:'AS',MY:'AS',SG:'AS',TH:'AS',VN:'AS',PH:'AS',MM:'AS',KH:'AS',LA:'AS',BN:'AS',TL:'AS',
  SA:'AS',AE:'AS',IL:'AS',IQ:'AS',IR:'AS',JO:'AS',KW:'AS',LB:'AS',OM:'AS',PS:'AS',QA:'AS',SY:'AS',YE:'AS',BH:'AS',TR:'AS',
  KZ:'AS',KG:'AS',TJ:'AS',TM:'AS',UZ:'AS',AF:'AS',AM:'AS',AZ:'AS',GE:'AS',CY:'AS',
  // Europe
  GB:'EU',IE:'EU',FR:'EU',DE:'EU',NL:'EU',BE:'EU',LU:'EU',CH:'EU',AT:'EU',
  ES:'EU',PT:'EU',IT:'EU',MT:'EU',SM:'EU',VA:'EU',AD:'EU',MC:'EU',
  PL:'EU',CZ:'EU',SK:'EU',HU:'EU',RO:'EU',BG:'EU',SI:'EU',HR:'EU',BA:'EU',RS:'EU',ME:'EU',MK:'EU',AL:'EU',XK:'EU',
  DK:'EU',SE:'EU',NO:'EU',FI:'EU',IS:'EU',EE:'EU',LV:'EU',LT:'EU',
  RU:'EU',UA:'EU',BY:'EU',MD:'EU',GR:'EU',LI:'EU',
  // North America (incl. Central America & Caribbean)
  US:'NA',CA:'NA',MX:'NA',GT:'NA',BZ:'NA',SV:'NA',HN:'NA',NI:'NA',CR:'NA',PA:'NA',
  CU:'NA',JM:'NA',HT:'NA',DO:'NA',BS:'NA',BB:'NA',TT:'NA',GD:'NA',LC:'NA',VC:'NA',AG:'NA',DM:'NA',KN:'NA',
  PR:'NA',
  // South America
  BR:'SA',AR:'SA',CL:'SA',CO:'SA',PE:'SA',VE:'SA',EC:'SA',BO:'SA',PY:'SA',UY:'SA',GY:'SA',SR:'SA',
  // Africa
  EG:'AF',LY:'AF',TN:'AF',DZ:'AF',MA:'AF',SD:'AF',SS:'AF',
  NG:'AF',GH:'AF',CI:'AF',SN:'AF',ML:'AF',BF:'AF',NE:'AF',TD:'AF',TG:'AF',BJ:'AF',GN:'AF',SL:'AF',LR:'AF',GM:'AF',GW:'AF',MR:'AF',CV:'AF',
  KE:'AF',TZ:'AF',UG:'AF',RW:'AF',BI:'AF',ET:'AF',SO:'AF',DJ:'AF',ER:'AF',
  ZA:'AF',NA:'AF',BW:'AF',ZW:'AF',ZM:'AF',MW:'AF',MZ:'AF',AO:'AF',SZ:'AF',LS:'AF',MG:'AF',MU:'AF',SC:'AF',KM:'AF',
  CM:'AF',CF:'AF',CG:'AF',CD:'AF',GA:'AF',GQ:'AF',ST:'AF',
  // Oceania
  AU:'OC',NZ:'OC',PG:'OC',FJ:'OC',SB:'OC',VU:'OC',NC:'OC',PF:'OC',WS:'OC',TO:'OC',KI:'OC',FM:'OC',PW:'OC',MH:'OC',NR:'OC',TV:'OC',GU:'OC'
};

// Both catalog scopes use the custom continent/country dialog. The known
// pool supplies measured exit geography while the candidate inventory
// supplies source-declared geography, so their labels and counts stay
// deliberately distinct.
function countrySummaries() {
  return nodePageData && Array.isArray(nodePageData.countries) ? nodePageData.countries : [];
}

function populateCountrySelect() {
  updateNodeCountryButton();
}

function candidateFacetList(name) {
  return candidatePageData && Array.isArray(candidatePageData[name]) ? candidatePageData[name] : [];
}

function populateCandidateFacetSelect(id, items, emptyLabel) {
  var sel = document.getElementById(id);
  if (!sel) return;
  var cur = sel.value;
  sel.innerHTML = '';
  var empty = document.createElement('option');
  empty.value = '';
  empty.textContent = emptyLabel;
  sel.appendChild(empty);
  items.forEach(function(item) {
    var value = String((item && item.value) || '').trim();
    if (!value) return;
    var option = document.createElement('option');
    option.value = value;
    option.textContent = value + '（' + formatCount(item.total || 0) + '）';
    sel.appendChild(option);
  });
  if (cur && !Array.prototype.some.call(sel.options, function(o){ return o.value === cur; })) {
    var selectedOption = document.createElement('option');
    selectedOption.value = cur;
    selectedOption.textContent = cur;
    sel.appendChild(selectedOption);
  }
  if (cur) sel.value = cur;
}

function candidateProtocolCount(protocol) {
  var total = 0;
  candidateFacetList('protocols').forEach(function(item) {
    if (String(item.value || '').toLowerCase() === protocol) total = Number(item.total || 0);
  });
  return total;
}

function chooseCandidateProtocol(protocol) {
  var sel = document.getElementById('cf-proto');
  if (!sel) return;
  sel.value = sel.value === protocol ? '' : protocol;
  onCandidateFilterChange();
}

function renderCandidateProtocolCards() {
  var container = document.getElementById('candidate-protocol-cards');
  if (!container) return;
  var selected = (document.getElementById('cf-proto') || {}).value || '';
  var cards = [
    {value:'socks5', label:'SOCKS5', note:'可进入本地转发池'},
    {value:'http', label:'HTTP', note:'可进入本地转发池'},
    {value:'https', label:'HTTP CONNECT', note:'来源标签 https · 复制为可连接的 http://'}
  ];
  container.innerHTML = cards.map(function(card) {
    var count = candidateProtocolCount(card.value);
    return '<button type="button" class="protocol-card' + (selected === card.value ? ' active' : '') + '" data-action="choose-candidate-protocol" data-protocol="' + escapeHtml(card.value) + '" aria-pressed="' + (selected === card.value ? 'true' : 'false') + '">' +
      '<strong>' + card.label + '</strong><span>' + formatCount(count) + '</span><small>' + card.note + '</small></button>';
  }).join('');
}

function candidateCountrySummaries() {
  return candidateFacetList('countries').map(function(item) {
    return {
      country: normalizedCountry(item && item.country),
      continent: String((item && item.continent) || '').toUpperCase(),
      total: Math.max(0, Number((item && item.total) || 0))
    };
  }).filter(function(item){ return !!item.country; });
}

function candidateUnknownCountryTotal() {
  return Math.max(0, Number((candidatePageData && candidatePageData.country_unknown_total) || 0));
}

function proxyipFacetList(name) {
  return proxyipPageData && Array.isArray(proxyipPageData[name]) ? proxyipPageData[name] : [];
}

function failedFacetList(name) {
  return failedPageData && Array.isArray(failedPageData[name]) ? failedPageData[name] : [];
}

function proxyipCountrySummaries() {
  return proxyipFacetList('countries').map(function(item) {
    return {
      country: normalizedCountry(item && item.country),
      continent: String((item && item.continent) || '').toUpperCase(),
      total: Math.max(0, Number((item && item.total) || 0))
    };
  }).filter(function(item){ return !!item.country; });
}

function proxyipUnknownCountryTotal() {
  return Math.max(0, Number((proxyipPageData && proxyipPageData.country_unknown_total) || 0));
}

function nodeCountryPickerSummaries() {
  return countrySummaries().map(function(item) {
    return {
      country: normalizedCountry(item && item.country),
      continent: String((item && item.continent) || '').toUpperCase(),
      total: Math.max(0, Number((item && item.total) || 0)),
      available: Math.max(0, Number((item && item.available) || 0))
    };
  }).filter(function(item){ return !!item.country; });
}

function nodeUnknownCountryCounts() {
  var poolTotal = Math.max(0, Number((nodePageData && nodePageData.pool_total) || 0));
  var availableTotal = Math.max(0, Number((nodePageData && nodePageData.available_total) || 0));
  var locatedTotal = 0, locatedAvailable = 0;
  nodeCountryPickerSummaries().forEach(function(item) {
    locatedTotal += item.total;
    locatedAvailable += item.available;
  });
  return {total:Math.max(0, poolTotal - locatedTotal), available:Math.max(0, availableTotal - locatedAvailable)};
}

function pickerCountrySummaries() {
  if (countryPickerScope === 'nodes') return nodeCountryPickerSummaries();
  if (countryPickerScope === 'proxyip') return proxyipCountrySummaries();
  return candidateCountrySummaries();
}

function pickerUnknownCountryCounts() {
  if (countryPickerScope === 'nodes') return nodeUnknownCountryCounts();
  if (countryPickerScope === 'proxyip') return {total:proxyipUnknownCountryTotal(), available:0};
  return {total:candidateUnknownCountryTotal(), available:0};
}

function pickerCountLabel(counts) {
  return countryPickerScope === 'nodes'
    ? (formatCount(counts.available || 0) + ' / ' + formatCount(counts.total || 0))
    : (formatCount(counts.total || 0) + ' 条');
}

function candidateContinentCounts() {
  var counts = {};
  pickerCountrySummaries().forEach(function(item) {
    var continent = item.continent || countryToContinent[item.country] || '';
    if (!counts[continent]) counts[continent] = {total:0,available:0};
    counts[continent].total += item.total;
    counts[continent].available += Number(item.available || 0);
  });
  counts.unknown = pickerUnknownCountryCounts();
  return counts;
}

function setCandidateContinentFilter(continent) {
  candidateContinentFilter = candidateContinentFilter === continent ? '' : continent;
  renderCandidateCountryPicker();
}

function renderCandidateCountryPicker() {
  var map = document.getElementById('candidate-continent-map');
  var list = document.getElementById('candidate-country-list');
  if (!map || !list) return;
  var counts = candidateContinentCounts();
  var definitions = [
    {code:'NA', cls:'na', label:'🌎 北美'}, {code:'SA', cls:'sa', label:'🌎 南美'},
    {code:'EU', cls:'eu', label:'🌍 欧洲'}, {code:'AS', cls:'as', label:'🌏 亚洲'},
    {code:'AF', cls:'af', label:'🌍 非洲'}, {code:'OC', cls:'oc', label:'🌏 大洋洲'},
    {code:'AN', cls:'an', label:'❄️ 南极洲'}, {code:'unknown', cls:'unknown', label:'🏳️ 国家未知'}
  ];
  map.innerHTML = definitions.map(function(item) {
    return '<button type="button" class="continent-tile continent-' + escapeHtml(item.cls) + (candidateContinentFilter === item.code ? ' active' : '') + '" data-action="set-candidate-continent" data-continent="' + escapeHtml(item.code) + '">' +
      '<strong>' + item.label + '</strong><span>' + pickerCountLabel(counts[item.code] || {}) + '</span></button>';
  }).join('');

  var query = String((document.getElementById('candidate-country-search') || {}).value || '').trim().toUpperCase();
  var inputId = countryPickerScope === 'nodes' ? 'f-country' : (countryPickerScope === 'proxyip' ? 'px-country' : 'cf-country');
  var selected = String((document.getElementById(inputId) || {}).value || '');
  var groups = {};
  pickerCountrySummaries().forEach(function(item) {
    var continent = item.continent || countryToContinent[item.country] || '';
    if (candidateContinentFilter && candidateContinentFilter !== continent) return;
    if (query && (item.country + ' ' + countryNameZH(item.country)).toUpperCase().indexOf(query) < 0) return;
    if (!groups[continent]) groups[continent] = [];
    groups[continent].push(item);
  });
  Object.keys(groups).forEach(function(continent) {
    groups[continent].sort(function(a,b){ return Number(b.available || 0) - Number(a.available || 0) || b.total - a.total || a.country.localeCompare(b.country); });
  });

  var html = '';
  var shown = 0;
  continentOrder.forEach(function(continent) {
    var items = groups[continent] || [];
    if (!items.length) return;
    var info = continentInfo[continent];
    var title = info ? (info.emoji + ' ' + info.name + ' / ' + continent) : '🏳️ 未知大洲';
    var groupCounts = items.reduce(function(sum,item){ sum.total += item.total; sum.available += Number(item.available || 0); return sum; }, {total:0,available:0});
    html += '<div class="country-continent-group"><div class="country-continent-title"><span>' + title + '</span><span>' + pickerCountLabel(groupCounts) + '</span></div>';
    items.forEach(function(item) {
      shown++;
      html += '<button type="button" class="country-option' + (selected === item.country ? ' active' : '') + '" data-action="choose-candidate-country" data-country="' + escapeHtml(item.country) + '">' +
        '<span aria-hidden="true">' + flagEmoji(item.country) + '</span><span class="country-option-code">' + item.country + ' ' + escapeHtml(countryNameZH(item.country)) + '</span><span class="country-option-count">' + pickerCountLabel(item) + '</span></button>';
    });
    html += '</div>';
  });
  var unknown = pickerUnknownCountryCounts();
  if ((!candidateContinentFilter || candidateContinentFilter === 'unknown') && (!query || 'UNKNOWN 国家未知 尚未定位'.indexOf(query) >= 0)) {
    shown++;
    html += '<div class="country-continent-group"><div class="country-continent-title"><span>🏳️ 国家未知</span><span>' + pickerCountLabel(unknown) + '</span></div>' +
      '<button type="button" class="country-option' + (selected === '__unknown__' ? ' active' : '') + '" data-action="choose-candidate-country" data-country="__unknown__"><span aria-hidden="true">🏳️</span><span class="country-option-code">尚未定位</span><span class="country-option-count">' + pickerCountLabel(unknown) + '</span></button></div>';
  }
  list.innerHTML = html || '<div class="country-option-empty">没有匹配的国家/地区</div>';
  setText('candidate-country-result-count', shown + ' 个地区');
}

function updateCandidateCountryButton() {
  var value = String((document.getElementById('cf-country') || {}).value || '');
  var button = document.getElementById('cf-country-button');
  if (!button) return;
  button.textContent = value === '__unknown__' ? '来源地区未知' : (value ? countryLabel(value) : '全部来源地区');
}

function updateProxyIPCountryButton() {
  var value = String((document.getElementById('px-country') || {}).value || '');
  var button = document.getElementById('px-country-button');
  if (!button) return;
  button.textContent = 'Cloudflare ProxyIP · ' + (value === '__unknown__' ? '来源地区未知' : (value ? countryLabel(value) : '全部来源地区'));
}

function updateNodeCountryButton() {
  var value = String((document.getElementById('f-country') || {}).value || '');
  var button = document.getElementById('f-country-button');
  if (!button) return;
  button.textContent = value === '__unknown__' ? '🏳️ 实测出口国家未知' : (value ? countryLabel(value) : '🗺️ 全部实测出口国家');
}

function openNodeCountryPicker() {
  countryPickerScope = 'nodes';
  openCountryPicker();
}

function openCandidateCountryPicker() {
  countryPickerScope = 'candidates';
  openCountryPicker();
}

function openProxyIPCountryPicker() {
  countryPickerScope = 'proxyip';
  openCountryPicker();
}

function openCountryPicker() {
  var modal = document.getElementById('candidate-country-modal');
  if (!modal) return;
  candidateCountryTrigger = document.activeElement;
  if (candidateCountryTrigger && candidateCountryTrigger.setAttribute) candidateCountryTrigger.setAttribute('aria-expanded', 'true');
  candidateContinentFilter = '';
  var search = document.getElementById('candidate-country-search');
  if (search) search.value = '';
  var title = document.getElementById('candidate-country-title');
  var mapTitle = document.getElementById('country-picker-map-title');
  var note = document.getElementById('country-picker-note');
  var allButton = document.getElementById('country-picker-all');
  if (countryPickerScope === 'nodes') {
    if (title) title.textContent = '🗺️ 按实测出口国家浏览代理池';
    if (mapTitle) mapTitle.textContent = '每个数量均为“当前可用 / 池内总数”';
    if (note) note.textContent = '这里使用节点通过代理拨号后实测到的出口地区；它可能与节点服务器地址所属地区不同。';
    if (allButton) allButton.textContent = '全部实测出口';
  } else if (countryPickerScope === 'proxyip') {
    if (title) title.textContent = '选择 Cloudflare ProxyIP 来源地区';
    if (mapTitle) mapTitle.textContent = '先选大洲，再选国家或地区';
    if (note) note.textContent = '只浏览端口集合含 443 的纯 IP 资源；它不接受 SOCKS/HTTP 的端口与认证参数。';
    if (allButton) allButton.textContent = '全部来源地区';
  } else {
    if (title) title.textContent = '选择候选来源地区';
    if (mapTitle) mapTitle.textContent = '先选大洲，再选国家或地区';
    if (note) note.textContent = '地区来自来源元数据，不等于经过代理拨号实测的出口地区。';
    if (allButton) allButton.textContent = '全部来源地区';
  }
  renderCandidateCountryPicker();
  modal.hidden = false;
  activateModal(modal, search);
}

function closeCandidateCountryPicker() {
  var modal = document.getElementById('candidate-country-modal');
  if (!modal || modal.hidden) return;
  modal.hidden = true;
  deactivateModal(modal);
  if (candidateCountryTrigger && candidateCountryTrigger.setAttribute) candidateCountryTrigger.setAttribute('aria-expanded', 'false');
  if (candidateCountryTrigger && candidateCountryTrigger.focus) candidateCountryTrigger.focus();
}

function candidateCountryBackdrop(event) {
  if (event && event.target === document.getElementById('candidate-country-modal')) closeCandidateCountryPicker();
}

function chooseCandidateCountry(country) {
  var scope = countryPickerScope;
  var input = document.getElementById(scope === 'nodes' ? 'f-country' : (scope === 'proxyip' ? 'px-country' : 'cf-country'));
  if (!input) return;
  input.value = country;
  if (scope === 'nodes') updateNodeCountryButton();
  else if (scope === 'proxyip') updateProxyIPCountryButton();
  else updateCandidateCountryButton();
  closeCandidateCountryPicker();
  if (scope === 'nodes') onFilterChange();
  else if (scope === 'proxyip') onProxyIPFilterChange();
  else onCandidateFilterChange();
}

function formatCandidateUpdatedAt(value) {
  if (!value) return '';
  var date;
  if (typeof value === 'number') date = new Date(value * 1000);
  else date = new Date(value);
  return date && !isNaN(date.getTime()) ? date.toLocaleString('zh-CN', {hour12:false}) : String(value);
}

function proxyIPVerifyFriendlyError(err) {
  var message = String(err && err.message ? err.message : (err || '')).replace(/^Error:\s*/, '');
  if ((err && err.name === 'AbortError') || /取消|cancelled|canceled/i.test(message)) return '验证已取消，可按需重试';
  if (/deadline|timeout|超时|HTTP 504/i.test(message)) return '外部验证服务响应超时，可稍后重试';
  if (/Failed to fetch|NetworkError|网络|ProxyIP 验证服务|HTTP 5\d\d/i.test(message)) return '外部验证服务暂时不可用，可稍后重试';
  return message || '验证失败，可稍后重试';
}

function proxyIPVerifyCellHTML(key, protocol) {
  if (String(protocol || '').toLowerCase() !== 'proxyip') return '<span class="small" aria-label="不适用">—</span>';
  key = String(key || '');
  var result = proxyIPVerifyCache[key] || null;
  var safeKey = escapeHtml(key);
  var note = '<span class="proxyip-verify-note">仅供 Cloudflare Worker ProxyIP 参考 · 资源/代理池状态不变</span>';
  var buttonLabel = !result ? '专用验证' : (result.state === 'error' ? '重试' : '重新验证');
  var button = '<button type="button" class="btn-sm" data-action="proxyip-verify" aria-label="' + escapeHtml(buttonLabel + ' ' + key) + '">' + escapeHtml(buttonLabel) + '</button>';
  if (!result) return '<div class="proxyip-verify"><div class="proxyip-verify-actions">' + button + '</div>' + note + '</div>';
  if (result.state === 'loading') {
    return '<div class="proxyip-verify"><div class="proxyip-verify-actions"><button type="button" class="btn-sm" data-action="proxyip-verify" aria-disabled="true">验证中…</button>' +
      '<span class="proxyip-verify-state" role="status" aria-live="polite">正在调用外部专用验证服务</span></div>' + note + '</div>';
  }
  if (result.state === 'error') {
    return '<div class="proxyip-verify"><div class="proxyip-verify-summary" role="status" aria-live="polite"><span class="proxyip-verify-state proxyip-verify-error">验证失败：' + escapeHtml(result.message) + '</span></div>' +
      '<div class="proxyip-verify-actions">' + button + '</div>' + note + '</div>';
  }
  var available = result.success === true;
  var statusClass = available ? 'proxyip-verify-ok' : 'proxyip-verify-unavailable';
  var statusText = available ? '专用验证可用' : '专用验证不可用';
  var latency = Math.max(0, Math.round(Number(result.response_time_ms) || 0));
  var checkedAt = formatCandidateUpdatedAt(result.checked_at);
  var title = '外部验证来源：' + String(result.source || '未知') + (checkedAt ? '；时间：' + checkedAt : '');
  return '<div class="proxyip-verify"><div class="proxyip-verify-summary" role="status" aria-live="polite" title="' + escapeHtml(title) + '">' +
    '<span class="proxyip-verify-state ' + statusClass + '">' + statusText + '</span>' +
    '<span class="proxyip-verify-latency">延迟 ' + latency + ' ms</span>' +
    '<span class="proxyip-verify-support">IPv4：' + (result.supports_ipv4 ? '支持' : '不支持') + '</span>' +
    '<span class="proxyip-verify-support">IPv6：' + (result.supports_ipv6 ? '支持' : '不支持') + '</span></div>' +
    '<div class="proxyip-verify-actions">' + button + '</div>' + note + '</div>';
}

function renderProxyIPVerifyCell(key) {
  var rows = document.querySelectorAll('#proxyip-tbody tr[data-key]');
  for (var i = 0; i < rows.length; i++) {
    if (rows[i].getAttribute('data-key') !== key) continue;
    var cell = rows[i].querySelector('.candidate-verify-cell');
    if (cell) {
      var restoreFocus = cell.contains(document.activeElement);
      cell.innerHTML = proxyIPVerifyCellHTML(key, 'proxyip');
      if (restoreFocus) {
        var action = cell.querySelector('[data-action="proxyip-verify"]');
        if (action) action.focus();
      }
    }
  }
}

function runProxyIPVerify(button) {
  var row = button && button.closest ? button.closest('#proxyip-tbody tr[data-key]') : null;
  var key = row ? String(row.getAttribute('data-key') || '') : '';
  if (key.indexOf('proxyip://') !== 0) return;
  if (proxyIPVerifyCache[key] && proxyIPVerifyCache[key].state === 'loading') return;
  proxyIPVerifyCache[key] = {state:'loading'};
  renderProxyIPVerifyCell(key);
  fetchJSON('/api/proxyip/verify', {
    method:'POST',
    headers:{'Content-Type':'application/json'},
    body:JSON.stringify({key:key})
  }).then(function(result) {
    var latency = Number(result && result.response_time_ms);
    if (!result || typeof result.success !== 'boolean' || !isFinite(latency) || latency < 0 ||
        typeof result.supports_ipv4 !== 'boolean' || typeof result.supports_ipv6 !== 'boolean') {
      throw new Error('验证服务返回结果不完整');
    }
    proxyIPVerifyCache[key] = {
      state:'complete',
      success:result.success,
      response_time_ms:latency,
      supports_ipv4:result.supports_ipv4,
      supports_ipv6:result.supports_ipv6,
      source:String(result.source || ''),
      checked_at:String(result.checked_at || '')
    };
  }).catch(function(err) {
    proxyIPVerifyCache[key] = {state:'error', message:proxyIPVerifyFriendlyError(err)};
  }).finally(function() {
    renderProxyIPVerifyCell(key);
  });
}

function onCandidatePageFetched(pageData) {
  var previousSnapshotID = candidateSnapshotID;
  candidatePageData = pageData && typeof pageData === 'object' ? pageData : {};
  if (!Array.isArray(candidatePageData.candidates)) candidatePageData.candidates = [];
  ['sources','protocols','countries'].forEach(function(name) {
    if (!Array.isArray(candidatePageData[name])) candidatePageData[name] = [];
  });
  candidatePage = Number(candidatePageData.page) > 0 ? Number(candidatePageData.page) : 1;
  candidateSnapshotID = String(candidatePageData.snapshot_id || '');
  if (previousSnapshotID && candidateSnapshotID && previousSnapshotID !== candidateSnapshotID) {
    selectedCandidateKeys = Object.create(null);
    candidateSpeedResults = Object.create(null);
  }
  var validKeys = candidatePageData.all_keys;
  if (Array.isArray(validKeys)) {
    var valid = Object.create(null);
    validKeys.forEach(function(key){ valid[String(key)] = true; });
    selectedCandidateList().forEach(function(key){ if (!valid[key]) { delete selectedCandidateKeys[key]; delete candidateSpeedResults[key]; } });
  }
  var returnedPageSize = Number(candidatePageData.page_size) > 0 ? Number(candidatePageData.page_size) : candidatePageSize;
  var responsivePageSize = defaultCandidatePageSize();
  if (!candidatePageSizeTouched && returnedPageSize !== responsivePageSize) {
    candidatePage = 1;
    candidatePageSize = responsivePageSize;
    candidateSnapshotID = '';
    candidateQueryGeneration++;
    queuedCandidateRefresh = true;
    syncCandidatePageSizeSelect();
    setListNotice('candidate-notice', 'loading', '正在按当前屏幕尺寸调整每页数量…');
    return;
  }
  candidatePageSize = returnedPageSize;
  syncCandidatePageSizeSelect();
  setListNotice('candidate-notice', '', '');
  candidatesLoaded = true;
  populateCandidateFacetSelect('cf-source', candidateFacetList('sources'), '全部来源');
  var protocols = candidateFacetList('protocols').slice();
  ['socks5','http','https'].forEach(function(value) {
    if (!protocols.some(function(item){ return String(item.value || '').toLowerCase() === value; })) protocols.push({value:value,total:0});
  });
  populateCandidateFacetSelect('cf-proto', protocols, '全部协议');
  renderCandidateProtocolCards();
  updateCandidateCountryButton();
  applyCandidateView();
  var countryModal = document.getElementById('candidate-country-modal');
  if (countryPickerScope === 'candidates' && countryModal && !countryModal.hidden) renderCandidateCountryPicker();
}

function onCandidateFilterChange() {
  candidatePage = 1;
  candidateQueryGeneration++;
  renderCandidateProtocolCards();
  updateCandidateCountryButton();
  if (candidateFilterTimer) clearTimeout(candidateFilterTimer);
  setText('candidate-count', '正在应用筛选…');
  candidateFilterTimer = setTimeout(function() {
    candidateFilterTimer = null;
    requestCandidates(true);
  }, 250);
}

function onCandidatePageSizeChange() {
  candidatePageSize = parseInt(document.getElementById('cf-pagesize').value, 10) || defaultCandidatePageSize();
  candidatePageSize = Math.max(1, Math.min(100, candidatePageSize));
  candidatePageSizeTouched = true;
  candidatePage = 1;
  candidateQueryGeneration++;
  requestCandidates(true);
}

function gotoCandidatePage(page) {
  candidatePage = Math.max(1, Number(page) || 1);
  candidateQueryGeneration++;
  requestCandidates(true);
}

function toggleCandidateDetails(button) {
  var row = button && button.closest ? button.closest('tr[data-key]') : null;
  if (!row) return;
  var key = row.getAttribute('data-key') || '';
  var expanded = !row.classList.contains('mobile-expanded');
  row.classList.toggle('mobile-expanded', expanded);
  if (expanded) expandedCandidateRows[key] = true;
  else delete expandedCandidateRows[key];
  button.setAttribute('aria-expanded', expanded ? 'true' : 'false');
  button.textContent = expanded ? '收起' : '详情';
}

function captureCandidateFocus() {
  var el = document.activeElement;
  if (!el) return null;
  var row = el.closest ? el.closest('#candidate-tbody tr[data-key]') : null;
  if (row && el.getAttribute('data-action')) {
    return {key:row.getAttribute('data-key'), action:el.getAttribute('data-action')};
  }
  var pager = el.closest ? el.closest('#candidate-pager,#candidate-pager-top') : null;
  if (pager && el.getAttribute('data-action')) {
    return {pager:el.getAttribute('data-action'), top:pager.id === 'candidate-pager-top'};
  }
  return null;
}

function restoreCandidateFocus(saved) {
  if (!saved) return;
  var el = null;
  if (saved.key) {
    var rows = document.querySelectorAll('#candidate-tbody tr[data-key]');
    for (var i = 0; i < rows.length; i++) {
      if (rows[i].getAttribute('data-key') === saved.key) {
        el = rows[i].querySelector('[data-action="' + saved.action + '"]');
        break;
      }
    }
  } else if (saved.pager) {
    var pagerID = saved.top ? '#candidate-pager-top' : '#candidate-pager';
    el = document.querySelector(pagerID + ' [data-action="' + saved.pager + '"]');
  }
  if (el && !el.disabled) el.focus();
  else {
    var fallback = document.querySelector('.candidate-table-scroll');
    if (fallback) fallback.focus();
  }
}

function selectedCandidateList() { return Object.keys(selectedCandidateKeys); }

function updateCandidateSelectionUI() {
  var keys = selectedCandidateList();
  setText('candidate-selected-count', '已选 ' + keys.length + ' / 16');
  var speedButton = document.querySelector('[data-action="candidate-speedtest-selected"]');
  var deleteButton = document.querySelector('[data-action="candidate-delete-selected"]');
  if (speedButton) speedButton.disabled = candidateOperationPending || !keys.length;
  if (deleteButton) deleteButton.disabled = candidateOperationPending || !keys.length;
  var pageToggle = document.getElementById('candidate-select-page');
  var rows = candidatePageData && Array.isArray(candidatePageData.candidates) ? candidatePageData.candidates : [];
  if (pageToggle) {
    var selectedOnPage = rows.filter(function(item){ return !!selectedCandidateKeys[String(item.key || '')]; }).length;
    pageToggle.checked = !!rows.length && selectedOnPage === rows.length;
    pageToggle.indeterminate = selectedOnPage > 0 && selectedOnPage < rows.length;
  }
}

function toggleCandidateSelection(button) {
  var key = rowKey(button);
  if (!key) return;
  if (button.checked) {
    if (!selectedCandidateKeys[key] && selectedCandidateList().length >= 16) {
      button.checked = false;
      notify('候选测速一次最多选择 16 个不同 key', 'error', 7000);
      return;
    }
    selectedCandidateKeys[key] = true;
  } else delete selectedCandidateKeys[key];
  updateCandidateSelectionUI();
}

function toggleCandidatePageSelection(button) {
  var rows = candidatePageData && Array.isArray(candidatePageData.candidates) ? candidatePageData.candidates : [];
  if (!button.checked) {
    rows.forEach(function(item){ delete selectedCandidateKeys[String(item.key || '')]; });
  } else {
    var available = 16 - selectedCandidateList().length;
    var added = 0;
    rows.forEach(function(item) {
      var key = String(item.key || '');
      if (key && !selectedCandidateKeys[key] && added < available) { selectedCandidateKeys[key] = true; added++; }
    });
    if (rows.some(function(item){ return !selectedCandidateKeys[String(item.key || '')]; })) notify('最多选择 16 个不同 key，已选到上限', 'error', 7000);
  }
  applyCandidateView();
}

function candidateResultText(key) {
  var result = candidateSpeedResults[key];
  if (!result) return '<span class="candidate-readonly">尚未测速</span>';
  if (result.pending) return '<span class="candidate-speed-pending">测速中…</span>';
  if (result.error) {
    var errorMessage = typeof result.error === 'object' ? (result.error.message || result.error.code || JSON.stringify(result.error)) : result.error;
    return '<span class="candidate-speed-error" role="status">失败：' + escapeHtml(errorMessage) + '</span>';
  }
  var duration = Number(result.duration_ms);
  var speed = Number(result.kbps);
  var parts = [];
  if (isFinite(duration)) parts.push('用时 ' + Math.round(duration) + ' ms');
  if (isFinite(speed)) parts.push(formatCount(Math.round(speed)) + ' KB/s');
  return '<span class="candidate-speed-success" role="status">' + escapeHtml(parts.join(' · ') || '测速完成') + '</span>';
}

function normalizeCandidateResults(payload, keys) {
  var rows = payload && Array.isArray(payload.results) ? payload.results : [];
  var byKey = Object.create(null);
  rows.forEach(function(item){ if (item && item.key) byKey[String(item.key)] = item; });
  keys.forEach(function(key) {
    var item = byKey[key];
    if (!item) candidateSpeedResults[key] = {error:'服务端未返回该项结果'};
    else if (item.error || item.ok === false) candidateSpeedResults[key] = {error:item.error || '测速失败'};
    else candidateSpeedResults[key] = item;
  });
}

function speedtestCandidates(keys) {
  keys = keys.filter(Boolean).filter(function(key, index, all){ return all.indexOf(key) === index; });
  if (!keys.length || candidateOperationPending) return;
  if (keys.length > 16) { notify('候选测速一次最多 16 个不同 key', 'error', 7000); return; }
  candidateOperationPending = true;
  keys.forEach(function(key){ candidateSpeedResults[key] = {pending:true}; });
  applyCandidateView();
  fetchJSON('/api/candidates/speedtest', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({keys:keys})})
    .then(function(payload){
      normalizeCandidateResults(payload, keys);
      var promoted = keys.filter(function(key){ return candidateSpeedResults[key] && !candidateSpeedResults[key].error; }).length;
      notify('候选测速完成；' + promoted + ' 个成功节点已加入转发池', 'success', 7000);
      candidateSnapshotID = '';
      candidateQueryGeneration++;
      return requestCandidates(true);
    })
    .catch(function(err){ keys.forEach(function(key){ candidateSpeedResults[key] = {error:String(err)}; }); notify('候选测速失败：' + String(err), 'error', 7000); })
    .finally(function(){ candidateOperationPending = false; applyCandidateView(); });
}

function deleteCandidates(keys) {
  keys = keys.filter(Boolean).filter(function(key, index, all){ return all.indexOf(key) === index; });
  if (!keys.length || candidateOperationPending) return;
  if (!confirm('永久删除 ' + keys.length + ' 个候选？此操作不可撤销。')) return;
  candidateOperationPending = true;
  updateCandidateSelectionUI();
  fetchJSON('/api/candidates/delete', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({keys:keys})})
    .then(function(payload) {
      var removed = payload && Array.isArray(payload.removed) ? payload.removed.map(String) : [];
      var notFound = payload && Array.isArray(payload.not_found) ? payload.not_found.map(String) : [];
      removed.forEach(function(key){ delete selectedCandidateKeys[key]; delete candidateSpeedResults[key]; });
      var lines = keys.map(function(key) {
        if (removed.indexOf(key) >= 0) return key + '：已删除';
        if (notFound.indexOf(key) >= 0) return key + '：未找到，未从选择中清理';
        return key + '：服务端未返回结果';
      });
      showResultDialog('候选删除结果', lines.join('\n'));
      candidateSnapshotID = '';
      candidatePage = 1;
      return requestCandidates(true);
    })
    .catch(function(err){ notify('删除候选失败：' + String(err), 'error', 7000); })
    .finally(function(){ candidateOperationPending = false; updateCandidateSelectionUI(); });
}

function deleteCandidate(button) { var key = rowKey(button); if (key) deleteCandidates([key]); }

function applyCandidateView() {
  var tbody = document.getElementById('candidate-tbody');
  var pager = document.getElementById('candidate-pager');
  var topPager = document.getElementById('candidate-pager-top');
  if (!tbody) return;
  var savedFocus = captureCandidateFocus();
  function renderCandidatePagers(html) {
    if (pager) pager.innerHTML = html;
    if (topPager) topPager.innerHTML = html;
  }
  var data = candidatePageData || {};
  var rows = Array.isArray(data.candidates) ? data.candidates : [];
  var total = Math.max(0, Number(data.filtered_total || 0));
  var catalogTotal = Math.max(0, Number(data.candidate_total || 0));
  var pageSize = Math.max(1, Number(data.page_size || candidatePageSize || 50));
  var pageCount = Math.max(1, Math.ceil(total / pageSize));
  var page = Math.max(1, Number(data.page || candidatePage || 1));
  if (page > pageCount) page = pageCount;
  candidatePage = page;
  candidatePageSize = pageSize;
  var start = total ? (page - 1) * pageSize : 0;

  setText('candidate-total', formatCount(catalogTotal));
  setText('tab-link-candidates', '候选待检 (' + formatCount(catalogTotal) + ')');
  setText('candidate-matching', formatCount(total));
  setText('stat-matching', formatCount(total));
  var updated = formatCandidateUpdatedAt(data.updated_at);
  var phaseLabels = {checking:'检查中', complete:'已完成', partial:'部分来源失败（已保留旧目录）', loading:'生成中', restored:'已恢复目录，等待按当前标准复检'};
  var phase = data.phase ? (' · 快照' + (phaseLabels[data.phase] || data.phase)) : '';
  setText('candidate-count', (total ? ('显示 ' + formatCount(start + 1) + '-' + formatCount(start + rows.length) + ' · 匹配 ' + formatCount(total)) : '匹配 0') + ' · 待检测 ' + formatCount(catalogTotal) + phase + (updated ? ' · 更新于 ' + updated : ''));

  if (!catalogTotal) {
    var emptyMessage = data.phase === 'loading' || data.phase === 'checking'
      ? '候选快照正在生成，完成后会自动显示。'
      : '候选快照尚未生成，请确认已启用来源后刷新。';
    tbody.innerHTML = '<tr><td colspan="7" class="empty">' + emptyMessage + '</td></tr>';
    renderCandidatePagers('');
    restoreCandidateFocus(savedFocus);
    return;
  }
  if (!total) {
    tbody.innerHTML = '<tr><td colspan="7" class="empty">没有符合当前筛选条件的候选</td></tr>';
    renderCandidatePagers('');
    restoreCandidateFocus(savedFocus);
    return;
  }

  tbody.innerHTML = rows.map(function(candidate) {
    var country = normalizedCountry(candidate.country);
    var location = country ? countryLabel(country) : '🏳️ 国家未知';
    if (candidate.city) location += ' · ' + String(candidate.city);
    var sources = Array.isArray(candidate.source_names) && candidate.source_names.length ? candidate.source_names.join(', ') : (candidate.source || '');
    var candidateKey = String(candidate.key || '');
    var candidateExpanded = !!expandedCandidateRows[candidateKey];
    var selected = !!selectedCandidateKeys[candidateKey];
    return '<tr class="' + (candidateExpanded ? 'mobile-expanded' : '') + '" data-key="' + escapeHtml(candidateKey) + '">' +
      '<td data-label="选择"><input type="checkbox" data-action="candidate-select" aria-label="选择候选 ' + escapeHtml(candidateKey) + '" ' + (selected ? 'checked' : '') + '></td>' +
      '<td data-label="协议">' + protoBadge(candidate.protocol || '') + (candidate.has_auth ? '<span class="auth-badge" title="该上游候选使用下列用户名和密码">需认证</span>' : '') + '</td>' +
      '<td data-label="候选地址" class="mono">' + escapeHtml(candidate.proxy_url || candidate.addr || '') + '<button type="button" class="copy-btn" data-action="copy" data-copy-address="' + escapeHtml(String(candidate.proxy_url || candidate.addr || '')) + '" aria-label="复制候选代理URL">复制</button><button type="button" class="mobile-detail-toggle" data-action="details" aria-expanded="' + (candidateExpanded ? 'true' : 'false') + '">' + (candidateExpanded ? '收起' : '详情') + '</button></td>' +
      '<td data-label="来源标注地区" class="loc-cell" title="' + escapeHtml(location) + '">' + escapeHtml(location) + '</td>' +
      '<td data-label="来源" class="small mobile-secondary">' + escapeHtml(sources) + '</td>' +
      '<td data-label="测速结果" class="candidate-speed-cell mobile-secondary">' + candidateResultText(candidateKey) + '</td>' +
      '<td data-label="操作" class="candidate-action-cell"><div class="candidate-row-actions"><button type="button" class="btn-sm" data-action="candidate-speedtest" aria-label="测速候选 ' + escapeHtml(candidateKey) + '">测速</button><button type="button" class="btn-sm danger" data-action="candidate-delete" aria-label="删除候选 ' + escapeHtml(candidateKey) + '">删除</button></div></td></tr>';
  }).join('');

  if (total <= pageSize) {
    renderCandidatePagers('');
  } else {
    renderCandidatePagers(
      '<button type="button" class="btn-sm" data-action="goto-candidate-page" data-page="' + (page - 1) + '" ' + (page <= 1 ? 'disabled' : '') + '>上一页</button>' +
      '<span class="small">第 ' + page + ' / ' + pageCount + ' 页</span>' +
      '<button type="button" class="btn-sm" data-action="goto-candidate-page" data-page="' + (page + 1) + '" ' + (page >= pageCount ? 'disabled' : '') + '>下一页</button>');
  }
  updateCandidateSelectionUI();
  restoreCandidateFocus(savedFocus);
}

function selectedFailedList() { return Object.keys(selectedFailedKeys); }

function updateFailedSelectionUI() {
  var keys = selectedFailedList();
  setText('failed-selected-count', '已选 ' + keys.length);
  var retryButton = document.getElementById('failed-retry-button');
  if (retryButton) retryButton.disabled = candidateCheckActive || !keys.length;
  var pageToggle = document.getElementById('failed-select-page');
  var rows = failedPageData && Array.isArray(failedPageData.failed_candidates) ? failedPageData.failed_candidates : [];
  if (pageToggle) {
    var selectedOnPage = rows.filter(function(item){ return !!selectedFailedKeys[String(item.key || '')]; }).length;
    pageToggle.checked = !!rows.length && selectedOnPage === rows.length;
    pageToggle.indeterminate = selectedOnPage > 0 && selectedOnPage < rows.length;
  }
}

function toggleFailedSelection(button) {
  var key = rowKey(button);
  if (!key) return;
  if (button.checked) selectedFailedKeys[key] = true;
  else delete selectedFailedKeys[key];
  updateFailedSelectionUI();
}

function toggleFailedPageSelection(button) {
  var rows = failedPageData && Array.isArray(failedPageData.failed_candidates) ? failedPageData.failed_candidates : [];
  rows.forEach(function(item) {
    var key = String(item.key || '');
    if (!key) return;
    if (button.checked) selectedFailedKeys[key] = true;
    else delete selectedFailedKeys[key];
  });
  applyFailedView();
}

function onFailedFilterChange() {
  failedPage = 1;
  failedQueryGeneration++;
  if (failedFilterTimer) clearTimeout(failedFilterTimer);
  setText('failed-count', '正在应用筛选…');
  failedFilterTimer = setTimeout(function() {
    failedFilterTimer = null;
    requestFailedCandidates(true);
  }, 250);
}

function onFailedPageSizeChange() {
  failedPageSize = parseInt(document.getElementById('fc-pagesize').value, 10) || defaultCandidatePageSize();
  failedPageSize = Math.max(1, Math.min(100, failedPageSize));
  failedPageSizeTouched = true;
  failedPage = 1;
  failedQueryGeneration++;
  requestFailedCandidates(true);
}

function gotoFailedPage(page) {
  failedPage = Math.max(1, Number(page) || 1);
  failedQueryGeneration++;
  requestFailedCandidates(true);
}

function failedFailureTypeBadge(kind) {
  var key = String(kind || 'unreachable');
  var label = key === 'policy_filtered' ? '策略排除' : '连通失败';
  var cls = key === 'policy_filtered' ? 'policy' : 'failed';
  return '<span class="candidate-state candidate-state-' + cls + '">' + escapeHtml(label) + '</span>';
}

function onFailedPageFetched(pageData) {
  var previousSnapshotID = failedSnapshotID;
  failedPageData = pageData && typeof pageData === 'object' ? pageData : {};
  if (!Array.isArray(failedPageData.failed_candidates)) failedPageData.failed_candidates = [];
  ['sources','protocols','failure_types'].forEach(function(name) {
    if (!Array.isArray(failedPageData[name])) failedPageData[name] = [];
  });
  failedPage = Number(failedPageData.page) > 0 ? Number(failedPageData.page) : 1;
  failedSnapshotID = String(failedPageData.snapshot_id || '');
  if (previousSnapshotID && failedSnapshotID && previousSnapshotID !== failedSnapshotID) {
    selectedFailedKeys = Object.create(null);
  }
  var returnedPageSize = Number(failedPageData.page_size) > 0 ? Number(failedPageData.page_size) : failedPageSize;
  var responsivePageSize = defaultCandidatePageSize();
  if (!failedPageSizeTouched && returnedPageSize !== responsivePageSize) {
    failedPage = 1;
    failedPageSize = responsivePageSize;
    failedSnapshotID = '';
    failedQueryGeneration++;
    queuedFailedRefresh = true;
    syncFailedPageSizeSelect();
    setListNotice('failed-notice', 'loading', '正在按当前屏幕尺寸调整每页数量…');
    return;
  }
  failedPageSize = returnedPageSize;
  syncFailedPageSizeSelect();
  setListNotice('failed-notice', '', '');
  failedLoaded = true;
  populateCandidateFacetSelect('fc-source', failedFacetList('sources'), '全部来源');
  var protocols = failedFacetList('protocols').slice();
  ['socks5','http','https'].forEach(function(value) {
    if (!protocols.some(function(item){ return String(item.value || '').toLowerCase() === value; })) protocols.push({value:value,total:0});
  });
  populateCandidateFacetSelect('fc-proto', protocols, '全部协议');
  populateCandidateFacetSelect('fc-failure-type', failedFacetList('failure_types'), '全部失败类型');
  applyFailedView();
}

function applyFailedView() {
  var tbody = document.getElementById('failed-tbody');
  var pager = document.getElementById('failed-pager');
  var topPager = document.getElementById('failed-pager-top');
  if (!tbody) return;
  function renderFailedPagers(html) {
    if (pager) pager.innerHTML = html;
    if (topPager) topPager.innerHTML = html;
  }
  var data = failedPageData || {};
  var rows = Array.isArray(data.failed_candidates) ? data.failed_candidates : [];
  var total = Math.max(0, Number(data.filtered_total || 0));
  var failedTotal = Math.max(0, Number(data.failed_total || 0));
  var pageSize = Math.max(1, Number(data.page_size || failedPageSize || 50));
  var pageCount = Math.max(1, Math.ceil(total / pageSize));
  var page = Math.max(1, Number(data.page || failedPage || 1));
  if (page > pageCount) page = pageCount;
  failedPage = page;
  failedPageSize = pageSize;
  var start = total ? (page - 1) * pageSize : 0;

  setText('failed-total', formatCount(failedTotal));
  setText('tab-link-failed-candidates', '失败节点 (' + formatCount(failedTotal) + ')');
  setText('failed-matching', formatCount(total));
  setText('failed-count', (total ? ('显示 ' + formatCount(start + 1) + '-' + formatCount(start + rows.length) + ' · 匹配 ' + formatCount(total)) : '匹配 0') + ' · 失败节点 ' + formatCount(failedTotal));

  if (!failedTotal) {
    tbody.innerHTML = '<tr><td colspan="6" class="empty">当前没有失败节点；正式检查失败的候选会出现在这里。</td></tr>';
    renderFailedPagers('');
    updateFailedSelectionUI();
    return;
  }
  if (!total) {
    tbody.innerHTML = '<tr><td colspan="6" class="empty">没有符合当前筛选条件的失败节点</td></tr>';
    renderFailedPagers('');
    updateFailedSelectionUI();
    return;
  }

  tbody.innerHTML = rows.map(function(candidate) {
    var sources = Array.isArray(candidate.source_names) && candidate.source_names.length ? candidate.source_names.join(', ') : (candidate.source || '');
    var candidateKey = String(candidate.key || '');
    var selected = !!selectedFailedKeys[candidateKey];
    var lastError = String(candidate.last_error || '（无错误详情）');
    return '<tr data-key="' + escapeHtml(candidateKey) + '">' +
      '<td data-label="选择"><input type="checkbox" data-action="failed-select" aria-label="选择失败节点 ' + escapeHtml(candidateKey) + '" ' + (selected ? 'checked' : '') + '></td>' +
      '<td data-label="失败类型">' + failedFailureTypeBadge(candidate.failure_type) + '</td>' +
      '<td data-label="协议">' + protoBadge(candidate.protocol || '') + '</td>' +
      '<td data-label="候选地址" class="mono">' + escapeHtml(candidate.proxy_url || candidate.addr || '') + '<button type="button" class="copy-btn" data-action="copy" data-copy-address="' + escapeHtml(String(candidate.proxy_url || candidate.addr || '')) + '" aria-label="复制候选代理URL">复制</button></td>' +
      '<td data-label="来源" class="small mobile-secondary">' + escapeHtml(sources) + '</td>' +
      '<td data-label="错误摘要" class="small mobile-secondary" title="' + escapeHtml(lastError) + '">' + escapeHtml(lastError) + '</td></tr>';
  }).join('');

  if (total <= pageSize) {
    renderFailedPagers('');
  } else {
    renderFailedPagers(
      '<button type="button" class="btn-sm" data-action="goto-failed-page" data-page="' + (page - 1) + '" ' + (page <= 1 ? 'disabled' : '') + '>上一页</button>' +
      '<span class="small">第 ' + page + ' / ' + pageCount + ' 页</span>' +
      '<button type="button" class="btn-sm" data-action="goto-failed-page" data-page="' + (page + 1) + '" ' + (page >= pageCount ? 'disabled' : '') + '>下一页</button>');
  }
  updateFailedSelectionUI();
}

function onProxyIPFilterChange() {
  proxyipPage = 1;
  proxyipQueryGeneration++;
  updateProxyIPCountryButton();
  if (proxyipFilterTimer) clearTimeout(proxyipFilterTimer);
  setText('proxyip-count', '正在应用筛选…');
  proxyipFilterTimer = setTimeout(function() {
    proxyipFilterTimer = null;
    requestProxyIPs(true);
  }, 250);
}

function onProxyIPPageSizeChange() {
  proxyipPageSize = parseInt(document.getElementById('px-pagesize').value, 10) || defaultCandidatePageSize();
  proxyipPageSize = Math.max(1, Math.min(100, proxyipPageSize));
  proxyipPageSizeTouched = true;
  proxyipPage = 1;
  proxyipQueryGeneration++;
  requestProxyIPs(true);
}

function gotoProxyIPPage(page) {
  proxyipPage = Math.max(1, Number(page) || 1);
  proxyipQueryGeneration++;
  requestProxyIPs(true);
}

function onProxyIPPageFetched(pageData) {
  proxyipPageData = pageData && typeof pageData === 'object' ? pageData : {};
  if (!Array.isArray(proxyipPageData.proxyips)) proxyipPageData.proxyips = [];
  ['sources','countries'].forEach(function(name) {
    if (!Array.isArray(proxyipPageData[name])) proxyipPageData[name] = [];
  });
  proxyipPage = Number(proxyipPageData.page) > 0 ? Number(proxyipPageData.page) : 1;
  proxyipSnapshotID = String(proxyipPageData.snapshot_id || '');
  var returnedPageSize = Number(proxyipPageData.page_size) > 0 ? Number(proxyipPageData.page_size) : proxyipPageSize;
  var responsivePageSize = defaultCandidatePageSize();
  if (!proxyipPageSizeTouched && returnedPageSize !== responsivePageSize) {
    proxyipPage = 1;
    proxyipPageSize = responsivePageSize;
    proxyipSnapshotID = '';
    proxyipQueryGeneration++;
    queuedProxyipRefresh = true;
    syncProxyIPPageSizeSelect();
    setListNotice('proxyip-notice', 'loading', '正在按当前屏幕尺寸调整每页数量…');
    return;
  }
  proxyipPageSize = returnedPageSize;
  syncProxyIPPageSizeSelect();
  setListNotice('proxyip-notice', '', '');
  proxyipLoaded = true;
  populateCandidateFacetSelect('px-source', proxyipFacetList('sources'), '全部来源');
  updateProxyIPCountryButton();
  applyProxyIPView();
  var countryModal = document.getElementById('candidate-country-modal');
  if (countryPickerScope === 'proxyip' && countryModal && !countryModal.hidden) renderCandidateCountryPicker();
}

function applyProxyIPView() {
  var tbody = document.getElementById('proxyip-tbody');
  var pager = document.getElementById('proxyip-pager');
  var topPager = document.getElementById('proxyip-pager-top');
  if (!tbody) return;
  function renderProxyIPPagers(html) {
    if (pager) pager.innerHTML = html;
    if (topPager) topPager.innerHTML = html;
  }
  var data = proxyipPageData || {};
  var rows = Array.isArray(data.proxyips) ? data.proxyips : [];
  var total = Math.max(0, Number(data.filtered_total || 0));
  var proxyIPTotal = Math.max(0, Number(data.proxyip_total || 0));
  var pageSize = Math.max(1, Number(data.page_size || proxyipPageSize || 50));
  var pageCount = Math.max(1, Math.ceil(total / pageSize));
  var page = Math.max(1, Number(data.page || proxyipPage || 1));
  if (page > pageCount) page = pageCount;
  proxyipPage = page;
  proxyipPageSize = pageSize;
  var start = total ? (page - 1) * pageSize : 0;

  setText('proxyip-total', formatCount(proxyIPTotal));
  setText('tab-link-proxyip', 'ProxyIP (' + formatCount(proxyIPTotal) + ')');
  setText('proxyip-matching', formatCount(total));
  setText('proxyip-count', (total ? ('显示 ' + formatCount(start + 1) + '-' + formatCount(start + rows.length) + ' · 匹配 ' + formatCount(total)) : '匹配 0') + ' · ProxyIP ' + formatCount(proxyIPTotal));

  if (!proxyIPTotal) {
    tbody.innerHTML = '<tr><td colspan="4" class="empty">当前没有 ProxyIP 资源；添加 ProxyIP 来源并刷新后会出现在这里。</td></tr>';
    renderProxyIPPagers('');
    return;
  }
  if (!total) {
    tbody.innerHTML = '<tr><td colspan="4" class="empty">没有符合当前筛选条件的 ProxyIP 资源</td></tr>';
    renderProxyIPPagers('');
    return;
  }

  tbody.innerHTML = rows.map(function(candidate) {
    var country = normalizedCountry(candidate.country);
    var location = country ? countryLabel(country) : '🏳️ 国家未知';
    if (candidate.city) location += ' · ' + String(candidate.city);
    var sources = Array.isArray(candidate.source_names) && candidate.source_names.length ? candidate.source_names.join(', ') : (candidate.source || '');
    var candidateKey = String(candidate.key || '');
    var address = String(candidate.proxy_url || candidate.addr || '');
    return '<tr data-key="' + escapeHtml(candidateKey) + '">' +
      '<td data-label="地址" class="mono">' + escapeHtml(address) + '<button type="button" class="copy-btn" data-action="copy" data-copy-address="' + escapeHtml(address) + '" aria-label="复制 ProxyIP 地址">复制</button></td>' +
      '<td data-label="来源标注地区" class="loc-cell" title="' + escapeHtml(location) + '">' + escapeHtml(location) + '</td>' +
      '<td data-label="来源" class="small mobile-secondary">' + escapeHtml(sources) + '</td>' +
      '<td data-label="专用验证" class="candidate-verify-cell">' + proxyIPVerifyCellHTML(candidateKey, 'proxyip') + '</td></tr>';
  }).join('');

  if (total <= pageSize) {
    renderProxyIPPagers('');
  } else {
    renderProxyIPPagers(
      '<button type="button" class="btn-sm" data-action="goto-proxyip-page" data-page="' + (page - 1) + '" ' + (page <= 1 ? 'disabled' : '') + '>上一页</button>' +
      '<span class="small">第 ' + page + ' / ' + pageCount + ' 页</span>' +
      '<button type="button" class="btn-sm" data-action="goto-proxyip-page" data-page="' + (page + 1) + '" ' + (page >= pageCount ? 'disabled' : '') + '>下一页</button>');
  }
}

function candidateBatchLimitDefault() {
  var opt = document.getElementById('opt-maxcandidates');
  var value = opt ? parseInt(opt.value, 10) : 0;
  if (!isFinite(value) || value <= 0) value = opt ? parseInt(opt.placeholder, 10) : 0;
  return isFinite(value) && value > 0 ? value : 100;
}

function setCandidateCheckButtonsDisabled(disabled) {
  var batchButton = document.getElementById('candidate-batch-check-button');
  if (batchButton) batchButton.disabled = disabled;
  var retryButton = document.getElementById('failed-retry-button');
  if (retryButton) retryButton.disabled = disabled || !selectedFailedList().length;
  var retryAllButton = document.getElementById('failed-retry-all-button');
  if (retryAllButton) retryAllButton.disabled = disabled;
}

function renderCandidateCheckOperation(statusElId, operation) {
  var el = document.getElementById(statusElId);
  if (!el) return;
  var total = Math.max(0, Number(operation.total) || 0);
  var completed = Math.max(0, Number(operation.completed) || 0);
  var pct = total > 0 ? Math.round(completed / total * 100) : 0;
  var kindLabel = operation.kind === 'failed_retry' ? '失败重查' : '候选批检';
  if (operation.status === 'queued') kindLabel += '（排队中）';
  var progressText = operation.status === 'queued'
    ? '等待当前任务结束…'
    : (total > 0 ? formatCount(completed) + ' / ' + formatCount(total) + ' (' + pct + '%)' : '正在准备…');
  var alive = Number(operation.alive || 0);
  var failed = Number(operation.failed || 0);
  var policy = Number(operation.policy_filtered || 0);
  var countHtml =
    '<span class="tc-alive">通过 ' + formatCount(alive) + '</span>' +
    ' · <span class="tc-failed">失败 ' + formatCount(failed) + '</span>' +
    (policy ? ' · <span class="tc-policy">策略排除 ' + formatCount(policy) + '</span>' : '');
  var elapsed = '';
  if (operation.started_at) {
    var ms = Date.now() - new Date(operation.started_at).getTime();
    if (ms > 0) {
      var secs = Math.floor(ms / 1000);
      elapsed = secs >= 60 ? Math.floor(secs / 60) + 'm' + (secs % 60) + 's' : secs + 's';
    }
  }
  var bar = '<div class="task-panel-bar"><span class="task-panel-fill" style="width:' + pct + '%"></span></div>';
  var canCancel = operation.status === 'queued' || operation.status === 'running';
  var cancelBtn = canCancel
    ? '<button type="button" class="btn-cancel" data-action="cancel-candidate-check">取消</button>'
    : '';
  el.innerHTML =
    '<div class="task-panel-header">' +
      '<span class="task-panel-title">' + escapeHtml(kindLabel) + '</span>' +
      cancelBtn +
    '</div>' +
    bar +
    '<div class="task-panel-footer">' +
      '<span class="task-panel-progress">' + escapeHtml(progressText) + '</span>' +
      '<span class="task-panel-counts">' + countHtml + '</span>' +
      (elapsed ? '<span class="task-panel-elapsed">已用 ' + escapeHtml(elapsed) + '</span>' : '') +
    '</div>';
}

function refreshAfterCandidateCheck() {
  requestStatus();
  requestCandidates(true);
  requestFailedCandidates(true);
  requestProxyIPs(true);
}

function finishCandidateCheckOperation(statusElId, operation) {
  candidateCheckPollTimer = null;
  candidateCheckOperationID = '';
  candidateCheckActive = false;
  setCandidateCheckButtonsDisabled(false);
  updateCandidateSelectionUI();
  updateFailedSelectionUI();
  var summary = '检测完成：通过 ' + formatCount(operation.alive || 0) + ' · 失败 ' + formatCount(operation.failed || 0) + (Number(operation.policy_filtered) ? ' · 策略排除 ' + formatCount(operation.policy_filtered) : '') + '，失败项已移入失败节点页';
  var tone = 'success';
  if (operation.status === 'failed') { summary = '检测任务失败' + (operation.error ? '：' + operation.error : '。'); tone = 'error'; }
  else if (operation.status === 'cancelled') { summary = '检测任务已取消' + (operation.error ? '：' + operation.error : '。'); tone = 'error'; }
  else if (operation.status === 'superseded') { summary = '检测任务被更新的配置取代，结果未应用。'; tone = 'error'; }
  renderCandidateCheckOperation(statusElId, operation);
  notify(summary, tone, 7000);
  refreshAfterCandidateCheck();
}

function pollCandidateCheckOperation(statusURL, operationID) {
  if (candidateCheckPollTimer) clearTimeout(candidateCheckPollTimer);
  candidateCheckOperationID = String(operationID || '');
  candidateCheckActive = true;
  setCandidateCheckButtonsDisabled(true);
  candidateCheckPollTimer = setTimeout(checkCandidateCheckOperation, 1200);
  function checkCandidateCheckOperation() {
    fetchJSON(statusURL).then(function(operation) {
      if (!operation || operation.status === 'idle') {
        candidateCheckPollTimer = setTimeout(checkCandidateCheckOperation, 1200);
        return;
      }
      if (candidateCheckOperationID && String(operation.id || '') !== candidateCheckOperationID) {
        candidateCheckPollTimer = setTimeout(checkCandidateCheckOperation, 1200);
        return;
      }
      candidateCheckOperationID = String(operation.id || '');
      var statusElId = operation.kind === 'failed_retry' ? 'failed-operation-status' : 'candidate-operation-status';
      if (['complete','cancelled','superseded','failed'].indexOf(operation.status) >= 0) {
        finishCandidateCheckOperation(statusElId, operation);
        return;
      }
      renderCandidateCheckOperation(statusElId, operation);
      candidateCheckPollTimer = setTimeout(checkCandidateCheckOperation, 1200);
    }).catch(function() {
      candidateCheckPollTimer = setTimeout(checkCandidateCheckOperation, 3000);
    });
  }
}

function startCandidateBatchCheck(button) {
  if (candidateCheckActive) { notify('已有检测任务在进行，请等待完成', 'error'); return; }
  var input = document.getElementById('candidate-batch-limit');
  var raw = input ? String(input.value || '').trim() : '';
  var limit = raw ? parseInt(raw, 10) : candidateBatchLimitDefault();
  if (!isFinite(limit) || limit < 1) { notify('检测数量必须是正整数', 'error'); return; }
  if (input && !raw) input.value = String(limit);
  candidateCheckActive = true;
  setCandidateCheckButtonsDisabled(true);
  setText('candidate-operation-status', '正在提交批量正式检测…');
  fetchJSON('/api/candidates/batch-check', {
    method:'POST', headers:{'Content-Type':'application/json'},
    body:JSON.stringify({limit:limit})
  }).then(function(result) {
    pollCandidateCheckOperation(String(result && result.status_url || '/api/candidates/batch-check/status'), String(result && result.id || ''));
  }).catch(function(err) {
    if (err.status === 409 && err.code === 'candidate_check_busy') {
      setText('candidate-operation-status', '已有检测任务在进行，转入状态跟踪…');
      pollCandidateCheckOperation('/api/candidates/batch-check/status', '');
      return;
    }
    candidateCheckActive = false;
    setCandidateCheckButtonsDisabled(false);
    setText('candidate-operation-status', '批量正式检测提交失败：' + String(err));
    notify('批量正式检测失败：' + String(err), 'error', 7000);
  });
}

function retryAllFailedCandidates() {
  if (candidateCheckActive) { notify('已有检测任务在进行，请等待完成', 'error'); return; }
  candidateCheckActive = true;
  setCandidateCheckButtonsDisabled(true);
  setText('failed-operation-status', '正在提交一键重查全部失败节点…');
  fetchJSON('/api/failed-candidates/retry', {
    method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({all: true})
  }).then(function(result) {
    pollCandidateCheckOperation(String(result && result.status_url || '/api/failed-candidates/retry/status'), String(result && result.id || ''));
  }).catch(function(err) {
    if (err.status === 409 && err.code === 'candidate_check_busy') {
      setText('failed-operation-status', '已有检测任务在进行，转入状态跟踪…');
      pollCandidateCheckOperation('/api/failed-candidates/retry/status', '');
      return;
    }
    candidateCheckActive = false;
    setCandidateCheckButtonsDisabled(false);
    setText('failed-operation-status', '一键重查提交失败：' + String(err));
    notify('一键重查失败节点失败：' + String(err), 'error', 7000);
  });
}

function cancelCandidateCheck() {
  fetchJSON('/api/candidates/check/cancel', {method: 'POST'})
    .then(function(result) {
      // If the backend already finished the operation, restore UI immediately
      // without waiting for the next poll tick (up to 1200 ms).
      if (result && (result.status === 'cancelled' || result.status === 'complete')) {
        var elId = result.kind === 'failed_retry' ? 'failed-operation-status' : 'candidate-operation-status';
        finishCandidateCheckOperation(elId, result);
      }
      notify('检测任务取消请求已发送', 'success', 3000);
    })
    .catch(function(err) {
      if (err.status === 409) { notify('当前没有正在运行的检测任务', 'error', 4000); return; }
      notify('取消失败：' + String(err), 'error', 5000);
    });
}

function retryFailedCandidates() {
  var keys = selectedFailedList();
  if (!keys.length || candidateCheckActive) return;
  setText('failed-operation-status', '正在提交手动重新检测…');
  fetchJSON('/api/failed-candidates/retry', {
    method:'POST', headers:{'Content-Type':'application/json'},
    body:JSON.stringify({keys:keys})
  }).then(function(result) {
    selectedFailedKeys = Object.create(null);
    updateFailedSelectionUI();
    pollCandidateCheckOperation(String(result && result.status_url || '/api/failed-candidates/retry/status'), String(result && result.id || ''));
  }).catch(function(err) {
    if (err.status === 409 && err.code === 'candidate_check_busy') {
      setText('failed-operation-status', '已有检测任务在进行，转入状态跟踪…');
      pollCandidateCheckOperation('/api/failed-candidates/retry/status', '');
      return;
    }
    candidateCheckActive = false;
    setCandidateCheckButtonsDisabled(false);
    updateFailedSelectionUI();
    setText('failed-operation-status', '手动重新检测提交失败：' + String(err));
    notify('手动重新检测失败：' + String(err), 'error', 7000);
  });
}

function onNodePageFetched(pageData) {
  nodePageData = pageData && typeof pageData === 'object' ? pageData : {};
  if (!Array.isArray(nodePageData.nodes)) nodePageData.nodes = [];
  if (!Array.isArray(nodePageData.countries)) nodePageData.countries = [];
  nodePage = Number(nodePageData.page) > 0 ? Number(nodePageData.page) : 1;
  nodeSnapshotID = String(nodePageData.snapshot_id || '');
  var returnedPageSize = Number(nodePageData.page_size) > 0 ? Number(nodePageData.page_size) : nodePageSize;
  var responsivePageSize = defaultNodePageSize();
  if (!nodePageSizeTouched && returnedPageSize !== responsivePageSize) {
    nodePage = 1;
    nodePageSize = responsivePageSize;
    nodeSnapshotID = '';
    nodeQueryGeneration++;
    queuedNodeRefresh = true;
    syncNodePageSizeSelect();
    setListNotice('node-notice', 'loading', '正在按当前屏幕尺寸调整每页数量…');
    return;
  }
  nodePageSize = returnedPageSize;
  syncNodePageSizeSelect();
  setListNotice('node-notice', '', '');
  nodesLoaded = true;
  populateCountrySelect();
  populateRuleTargets();
  applyNodeView();
  var countryModal = document.getElementById('candidate-country-modal');
  if (countryPickerScope === 'nodes' && countryModal && !countryModal.hidden) renderCandidateCountryPicker();
}

// addCountryOptionsTo appends one "COUNTRY:XX" option per distinct country in
// the live pool to a <select>, so routing rules (and the default group) can
// target a country directly without pre-creating a group. Static group
// options rendered by the server are preserved; only the country options
// (tagged data-country) are rebuilt on each refresh.
function addCountryOptionsTo(sel) {
  if (!sel) return;
  var cur = sel.value;
  Array.prototype.slice.call(sel.querySelectorAll('option[data-country]')).forEach(function(o){ o.remove(); });
  var countries = {};
  countrySummaries().forEach(function(summary){ var c = normalizedCountry(summary.country); if (c) countries[c] = true; });
  Object.keys(countries).sort().forEach(function(c){
    var o = document.createElement('option');
    o.value = 'COUNTRY:' + c;
    o.textContent = 'COUNTRY:' + c + ' ' + countryNameZH(c) + '（该国任意节点）';
    o.setAttribute('data-country', '1');
    sel.appendChild(o);
  });
  if (cur && Array.prototype.some.call(sel.options, function(o){ return o.value === cur; })) sel.value = cur;
}

function populateRuleTargets() {
  addCountryOptionsTo(document.getElementById('rule-target-select'));
  addCountryOptionsTo(document.getElementById('default-group-select'));
}

function onFilterChange() {
  nodePage = 1;
  nodeQueryGeneration++;
  if (nodeFilterTimer) clearTimeout(nodeFilterTimer);
  setText('node-count', '正在应用筛选…');
  nodeFilterTimer = setTimeout(function(){
    nodeFilterTimer = null;
    requestNodes(true);
  }, 250);
}
function onPageSizeChange() {
  nodePageSize = parseInt(document.getElementById('f-pagesize').value, 10) || defaultNodePageSize();
  nodePageSizeTouched = true;
  nodePage = 1;
  nodeQueryGeneration++;
  requestNodes(true);
}
function gotoPage(p) {
  nodePage = Math.max(1, Number(p) || 1);
  nodeQueryGeneration++;
  requestNodes(true);
}
function setAuto() {
  postJSON('/api/nodes/auto', {}, function(err){ if (err) { notify(err, 'error'); } else { notify('已恢复自动轮换', 'success'); pollStatus(true); } });
}

function clearUnavailable() {
	var button = document.getElementById('clear-unavailable-button');
	if (button && button.disabled) {
		notify('健康标准全量复检尚未完成，暂不能永久清理', 'error');
		return;
	}
  if (!confirm('彻底删除所有标记为"不可用"的节点?这个操作不可撤销(可用节点不受影响)。')) return;
  fetchJSON('/api/nodes/clear-unavailable', {method:'POST', headers:{'Content-Type':'application/json'}, body:'{}'})
    .then(function(j){ notify('已清理 ' + (j.removed||0) + ' 个不可用节点', 'success'); pollStatus(true); })
    .catch(function(err){ notify(String(err), 'error', 7000); });
}

function setText(id, value) {
  var el = document.getElementById(id);
  if (el) el.textContent = value;
}

function updateTopCounts(total, available, unavailable) {
  if (typeof total === 'number') setText('stat-total', formatCount(total));
  if (typeof available === 'number') {
    setText('stat-available', formatCount(available));
    if (typeof unavailable === 'number') setText('stat-unavailable', formatCount(unavailable));
    else if (typeof total === 'number') setText('stat-unavailable', formatCount(Math.max(0, total - available)));
  }
}

function captureNodeFocus() {
  var el = document.activeElement;
  if (!el) return null;
  var tr = el.closest ? el.closest('#node-tbody tr') : null;
  if (tr && el.getAttribute('data-action')) {
    return {key:tr.getAttribute('data-key'), action:el.getAttribute('data-action')};
  }
  var pager = el.closest ? el.closest('#node-pager,#node-pager-top') : null;
  if (pager && el.getAttribute('data-action')) {
    return {pager:el.getAttribute('data-action'), top:pager.id === 'node-pager-top'};
  }
  return null;
}

function restoreNodeFocus(saved) {
  if (!saved) return;
  var el = null;
  if (saved.key) {
    var rows = document.querySelectorAll('#node-tbody tr[data-key]');
    for (var i = 0; i < rows.length; i++) {
      if (rows[i].getAttribute('data-key') === saved.key) {
        el = rows[i].querySelector('[data-action="' + saved.action + '"]');
        break;
      }
    }
  } else if (saved.pager) {
    var pagerID = saved.top ? '#node-pager-top' : '#node-pager';
    el = document.querySelector(pagerID + ' [data-action="' + saved.pager + '"]');
  }
  if (el && !el.disabled) el.focus();
  else {
    var fallback = document.querySelector('.node-table-scroll');
    if (fallback) fallback.focus();
  }
}

function toggleNodeDetails(button) {
  var row = button && button.closest ? button.closest('tr[data-key]') : null;
  if (!row) return;
  var key = row.getAttribute('data-key') || '';
  var expanded = !row.classList.contains('mobile-expanded');
  row.classList.toggle('mobile-expanded', expanded);
  if (expanded) expandedNodeRows[key] = true;
  else delete expandedNodeRows[key];
  button.setAttribute('aria-expanded', expanded ? 'true' : 'false');
  button.textContent = expanded ? '收起' : '详情';
}

function selectedNodeList() { return Object.keys(selectedNodeURLs); }

function updateNodeSelectionUI(rows) {
  var keys = selectedNodeList();
  setText('node-selected-count', '已选 ' + keys.length);
  var copyButton = document.querySelector('[data-action="copy-selected-nodes"]');
  if (copyButton) copyButton.disabled = !keys.length;
  var speedtestButton = document.querySelector('[data-action="speedtest-selected-nodes"]');
  if (speedtestButton) speedtestButton.disabled = !keys.length || nodeSpeedtestPending;
  var pageToggle = document.getElementById('node-select-page');
  if (pageToggle) {
    var selectedOnPage = rows.filter(function(item){ return !!selectedNodeURLs[String(item.key || '')]; }).length;
    pageToggle.checked = !!rows.length && selectedOnPage === rows.length;
    pageToggle.indeterminate = selectedOnPage > 0 && selectedOnPage < rows.length;
  }
}

function speedtestSelectedNodes() {
  var keys = selectedNodeList();
  if (!keys.length || nodeSpeedtestPending) return;
  if (keys.length > 16) { notify('节点测速一次最多 16 个不同 key', 'error', 7000); return; }
  nodeSpeedtestPending = true;
  keys.forEach(function(key){ nodeSpeedResults[key] = {pending:true}; });
  updateNodeSelectionUI(nodePageData && Array.isArray(nodePageData.nodes) ? nodePageData.nodes : []);
  notify('正在批量测速 ' + keys.length + ' 个节点…', '', 5000);
  fetchJSON('/api/nodes/speedtest/batch', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({keys:keys})})
    .then(function(payload){
      var results = payload && Array.isArray(payload.results) ? payload.results : [];
      var ok = 0, fail = 0;
      results.forEach(function(item){
        var key = String(item.key || '');
        if (!key) return;
        if (item.error || item.ok === false) { nodeSpeedResults[key] = {error: item.error || '测速失败'}; fail++; }
        else { nodeSpeedResults[key] = item; ok++; }
      });
      keys.forEach(function(key){ if (!results.some(function(r){ return String(r.key||'') === key; })) { nodeSpeedResults[key] = {error:'服务端未返回该项结果'}; fail++; } });
      notify('批量测速完成：' + ok + ' 成功' + (fail ? '，' + fail + ' 失败' : ''), ok ? 'success' : 'error', 7000);
      nodeSnapshotID = '';
      nodeQueryGeneration++;
      return requestNodes(true);
    })
    .catch(function(err){
      keys.forEach(function(key){ nodeSpeedResults[key] = {error:String(err)}; });
      notify('批量测速失败：' + String(err), 'error', 7000);
    })
    .finally(function(){
      nodeSpeedtestPending = false;
      updateNodeSelectionUI(nodePageData && Array.isArray(nodePageData.nodes) ? nodePageData.nodes : []);
    });
}

function toggleNodeSelection(button) {
  var key = rowKey(button);
  if (!key) return;
  if (button.checked) {
    var row = button.closest ? button.closest('tr[data-key]') : null;
    var node = nodePageData && Array.isArray(nodePageData.nodes) ? nodePageData.nodes.filter(function(item){ return String(item.key || '') === key; })[0] : null;
    selectedNodeURLs[key] = String((node && (node.proxy_url || node.addr)) || (row && row.getAttribute('data-copy-address')) || '');
  } else {
    delete selectedNodeURLs[key];
  }
  updateNodeSelectionUI(nodePageData && Array.isArray(nodePageData.nodes) ? nodePageData.nodes : []);
}

function toggleNodePageSelection(button) {
  var rows = nodePageData && Array.isArray(nodePageData.nodes) ? nodePageData.nodes : [];
  var targetState = !!button.checked;
  rows.forEach(function(node) {
    var key = String(node.key || '');
    if (!key) return;
    if (targetState) selectedNodeURLs[key] = String(node.proxy_url || node.addr || '');
    else delete selectedNodeURLs[key];
  });
  updateNodeSelectionUI(rows);
  applyNodeView();
}

function copySelectedNodes(button) {
  var urls = selectedNodeList().map(function(key){ return selectedNodeURLs[key]; }).filter(Boolean);
  if (!urls.length) return;
  copyAddr(urls.join('\n'), button);
}

function applyNodeView() {
  var tbody = document.getElementById('node-tbody');
  if (!tbody) return;
  var savedFocus = captureNodeFocus();
  var banner = document.querySelector('#current-node-banner .cn-addr');
  var countEl = document.getElementById('node-count');
  var data = nodePageData || {};
  var pageRows = Array.isArray(data.nodes) ? data.nodes : [];
  var active = data.active && typeof data.active === 'object' ? data.active : null;
  var pager = document.getElementById('node-pager');
  var topPager = document.getElementById('node-pager-top');
  function renderNodePagers(html) {
    if (pager) pager.innerHTML = html;
    if (topPager) topPager.innerHTML = html;
  }
  var total = Math.max(0, Number(data.filtered_total || 0));
  var poolTotal = Math.max(0, Number(data.pool_total || 0));
  var availCount = Math.max(0, Number(data.available_total || 0));
  var unavailCount = Math.max(0, Number(data.unavailable_total || 0));
  var pageSize = Math.max(1, Number(data.page_size || nodePageSize || 20));
  var pageCount = Math.max(1, Math.ceil(total / pageSize));
  var page = Math.max(1, Number(data.page || nodePage || 1));
  if (page > pageCount) page = pageCount;
  nodePage = page;
  nodePageSize = pageSize;
  var startIdx = total ? (page - 1) * pageSize : 0;
  var hideUnavail = document.getElementById('f-hide-unavail').checked;

  updateTopCounts(poolTotal, availCount, unavailCount);
  setText('tab-link-nodes', '转发池 (' + formatCount(poolTotal) + ')');
  setText('node-total', formatCount(poolTotal));
  setText('node-available', formatCount(availCount));
  setText('node-unavailable', formatCount(unavailCount));
  setText('node-matching', formatCount(total));
  setText('stat-matching', formatCount(total));

  if (countEl) {
    countEl.textContent = (total
      ? ('显示 ' + (startIdx + 1) + '-' + (startIdx + pageRows.length) + ' · 匹配 ' + total)
      : '匹配 0') + ' · 池内 ' + poolTotal + '（可用 ' + availCount + ' / 不可用 ' + unavailCount + (hideUnavail && unavailCount ? '，当前隐藏' : '') + '）';
  }

  if (!poolTotal) {
    tbody.innerHTML = '<tr><td colspan="13" class="empty">池内暂无节点，等待下次抓取周期...</td></tr>';
    updateNodeSelectionUI(pageRows);
    renderNodePagers('');
    if (banner) banner.textContent = '无 (代理池为空)';
    restoreNodeFocus(savedFocus);
    return;
  }
  if (!total) {
    tbody.innerHTML = '<tr><td colspan="13" class="empty">没有匹配的节点</td></tr>';
    updateNodeSelectionUI(pageRows);
    renderNodePagers('');
  } else {
    var html = '';
    pageRows.forEach(function(n) {
      var loc = n.country ? escapeHtml(countryLabel(n.country)) : '';
      if (n.city) loc += ' · ' + escapeHtml(n.city);
      var lat = n.latency_ms ? n.latency_ms + 'ms' : '-';
      var spd = speedCell(n);
      var nodeIP = addressHost(n.addr);
      var exit = n.exit_ip || '';
      var exitCell = exit
        ? '<span class="mono' + (exit !== nodeIP ? ' exit-diff' : '') + '">' + escapeHtml(exit) + '</span>'
        : '<span class="small">-</span>';
      var sf = (n.successes || 0) + '/' + (n.failures || 0);
      var ops = inFlightOps[n.key] || {};
      var rowExpanded = !!expandedNodeRows[n.key];
      var switchAction = n.available === false
        ? '<button type="button" class="btn-sm" data-action="switch" disabled aria-label="节点 ' + escapeHtml(n.addr) + ' 当前不可用，不能切换" title="当前不可用；可先点击验证，恢复后再切换">不可用</button>'
        : '<button type="button" class="btn-sm" data-action="switch-node" aria-label="使用节点 ' + escapeHtml(n.addr) + '">使用</button>';
      var actionsCell =
        '<div class="row-actions">' + switchAction +
        (ops.speedtest
          ? '<button type="button" class="btn-sm" data-action="speedtest" aria-disabled="true">测速中...</button>'
          : '<button type="button" class="btn-sm" data-action="speedtest" aria-label="测速节点 ' + escapeHtml(n.addr) + '">测速</button>') +
        (ops.verify
          ? '<button type="button" class="btn-sm" data-action="verify" aria-disabled="true">验证中...</button>'
          : '<button type="button" class="btn-sm" data-action="verify" title="立即重新拨号,查看真实出口IP/国家是否和标签一致" aria-label="验证节点 ' + escapeHtml(n.addr) + '">验证</button>') +
        '<button type="button" class="btn-sm" data-action="node-stats" data-key="' + escapeHtml(n.key) + '" title="查看累计转发成功/失败、连续健康失败和恢复条件" aria-label="查看节点 ' + escapeHtml(n.addr) + ' 统计">统计</button>' +
        '<button type="button" class="btn-sm danger" data-action="delete-node" aria-label="删除节点 ' + escapeHtml(n.addr) + '">删除</button>' +
        '<button type="button" class="mobile-detail-toggle" data-action="details" aria-expanded="' + (rowExpanded ? 'true' : 'false') + '">' + (rowExpanded ? '收起' : '详情') + '</button></div>';
      var selected = !!selectedNodeURLs[String(n.key || '')];
      html += '<tr class="' + (n.active ? 'active ' : '') + (n.available === false ? 'unavail ' : '') + (rowExpanded ? 'mobile-expanded' : '') + '" data-key="' + escapeHtml(n.key) + '">' +
        '<td data-label="选择"><input type="checkbox" data-action="node-select" aria-label="选择节点 ' + escapeHtml(n.addr) + '" ' + (selected ? 'checked' : '') + '></td>' +
		'<td data-label="状态">' + (n.active ? '<span class="badge-inuse">使用中</span>' : (n.source_retired ? '<span class="badge-unavail">来源已停用</span>' : (n.health_invalidated ? '<span class="badge-unavail">检测失败，需人工验证恢复</span>' : (n.policy_excluded ? '<span class="badge-unavail">出口策略排除</span>' : (n.available === false ? '<span class="badge-unavail">暂不可用</span>' : '<span class="small">可用</span>'))))) + '</td>' +
        '<td data-label="协议">' + protoBadge(n.protocol) + '</td>' +
        '<td data-label="代理URL" class="mono">' + escapeHtml(n.proxy_url || n.addr) + '<button type="button" class="copy-btn" data-action="copy" data-copy-address="' + escapeHtml(n.proxy_url || n.addr) + '" aria-label="复制完整代理URL">复制</button></td>' +
        '<td data-label="出口IP" class="mobile-secondary">' + exitCell + '</td>' +
        '<td data-label="匿名" class="mobile-secondary">' + anonBadge(n.anonymity) + '</td>' +
        '<td data-label="国家/城市" class="loc-cell" title="' + escapeHtml(loc) + '">' + (loc || '<span class="small">-</span>') + '</td>' +
        '<td data-label="评分">' + scoreCell(n.score) + '</td>' +
		'<td data-label="健康" class="small mobile-secondary" title="累计成功/失败仅统计真实转发请求结果，不含健康检查和测速">' + sf + (Number(n.consecutive_failures || 0) > 0 ? '<br>连续失败 ' + Math.max(0, Number(n.consecutive_failures || 0)) + (n.health_invalidated ? '（终态）' : '') : '') + '</td>' +
        '<td data-label="延迟">' + lat + '</td>' +
        '<td data-label="速度" class="speed-cell mobile-secondary">' + spd + '</td>' +
        '<td data-label="来源" class="small mobile-secondary">' + escapeHtml(n.source || '') + '</td>' +
        '<td data-label="操作">' + actionsCell + '</td></tr>';
    });
    tbody.innerHTML = html;
    if (total <= pageSize) {
      renderNodePagers('');
    } else {
      renderNodePagers(
          '<button type="button" class="btn-sm" data-action="goto-node-page" data-page="' + (page - 1) + '" ' + (page <= 1 ? 'disabled' : '') + '>上一页</button>' +
          '<span class="small">第 ' + page + ' / ' + pageCount + ' 页</span>' +
          '<button type="button" class="btn-sm" data-action="goto-node-page" data-page="' + (page + 1) + '" ' + (page >= pageCount ? 'disabled' : '') + '>下一页</button>');
    }
  }
  updateNodeSelectionUI(pageRows);

  if (banner) {
    var lockUI = anyPinned
      ? '<span class="lock-badge">🔒 手动锁定</span><button type="button" class="btn-sm" data-action="rotate-node">轮换下一个</button><button type="button" class="btn-sm" data-action="set-auto">恢复自动轮换</button>'
      : '<span class="auto-badge">🔄 自动轮换中</span><button type="button" class="btn-sm" data-action="rotate-node">轮换下一个</button>';
    var body = active
      ? escapeHtml(active.addr) + '<span class="cn-meta">' + protoBadge(active.protocol) + ' 出口 ' + escapeHtml(active.exit_ip || '?') + ' ' + escapeHtml(active.country || '') + '</span>'
      : '无可用节点';
    banner.innerHTML = body + lockUI;
  }
  restoreNodeFocus(savedFocus);
}

function copyAddrFrom(el) {
  copyAddr(el ? el.getAttribute('data-copy-address') : '', el);
}

function copyAddr(addr, el) {
  function flash(text) {
    if (!el) return;
    var orig = el.textContent;
    el.textContent = text;
    setTimeout(function(){ el.textContent = orig; }, 1000);
  }
  // navigator.clipboard only exists in a secure context (https:// or
  // localhost) - this dashboard is plain http://, so any access from a LAN
  // address (the normal way to reach it) has no clipboard API at all.
  // Falling through to just claiming success would be a lie the user can't
  // detect, so fall back to the classic hidden-textarea + execCommand
  // trick, which still works over plain http.
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(addr).then(function(){ flash('已复制'); }).catch(function(){ flash('复制失败'); });
    return;
  }
  try {
    var ta = document.createElement('textarea');
    ta.value = addr;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.focus();
    ta.select();
    var ok = document.execCommand('copy');
    document.body.removeChild(ta);
    flash(ok ? '已复制' : '复制失败');
  } catch (e) {
    flash('复制失败');
  }
}

function exportNodes(fmt) {
  var q = 'format=' + fmt;
  var c = document.getElementById('f-country').value; if (c) q += '&country=' + encodeURIComponent(c);
  var p = document.getElementById('f-proto').value; if (p) q += '&protocol=' + encodeURIComponent(p);
  if (document.getElementById('f-ipchanged').checked) q += '&only_changed=1';
  if (document.getElementById('f-hide-unavail').checked) q += '&available=1';
  var text = (document.getElementById('f-text').value || '').trim(); if (text) q += '&search=' + encodeURIComponent(text);
  var anon = (document.getElementById('f-anonymity') || {}).value || ''; if (anon) q += '&anonymity=' + encodeURIComponent(anon);
  var a = document.createElement('a');
  a.href = '/api/nodes/export?' + q;
  document.body.appendChild(a); a.click(); a.remove();
}

function rowKey(btn) { var tr = btn.closest('tr'); return tr ? tr.getAttribute('data-key') : ''; }

function switchNode(btn) {
  postJSON('/api/nodes/switch', {key: rowKey(btn)}, function(err) {
    if (err) { notify(err, 'error', 7000); } else { notify('已切换并锁定当前节点', 'success'); pollStatus(true); }
  });
}
function deleteNode(btn) {
  var key = rowKey(btn);
  if (!key || !confirm('永久删除节点 ' + key + '？此操作不可撤销。')) return;
  fetchJSON('/api/nodes/delete', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({keys:[key]})})
    .then(function(result){ var removed = result && Array.isArray(result.removed) ? result.removed : []; if (!removed.length) throw new Error('服务端未确认删除该节点'); notify('节点已删除', 'success'); nodeSnapshotID = ''; return pollStatus(true); })
    .catch(function(err){ notify('删除节点失败：' + String(err), 'error', 7000); });
}

function refreshSource(button) {
  var id = String(button.getAttribute('data-source-id') || '');
  if (!id) return;
  button.setAttribute('aria-disabled', 'true');
  fetchJSON('/api/sources/refresh', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({id:id})})
    .then(function(result){ var merged = result && result.accepted === false; notify(merged ? '该来源已有更新任务，已合并到现有任务' : ('来源“' + (button.getAttribute('data-source-name') || id) + '”更新任务已提交'), 'success'); })
    .catch(function(err){ notify('来源更新失败：' + String(err), 'error', 7000); })
    .finally(function(){ button.removeAttribute('aria-disabled'); });
}

function saveSourceAutoRefresh(button) {
  var id = String(button.getAttribute('data-source-id') || '');
  var row = button.closest('tr');
  var enabledInput = row && row.querySelector('[data-source-auto-enabled]');
  var intervalInput = row && row.querySelector('[data-source-auto-interval]');
  if (!id || !enabledInput || !intervalInput) { notify('无法读取该来源的自动更新设置', 'error'); return; }
  var intervalSeconds = Math.max(0, parseInt(intervalInput.value, 10) || 0);
  intervalInput.value = String(intervalSeconds);
  button.setAttribute('aria-disabled', 'true');
  setText('source-auto-status', '正在保存来源设置…');
  fetchJSON('/api/sources/auto-refresh', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({id:id, enabled:!!enabledInput.checked, interval_seconds:intervalSeconds})})
    .then(function(result) {
      var source = result && result.source;
      if (source) {
        enabledInput.checked = !!source.auto_refresh_enabled;
        intervalInput.value = String(Math.max(0, Number(source.refresh_interval_seconds) || 0));
      }
      setText('source-auto-status', '设置已保存到服务端');
      notify('该来源的自动更新设置已保存', 'success');
    })
    .catch(function(err){ setText('source-auto-status', '保存失败：' + String(err)); notify('自动更新设置保存失败：' + String(err), 'error', 7000); })
    .finally(function(){ button.removeAttribute('aria-disabled'); });
}

function pageIsVisible() { return document.visibilityState !== 'hidden'; }
function canFetchNodes() { return currentTab === 'nodes' && pageIsVisible(); }

function setConnectionState(state, detail) {
  var elements = document.querySelectorAll('[data-connection-status]');
  for (var i = 0; i < elements.length; i++) {
    var el = elements[i];
    el.dataset.state = state;
    if (el.classList.contains('api-chip')) {
      if (state === 'online') el.textContent = '管理 API · 在线';
      else if (state === 'connecting') el.textContent = '管理 API · 连接中';
      else el.textContent = '管理 API · 离线';
    } else if (state === 'online') el.textContent = '管理 API 已连接';
    else if (state === 'connecting') el.textContent = '正在连接管理 API';
    else el.textContent = '管理 API 连接中断';
    el.title = detail ? String(detail) : '';
  }
}

function localizedStatusTime(rfc3339, legacy, emptyText) {
  if (rfc3339) {
    var value = new Date(rfc3339);
    if (!isNaN(value.getTime())) return value.toLocaleString('zh-CN', {hour12:false});
  }
  return legacy || emptyText;
}

function applyStatusSummary(d) {
  if (!d || typeof d !== 'object') return;
  var pageData = nodePageData || {};
  var total = typeof d.total === 'number' ? d.total : (nodesLoaded ? pageData.pool_total : null);
  var available = typeof d.available_total === 'number' ? d.available_total : (nodesLoaded ? pageData.available_total : null);
  var unavailable = typeof d.unavailable_total === 'number' ? d.unavailable_total : (nodesLoaded ? pageData.unavailable_total : null);
  updateTopCounts(total, available, unavailable);
	var clearButton = document.getElementById('clear-unavailable-button');
	var clearHelp = document.getElementById('clear-unavailable-help');
	if (clearButton) {
		clearButton.disabled = !!d.health_recheck_pending;
		clearButton.title = d.health_recheck_pending ? '等待当前健康标准的全量复检完成' : '';
	}
	if (clearHelp && d.health_recheck_pending) {
		clearHelp.textContent = '健康标准刚发生变化，旧结果正在全量复检。为避免把尚未轮到的节点永久删除，清理功能已暂时锁定。';
	} else if (clearHelp) {
		clearHelp.textContent = '不可用节点默认只隐藏并会在恢复后重新出现。仅在确认不再保留历史节点时执行永久清理。';
	}
  if (typeof d.failed_candidate_total === 'number') {
    var failedTotal = Math.max(0, Number(d.failed_candidate_total));
    setText('tab-link-failed-candidates', '失败节点 (' + formatCount(failedTotal) + ')');
    if (!failedLoaded) setText('failed-total', formatCount(failedTotal));
  }
  if (typeof d.proxyip_total === 'number') {
    var proxyIPTotal = Math.max(0, Number(d.proxyip_total));
    setText('tab-link-proxyip', 'ProxyIP (' + formatCount(proxyIPTotal) + ')');
    if (!proxyipLoaded) setText('proxyip-total', formatCount(proxyIPTotal));
    setText('stat-proxyip', proxyIPTotal);
  }
  var sourceLastAt = d.last_source_refresh_at || d.last_scrape_at;
  var sourceNextAt = d.next_source_refresh_at || d.next_scrape_at;
  var sourceLast = d.last_source_refresh || d.last_scrape;
  var sourceNext = d.next_source_refresh || d.next_scrape;
  var lastDisplay = localizedStatusTime(sourceLastAt, sourceLast, 'N/A');
  var nextDisplay = localizedStatusTime(sourceNextAt, sourceNext, 'N/A');
  setText('stat-last', lastDisplay);
  setText('stat-next', nextDisplay);
  setText('timeline-source-last', localizedStatusTime(d.last_source_refresh_at, sourceLast, '尚未刷新'));
  setText('timeline-source-next', localizedStatusTime(d.next_source_refresh_at, sourceNext, '等待调度'));
  setText('timeline-full-last', localizedStatusTime(d.last_full_recheck_at, d.last_full_recheck, '尚未全检'));
  setText('timeline-full-next', localizedStatusTime(d.next_full_recheck_at, d.next_full_recheck, '等待调度'));
  lastKnownScrape = sourceLastAt || sourceLast || '';
  lastKnownNextScrape = sourceNextAt || sourceNext || '';

  if (typeof d.candidate_total === 'number') {
    var candidateTotal = Math.max(0, Number(d.candidate_total));
    setText('tab-link-candidates', '候选待检 (' + formatCount(candidateTotal) + ')');
    if (!candidatesLoaded) setText('candidate-total', formatCount(candidateTotal));
    var candidateLink = document.getElementById('tab-link-candidates');
    if (candidateLink) {
      var candidateState = d.candidate_phase || 'loading';
      candidateLink.title = candidateState === 'partial'
        ? '候选库存已保留失败来源的上一版数据；本轮有 ' + formatCount(d.candidate_source_errors || 0) + ' 个来源失败'
        : '候选快照状态：' + candidateState;
    }
  }

  var scrapeEl = document.getElementById('scrape-flow');
  if (scrapeEl && d.scrape && typeof d.scrape === 'object') {
    setText('scrape-raw', formatCount(typeof d.scrape.raw === 'number' ? d.scrape.raw : 0));
    setText('scrape-candidates', formatCount(typeof d.scrape.candidates === 'number' ? d.scrape.candidates : 0));
    setText('scrape-checked', formatCount(typeof d.scrape.checked === 'number' ? d.scrape.checked : 0));
    setText('scrape-alive', formatCount(typeof d.scrape.fresh_alive === 'number' ? d.scrape.fresh_alive : 0));
    var sourceTotal = typeof d.scrape.source_total === 'number' ? d.scrape.source_total : 0;
    var sourceErrors = typeof d.scrape.source_errors === 'number' ? d.scrape.source_errors : 0;
    setText('scrape-meta', sourceTotal + ' 个来源' + (sourceErrors ? ' · ' + sourceErrors + ' 个来源报错' : ' · 无来源报错'));
    scrapeEl.hidden = false;
  }

  if (Array.isArray(d.groups)) {
    anyPinned = false;
    d.groups.forEach(function(g){ if (g.name === 'ANY') anyPinned = !!g.pinned; });
    renderGroups(d.groups);
  }
}

function requestStatus() {
  if (statusRequest) return statusRequest;
  statusRequest = fetchJSON('/api/status?compact=1')
    .then(function(d){ setConnectionState('online'); applyStatusSummary(d); return d; })
    .catch(function(err){ setConnectionState('offline', err); throw err; })
    .finally(function(){ statusRequest = null; });
  return statusRequest;
}

var queuedNodeRefresh = false;
function requiredControl(id) {
  var control = document.getElementById(id);
  if (!control) throw new Error('缺少必需的页面控件 #' + id);
  return control;
}

var nodeAvailabilityFilter = '';

function filterNodeAvailability(availability) {
  var newFilter = nodeAvailabilityFilter === availability ? '' : availability;
  nodeAvailabilityFilter = newFilter;
  var hideUnavail = document.getElementById('f-hide-unavail');
  if (hideUnavail) {
    hideUnavail.checked = newFilter === 'available';
  }
  updateNodeAvailabilityButtons();
  onFilterChange();
}

function updateNodeAvailabilityButtons() {
  var buttons = document.querySelectorAll('.task-metrics .task-metric[data-action="filter-node-availability"]');
  buttons.forEach(function(btn) {
    var av = btn.getAttribute('data-availability') || '';
    btn.setAttribute('aria-pressed', av === nodeAvailabilityFilter ? 'true' : 'false');
  });
}

function nodePageURL() {
  var q = [
    'page=' + encodeURIComponent(nodePage),
    'page_size=' + encodeURIComponent(nodePageSize)
  ];
  var text = (requiredControl('f-text').value || '').trim();
  var country = requiredControl('f-country').value;
  var protocol = requiredControl('f-proto').value;
  var anonymity = requiredControl('f-anonymity').value;
  var sort = requiredControl('f-sort').value;
  if (text) q.push('search=' + encodeURIComponent(text));
  if (country) q.push('country=' + encodeURIComponent(country));
  if (protocol) q.push('protocol=' + encodeURIComponent(protocol));
  if (anonymity) q.push('anonymity=' + encodeURIComponent(anonymity));
  if (sort) q.push('sort=' + encodeURIComponent(sort));
  if (requiredControl('f-ipchanged').checked) q.push('only_changed=1');
  if (nodeAvailabilityFilter === 'available') q.push('available=1');
  else if (nodeAvailabilityFilter === 'unavailable') q.push('unavailable=1');
  else if (requiredControl('f-hide-unavail').checked) q.push('available=1');
  if (nodePage > 1 && nodeSnapshotID) q.push('snapshot_id=' + encodeURIComponent(nodeSnapshotID));
  return '/api/nodes/page?' + q.join('&');
}

function requestNodes(force) {
  if (!canFetchNodes()) return Promise.resolve(null);
  if (nodesRequest) {
    queuedNodeRefresh = queuedNodeRefresh || !!force;
    return nodesRequest;
  }
  if (!force && Date.now() - lastNodesFetchAt < 30000) return Promise.resolve(null);

  nodesAbortController = typeof AbortController === 'function' ? new AbortController() : null;
  var options = nodesAbortController ? {signal:nodesAbortController.signal} : undefined;
  var requestGeneration = nodeQueryGeneration;
  if (!nodesLoaded) setListNotice('node-notice', 'loading', '正在获取代理池分页数据…');
  nodesRequest = Promise.resolve().then(function() { return fetchJSON(nodePageURL(), options); })
    .then(function(pageData) {
      if (canFetchNodes() && requestGeneration === nodeQueryGeneration) {
        lastNodesFetchAt = Date.now();
        onNodePageFetched(pageData);
      }
      return pageData;
    })
    .catch(function(err) {
      if (err && err.status === 409 && err.code === 'snapshot_changed') {
        nodeSnapshotID = '';
        nodePage = 1;
        queuedNodeRefresh = true;
        setListNotice('node-notice', 'loading', '代理池已更新，正在从新快照第一页继续浏览…');
        return null;
      }
      if (!err || err.name !== 'AbortError') {
        setText('node-count', '节点列表更新失败');
        setListNotice('node-notice', 'error', '无法更新代理池：' + String(err) + '。已保留上一次成功加载的内容。');
      }
      return null;
    })
    .finally(function() {
      var runAgain = queuedNodeRefresh;
      queuedNodeRefresh = false;
      nodesRequest = null;
      nodesAbortController = null;
      if (runAgain && canFetchNodes()) setTimeout(function(){ requestNodes(true); }, 0);
    });
  return nodesRequest;
}

function abortNodeRequest() {
  queuedNodeRefresh = false;
  if (nodeFilterTimer) {
    clearTimeout(nodeFilterTimer);
    nodeFilterTimer = null;
  }
  if (nodesAbortController) nodesAbortController.abort();
}

function canFetchCandidates() { return currentTab === 'candidates' && pageIsVisible(); }

function candidatePageURL() {
  var q = [
    'page=' + encodeURIComponent(candidatePage),
    'page_size=' + encodeURIComponent(candidatePageSize)
  ];
  var text = (requiredControl('cf-text').value || '').trim();
  var source = requiredControl('cf-source').value;
  var protocol = requiredControl('cf-proto').value;
  var country = requiredControl('cf-country').value;
  if (text) q.push('search=' + encodeURIComponent(text));
  if (source) q.push('source=' + encodeURIComponent(source));
  if (protocol) q.push('protocol=' + encodeURIComponent(protocol));
  if (country) q.push('country=' + encodeURIComponent(country));
  if (candidatePage > 1 && candidateSnapshotID) q.push('snapshot_id=' + encodeURIComponent(candidateSnapshotID));
  return '/api/candidates/page?' + q.join('&');
}

var queuedCandidateRefresh = false;
function requestCandidates(force) {
  if (!canFetchCandidates()) return Promise.resolve(null);
  if (candidatesRequest) {
    queuedCandidateRefresh = queuedCandidateRefresh || !!force;
    return candidatesRequest;
  }
  // Filtering a 400k+ snapshot is intentionally not part of every 15-second
  // status poll. The list refreshes on tab entry/filter/page changes and at
  // most once every two minutes while left open.
  var refreshInterval = 120000;
  if (candidatePageData && candidatePageData.phase === 'loading') refreshInterval = 10000;
  else if (candidatePageData && candidatePageData.phase === 'checking') refreshInterval = 30000;
  if (!force && Date.now() - lastCandidatesFetchAt < refreshInterval) return Promise.resolve(null);

  candidatesAbortController = typeof AbortController === 'function' ? new AbortController() : null;
  var options = candidatesAbortController ? {signal:candidatesAbortController.signal} : undefined;
  var requestGeneration = candidateQueryGeneration;
  if (!candidatesLoaded) setListNotice('candidate-notice', 'loading', '正在查询完整候选快照，请稍候…');
  candidatesRequest = Promise.resolve().then(function() { return fetchJSON(candidatePageURL(), options); })
    .then(function(pageData) {
      if (canFetchCandidates() && requestGeneration === candidateQueryGeneration) {
        lastCandidatesFetchAt = Date.now();
        onCandidatePageFetched(pageData);
      }
      return pageData;
    })
    .catch(function(err) {
      if (err && err.status === 409 && err.code === 'snapshot_changed') {
        candidateSnapshotID = '';
        candidatePage = 1;
        selectedCandidateKeys = Object.create(null);
        candidateSpeedResults = Object.create(null);
        updateCandidateSelectionUI();
        queuedCandidateRefresh = true;
        setListNotice('candidate-notice', 'loading', '候选目录已生成新快照，正在从第一页继续浏览…');
        return null;
      }
      if (!err || err.name !== 'AbortError') {
        setText('candidate-count', '完整候选目录更新失败');
        setListNotice('candidate-notice', 'error', '无法更新候选目录：' + String(err) + '。已保留上一次成功加载的内容。');
      }
      return null;
    })
    .finally(function() {
      var runAgain = queuedCandidateRefresh;
      queuedCandidateRefresh = false;
      candidatesRequest = null;
      candidatesAbortController = null;
      if (runAgain && canFetchCandidates()) setTimeout(function(){ requestCandidates(true); }, 0);
    });
  return candidatesRequest;
}

function abortCandidateRequest() {
  queuedCandidateRefresh = false;
  if (candidateFilterTimer) {
    clearTimeout(candidateFilterTimer);
    candidateFilterTimer = null;
  }
  if (candidatesAbortController) candidatesAbortController.abort();
}

function canFetchFailed() { return currentTab === 'failed-candidates' && pageIsVisible(); }

function failedPageURL() {
  var q = [
    'page=' + encodeURIComponent(failedPage),
    'page_size=' + encodeURIComponent(failedPageSize)
  ];
  var text = (requiredControl('fc-text').value || '').trim();
  var source = requiredControl('fc-source').value;
  var protocol = requiredControl('fc-proto').value;
  var failureType = requiredControl('fc-failure-type').value;
  if (text) q.push('search=' + encodeURIComponent(text));
  if (source) q.push('source=' + encodeURIComponent(source));
  if (protocol) q.push('protocol=' + encodeURIComponent(protocol));
  if (failureType) q.push('failure_type=' + encodeURIComponent(failureType));
  if (failedPage > 1 && failedSnapshotID) q.push('snapshot_id=' + encodeURIComponent(failedSnapshotID));
  return '/api/failed-candidates?' + q.join('&');
}

var queuedFailedRefresh = false;
function requestFailedCandidates(force) {
  if (!canFetchFailed()) return Promise.resolve(null);
  if (failedRequest) {
    queuedFailedRefresh = queuedFailedRefresh || !!force;
    return failedRequest;
  }
  if (!force && Date.now() - lastFailedFetchAt < 120000) return Promise.resolve(null);

  failedAbortController = typeof AbortController === 'function' ? new AbortController() : null;
  var options = failedAbortController ? {signal:failedAbortController.signal} : undefined;
  var requestGeneration = failedQueryGeneration;
  if (!failedLoaded) setListNotice('failed-notice', 'loading', '正在查询失败节点快照，请稍候…');
  failedRequest = Promise.resolve().then(function() { return fetchJSON(failedPageURL(), options); })
    .then(function(pageData) {
      if (canFetchFailed() && requestGeneration === failedQueryGeneration) {
        lastFailedFetchAt = Date.now();
        onFailedPageFetched(pageData);
      }
      return pageData;
    })
    .catch(function(err) {
      if (err && err.status === 409 && err.code === 'snapshot_changed') {
        failedSnapshotID = '';
        failedPage = 1;
        selectedFailedKeys = Object.create(null);
        updateFailedSelectionUI();
        queuedFailedRefresh = true;
        setListNotice('failed-notice', 'loading', '失败节点目录已生成新快照，正在从第一页继续浏览…');
        return null;
      }
      if (!err || err.name !== 'AbortError') {
        setText('failed-count', '失败节点目录更新失败');
        setListNotice('failed-notice', 'error', '无法更新失败节点目录：' + String(err) + '。已保留上一次成功加载的内容。');
      }
      return null;
    })
    .finally(function() {
      var runAgain = queuedFailedRefresh;
      queuedFailedRefresh = false;
      failedRequest = null;
      failedAbortController = null;
      if (runAgain && canFetchFailed()) setTimeout(function(){ requestFailedCandidates(true); }, 0);
    });
  return failedRequest;
}

function abortFailedRequest() {
  queuedFailedRefresh = false;
  if (failedFilterTimer) {
    clearTimeout(failedFilterTimer);
    failedFilterTimer = null;
  }
  if (failedAbortController) failedAbortController.abort();
}

function canFetchProxyIPs() { return currentTab === 'proxyip' && pageIsVisible(); }

function proxyIPPageURL() {
  var q = [
    'page=' + encodeURIComponent(proxyipPage),
    'page_size=' + encodeURIComponent(proxyipPageSize)
  ];
  var text = (requiredControl('px-text').value || '').trim();
  var source = requiredControl('px-source').value;
  var country = requiredControl('px-country').value;
  if (text) q.push('search=' + encodeURIComponent(text));
  if (source) q.push('source=' + encodeURIComponent(source));
  if (country) q.push('country=' + encodeURIComponent(country));
  if (proxyipPage > 1 && proxyipSnapshotID) q.push('snapshot_id=' + encodeURIComponent(proxyipSnapshotID));
  return '/api/proxyip/page?' + q.join('&');
}

var queuedProxyipRefresh = false;
function requestProxyIPs(force) {
  if (!canFetchProxyIPs()) return Promise.resolve(null);
  if (proxyipRequest) {
    queuedProxyipRefresh = queuedProxyipRefresh || !!force;
    return proxyipRequest;
  }
  if (!force && Date.now() - lastProxyipFetchAt < 120000) return Promise.resolve(null);

  proxyipAbortController = typeof AbortController === 'function' ? new AbortController() : null;
  var options = proxyipAbortController ? {signal:proxyipAbortController.signal} : undefined;
  var requestGeneration = proxyipQueryGeneration;
  if (!proxyipLoaded) setListNotice('proxyip-notice', 'loading', '正在查询 ProxyIP 资源快照，请稍候…');
  proxyipRequest = Promise.resolve().then(function() { return fetchJSON(proxyIPPageURL(), options); })
    .then(function(pageData) {
      if (canFetchProxyIPs() && requestGeneration === proxyipQueryGeneration) {
        lastProxyipFetchAt = Date.now();
        onProxyIPPageFetched(pageData);
      }
      return pageData;
    })
    .catch(function(err) {
      if (err && err.status === 409 && err.code === 'snapshot_changed') {
        proxyipSnapshotID = '';
        proxyipPage = 1;
        queuedProxyipRefresh = true;
        setListNotice('proxyip-notice', 'loading', 'ProxyIP 目录已生成新快照，正在从第一页继续浏览…');
        return null;
      }
      if (!err || err.name !== 'AbortError') {
        setText('proxyip-count', 'ProxyIP 目录更新失败');
        setListNotice('proxyip-notice', 'error', '无法更新 ProxyIP 目录：' + String(err) + '。已保留上一次成功加载的内容。');
      }
      return null;
    })
    .finally(function() {
      var runAgain = queuedProxyipRefresh;
      queuedProxyipRefresh = false;
      proxyipRequest = null;
      proxyipAbortController = null;
      if (runAgain && canFetchProxyIPs()) setTimeout(function(){ requestProxyIPs(true); }, 0);
    });
  return proxyipRequest;
}

function abortProxyIPRequest() {
  queuedProxyipRefresh = false;
  if (proxyipFilterTimer) {
    clearTimeout(proxyipFilterTimer);
    proxyipFilterTimer = null;
  }
  if (proxyipAbortController) proxyipAbortController.abort();
}

function requestCurrentCatalog(force) {
  if (currentTab === 'nodes') return requestNodes(!!force);
  if (currentTab === 'candidates') return requestCandidates(!!force);
  if (currentTab === 'failed-candidates') return requestFailedCandidates(!!force);
  if (currentTab === 'proxyip') return requestProxyIPs(!!force);
  return Promise.resolve(null);
}

function pollStatus(forceNodes) {
  var statusDone = pageIsVisible() ? requestStatus().catch(function(){ return null; }) : Promise.resolve(null);
  return statusDone.then(function(){ return requestCurrentCatalog(!!forceNodes); });
}

function schedulePoll(delay) {
  if (pollTimer) clearTimeout(pollTimer);
  pollTimer = setTimeout(function() {
    pollStatus(false).finally(function(){ schedulePoll(15000); });
  }, typeof delay === 'number' ? delay : 15000);
}

function doRefresh(btn) {
  btn.disabled = true;
  var orig = btn.textContent;
  btn.textContent = '刷新中...';
  var statusEl = document.getElementById('refresh-status');
  var beforeLast = lastKnownScrape || ((document.getElementById('stat-last') || {}).textContent || '');
  var beforeNext = lastKnownNextScrape || ((document.getElementById('stat-next') || {}).textContent || '');
  if (beforeLast === 'N/A') beforeLast = '';
  if (beforeNext === 'N/A') beforeNext = '';
  if (statusEl) statusEl.textContent = '刷新请求提交中…';

  fetchJSON('/api/refresh', {method:'POST', headers:{'Content-Type':'application/json'}, body:'{}'})
    .then(function(job){
      if (statusEl) statusEl.textContent = job && job.coalesced ? '已有刷新任务运行，本次请求已合并…' : '后台正在抓取并检测节点…';
      waitForRefresh(beforeLast, beforeNext, btn, orig, Date.now(), job && job.id ? String(job.id) : '');
    })
    .catch(function(err){
      btn.disabled = false;
      btn.textContent = orig;
      if (statusEl) statusEl.textContent = '刷新失败：' + String(err);
    });
}

function refreshJobFromState(state, id) {
  if (!state || !id) return null;
  var jobs = [state.active, state.pending, state.last];
  for (var i = 0; i < jobs.length; i++) {
    if (jobs[i] && String(jobs[i].id || '') === id) return jobs[i];
  }
  return null;
}

function finishRefreshPresentation(operation, btn, originalLabel) {
  btn.disabled = false;
  btn.textContent = originalLabel;
  var statusEl = document.getElementById('refresh-status');
  var status = operation && operation.status ? operation.status : 'complete';
  var detail = operation && operation.error ? String(operation.error) : '';
  var message = '刷新完成，节点状态已更新。';
  var toast = '代理池刷新完成';
  var tone = 'success';
  var clearLater = true;
  if (status === 'partial') {
    message = '刷新完成；部分来源失败，旧候选已保留。';
    toast = '刷新完成，部分来源暂时失败';
    tone = '';
  } else if (status === 'skipped') {
    message = '刷新已跳过' + (detail ? '：' + detail : '。');
    toast = message;
    tone = 'error';
    clearLater = false;
  } else if (status === 'failed') {
    message = '刷新失败' + (detail ? '：' + detail : '，请查看服务日志。');
    toast = message;
    tone = 'error';
    clearLater = false;
  }
  if (statusEl) statusEl.textContent = message;
  notify(toast, tone, tone === 'error' ? 8000 : 4500);
  if (status === 'complete' || status === 'partial') requestCurrentCatalog(true);
  if (clearLater) setTimeout(function(){ if (statusEl && statusEl.textContent === message) statusEl.textContent = ''; }, 8000);
}

function waitForRefresh(beforeLast, beforeNext, btn, orig, startedAt, jobID) {
  if (refreshPollTimer) clearTimeout(refreshPollTimer);
  refreshPollTimer = setTimeout(function checkRefreshStatus() {
    var jobRequest = jobID ? fetchJSON('/api/refresh/status').catch(function(){ return null; }) : Promise.resolve(null);
    Promise.all([requestStatus(), jobRequest]).then(function(results) {
      var d = results[0] || {};
      var operation = refreshJobFromState(results[1], jobID);
      var last = d.last_scrape_at || d.last_scrape || '';
      var next = d.next_scrape_at || d.next_scrape || '';
      var operationDone = operation && ['complete','partial','skipped','failed'].indexOf(operation.status) >= 0;
      // A tracked job must finish on its own ID. A different periodic/active
      // refresh changing timestamps must never complete this queued operation.
      var completed = jobID
        ? operationDone
        : ((!!last && last !== beforeLast) || (!!next && next !== beforeNext && !!last));
      if (completed) {
        finishRefreshPresentation(operation, btn, orig);
        refreshPollTimer = null;
        return;
      }
      if (operation && operation.status === 'queued') {
        var queuedEl = document.getElementById('refresh-status');
        if (queuedEl) queuedEl.textContent = '刷新任务已排队，等待当前任务完成…';
      } else if (operation && operation.status === 'running') {
        var runningEl = document.getElementById('refresh-status');
        if (runningEl) runningEl.textContent = '正在抓取来源并检测节点…';
      }
      if (Date.now() - startedAt >= 300000) {
        btn.disabled = false;
        btn.textContent = orig;
        var timeoutEl = document.getElementById('refresh-status');
        if (timeoutEl) timeoutEl.textContent = '刷新仍在后台运行，可稍后查看上次刷新时间。';
        refreshPollTimer = null;
        return;
      }
      refreshPollTimer = setTimeout(checkRefreshStatus, 2000);
    }).catch(function() {
      if (Date.now() - startedAt >= 300000) {
        btn.disabled = false;
        btn.textContent = orig;
        refreshPollTimer = null;
        return;
      }
      refreshPollTimer = setTimeout(checkRefreshStatus, 3000);
    });
  }, 1000);
}

function saveCheckURL(button) {
  var input = document.getElementById('check-url-input');
  var statusEl = document.getElementById('check-url-status');
  var url = (input.value || '').trim();
  if (!url) { notify('请输入一个 http:// 或 https:// 开头的网址', 'error'); return; }
  var original = button ? button.textContent : '';
  if (button) { button.disabled = true; button.textContent = '保存中…'; }
  fetchJSON('/api/settings/check-url', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({url:url})})
    .then(function(result) {
	  if (result && result.changed === false) {
		var unchangedMessage = '检测目标没有变化，无需让节点重新下线。';
		if (statusEl) statusEl.textContent = unchangedMessage;
		notify(unchangedMessage, 'success');
		return;
	  }
      var invalidated = Math.max(0, Number(result && result.invalidated_total) || 0);
      var message = '已保存；旧标准健康结果已失效，正在全量复检' + (invalidated ? ' ' + formatCount(invalidated) + ' 个节点' : '') + '。';
      if (statusEl) statusEl.textContent = message;
      notify(message, '', 7000);
      pollStatus(true);
	  if (result && result.health_recheck && result.health_recheck.id) {
		pollHealthRecheck(result.health_recheck.id);
	  }
    })
    .catch(function(err) { notify(err, 'error', 7000); })
    .finally(function() {
      if (button) { button.disabled = false; button.textContent = original; }
    });
}

function syncAutoCheckDefault() {
  var followDefault = document.getElementById('opt-auto-check-default');
  var autoCheck = document.getElementById('opt-auto-check');
  if (followDefault && autoCheck) autoCheck.disabled = followDefault.checked;
}

function syncRequireIPChangeDefault() {
  var followDefault = document.getElementById('opt-require-ip-change-default');
  var requireIPChange = document.getElementById('opt-require-ip-change');
  if (followDefault && requireIPChange) requireIPChange.disabled = followDefault.checked;
}

function loadCheckOptions() {
  fetchJSON('/api/settings/check-options').then(function(result) {
    if (!result) return;
    var overrides = result.overrides || {};
    var mc = document.getElementById('opt-maxconcurrent');
    var ct = document.getElementById('opt-checktimeout');
    var cand = document.getElementById('opt-maxcandidates');
    var sourceRefresh = document.getElementById('opt-source-refresh-interval');
    var fullRecheck = document.getElementById('opt-full-recheck-interval');
    var ric = document.getElementById('opt-require-ip-change');
    var ricDefault = document.getElementById('opt-require-ip-change-default');
    if (mc) { mc.value = Number(overrides.max_concurrent || 0) || ''; mc.placeholder = String(result.max_concurrent || ''); }
    if (ct) { ct.value = Number(overrides.check_timeout_seconds || 0) || ''; ct.placeholder = String(result.check_timeout_seconds || ''); }
    if (cand) { cand.value = Number(overrides.max_candidates || 0) || ''; cand.placeholder = String(result.max_candidates || ''); }
    var batchLimit = document.getElementById('candidate-batch-limit');
    if (batchLimit) batchLimit.placeholder = String(Number(overrides.max_candidates || 0) || result.max_candidates || '');
    if (sourceRefresh) {
      sourceRefresh.value = Number(overrides.source_refresh_interval_seconds || 0) ? String(Number(overrides.source_refresh_interval_seconds) / 60) : '';
      sourceRefresh.placeholder = String(Number(result.source_refresh_interval_seconds || 0) / 60 || '');
    }
    if (fullRecheck) {
      fullRecheck.value = Number(overrides.full_recheck_interval_seconds || 0) ? String(Number(overrides.full_recheck_interval_seconds) / 60) : '';
      fullRecheck.placeholder = String(Number(result.full_recheck_interval_seconds || 0) / 60 || '');
    }
    if (ric) ric.checked = !!result.require_ip_change;
    if (ricDefault) ricDefault.checked = overrides.require_ip_change === 'default' || overrides.require_ip_change == null;
    syncRequireIPChangeDefault();
    var autoCheck = document.getElementById('opt-auto-check');
    var autoInterval = document.getElementById('opt-auto-check-interval');
    var autoCheckDefault = document.getElementById('opt-auto-check-default');
    var isAutoCheckOverridden = overrides.auto_candidate_check != null && overrides.auto_candidate_check !== 'default';
    if (autoCheckDefault) autoCheckDefault.checked = !isAutoCheckOverridden;
    if (autoCheck) autoCheck.checked = result.auto_candidate_check !== false;
    syncAutoCheckDefault();
    if (autoInterval) {
      autoInterval.value = Number(overrides.auto_check_interval_seconds || 0) || '';
      autoInterval.placeholder = String(result.auto_check_interval_seconds || 0);
    }
  }).catch(function() {});
}

function saveCheckOptions(button) {
  var statusEl = document.getElementById('check-options-status');
  function intOf(id) {
    var raw = (document.getElementById(id).value || '').trim();
    if (!raw) return 0;
    var n = Number(raw);
    return isFinite(n) ? Math.floor(n) : -1;
  }
  function secondsFromMinutes(id) {
    var raw = (document.getElementById(id).value || '').trim();
    if (!raw) return 0;
    var minutes = Number(raw);
    return isFinite(minutes) ? Math.round(minutes * 60) : -1;
  }
  var maxConcurrent = intOf('opt-maxconcurrent');
  var checkTimeout = intOf('opt-checktimeout');
  var maxCandidates = intOf('opt-maxcandidates');
  var sourceRefreshSeconds = secondsFromMinutes('opt-source-refresh-interval');
  var fullRecheckSeconds = secondsFromMinutes('opt-full-recheck-interval');
  var autoIntervalRaw = ((document.getElementById('opt-auto-check-interval') || {}).value || '').trim();
  var autoIntervalSeconds = autoIntervalRaw ? Math.floor(Number(autoIntervalRaw)) : 0;
  if (maxConcurrent < 0 || checkTimeout < 0 || maxCandidates < 0 || sourceRefreshSeconds < 0 || fullRecheckSeconds < 0) { notify('检测选项和周期必须是数字', 'error'); return; }
  if (!isFinite(autoIntervalSeconds) || autoIntervalSeconds < 0 || autoIntervalSeconds > 3600) { notify('批间暂停必须是 0-3600 的秒数', 'error'); return; }
  var followDefault = !!(document.getElementById('opt-require-ip-change-default') || {}).checked;
  var payload = {
    max_concurrent: maxConcurrent,
    check_timeout_seconds: checkTimeout,
    max_candidates: maxCandidates,
    source_refresh_interval_seconds: sourceRefreshSeconds,
    full_recheck_interval_seconds: fullRecheckSeconds,
    require_ip_change: followDefault ? 'default' : !!(document.getElementById('opt-require-ip-change') || {}).checked,
    auto_candidate_check: (function() {
      var follow = document.getElementById('opt-auto-check-default');
      if (follow && follow.checked) return 'default';
      return !!(document.getElementById('opt-auto-check') || {}).checked;
    }()),
    auto_check_interval_seconds: autoIntervalSeconds
  };
  var original = button ? button.textContent : '';
  if (button) { button.disabled = true; button.textContent = '保存中…'; }
  fetchJSON('/api/settings/check-options', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(payload)})
    .then(function(result) {
      var recheck = result && result.health_recheck && result.health_recheck.id;
      var message = result && result.policy_changed
        ? '已保存；策略已变更（或超时发生变化），旧健康结果失效，正在全池复检。'
        : '已保存；并发/抽样从下一轮检测开始生效，来源刷新与全池复检周期将在各自下一轮调度时应用。';
      if (result && result.baseline_refresh_attempted && !result.baseline_refreshed) {
        message += ' 基线刷新失败，暂保留旧基线；可稍后手动刷新。';
      }
      if (statusEl) statusEl.textContent = message;
      notify(message, result && result.baseline_refresh_attempted && !result.baseline_refreshed ? 'error' : 'success', 7000);
      loadCheckOptions();
      loadBaselineExit();
      pollStatus(true);
      if (recheck) pollHealthRecheck(result.health_recheck.id);
    })
    .catch(function(err) { notify(err, 'error', 7000); })
    .finally(function() {
      if (button) { button.disabled = false; button.textContent = original; }
    });
}

function loadBaselineExit() {
  fetchJSON('/api/settings/baseline-exit').then(function(result) {
    var ip = result && result.baseline_ip;
    setText('baseline-exit-ip', ip || '（尚未建立基线）');
  }).catch(function() { setText('baseline-exit-ip', '获取失败'); });
}

function refreshBaselineExit(button) {
  var statusEl = document.getElementById('baseline-exit-status');
  var original = button ? button.textContent : '';
  if (button) { button.disabled = true; button.textContent = '刷新中…'; }
  fetchJSON('/api/settings/baseline-exit', {method:'POST', headers:{'Content-Type':'application/json'}, body:'{}'})
    .then(function(result) {
      var ip = result && result.baseline_ip;
      setText('baseline-exit-ip', ip || '（刷新后仍无基线）');
      var succeeded = !!(result && result.success);
      var message = succeeded
        ? '基线出口已刷新为 ' + (ip || '未知') + (result.policy_changed ? '；旧健康结果已失效，正在全池复检。' : '。')
        : '基线刷新失败' + (ip ? '，当前仍使用旧基线 ' + ip : '，当前没有可用基线') + '。';
      if (statusEl) statusEl.textContent = message;
      notify(message, succeeded ? 'success' : 'error', 7000);
      var recheck = result && result.health_recheck && result.health_recheck.id;
      if (recheck) pollHealthRecheck(recheck);
    })
    .catch(function(err) { notify(err, 'error', 7000); })
    .finally(function() {
      if (button) { button.disabled = false; button.textContent = original; }
    });
}

function rotateNode(button) {
  var original = button ? button.textContent : '';
  if (button) { button.disabled = true; button.textContent = '轮换中…'; }
  fetchJSON('/api/nodes/rotate', {method:'POST', headers:{'Content-Type':'application/json'}, body:'{}'})
    .then(function(result) {
      var node = result && result.node;
      notify(node && node.addr ? '已轮换到 ' + node.addr : '已轮换到下一个节点', 'success');
      pollStatus(true);
    })
    .catch(function(err) { notify(err, 'error', 7000); })
    .finally(function() {
      if (button) { button.disabled = false; button.textContent = original; }
    });
}

function showNodeStats(key) {
  if (!key) return;
  fetchJSON('/api/nodes/stats?key=' + encodeURIComponent(key)).then(function(stats) {
    var lines = [
      '累计转发成功/失败: ' + (stats.successes || 0) + ' / ' + (stats.failures || 0),
      '连续健康失败: ' + (stats.consecutive_health_failures || 0) + (stats.health_failure_terminal ? '（当前不可路由，可由完整全检或人工验证恢复）' : ''),
      '最近健康成功: ' + (stats.last_health_success_at || '从未'),
      '最近延迟: ' + (stats.last_latency_ms ? stats.last_latency_ms + 'ms' : '-'),
      '当前可用: ' + (stats.available ? '是' : '否')
    ];
    showResultDialog('节点统计', lines.join('\n'));
  }).catch(function(err) { notify(err, 'error', 7000); });
}

function pollHealthRecheck(jobID) {
  if (healthRecheckPollTimer) clearTimeout(healthRecheckPollTimer);
  var statusEl = document.getElementById('check-url-status');
  healthRecheckPollTimer = setTimeout(function checkHealthRecheck() {
	fetchJSON('/api/health-recheck/status').then(function(state) {
	  var jobs = [state && state.active, state && state.pending, state && state.last];
	  var job = null;
	  jobs.some(function(candidate) {
		if (candidate && candidate.id === jobID) { job = candidate; return true; }
		return false;
	  });
	  if (!job) {
		healthRecheckPollTimer = setTimeout(checkHealthRecheck, 2500);
		return;
	  }
	  var completed = Math.max(0, Number(job.completed) || 0);
	  var total = Math.max(0, Number(job.total) || 0);
	  if (job.status === 'complete') {
		if (statusEl) statusEl.textContent = '全量复检完成：' + formatCount(job.reachable || 0) + ' 个可达，' + formatCount(job.failed || 0) + ' 个失败' + (job.policy_filtered ? '，' + formatCount(job.policy_filtered) + ' 个因出口策略排除' : '') + '。';
		healthRecheckPollTimer = null;
		pollStatus(true);
		return;
	  }
	  if (job.status === 'superseded') {
		if (statusEl) statusEl.textContent = '这轮复检已被更新的健康标准替代。';
		healthRecheckPollTimer = null;
		pollStatus(true);
		return;
	  }
	  if (statusEl) statusEl.textContent = job.status === 'queued'
		? '全量复检已排队，等待当前检查结束…'
		: '全量复检中：' + formatCount(completed) + ' / ' + formatCount(total);
	  healthRecheckPollTimer = setTimeout(checkHealthRecheck, 2000);
	}).catch(function() {
	  healthRecheckPollTimer = setTimeout(checkHealthRecheck, 3500);
	});
  }, 500);
}

function runSpeedtest(btn) {
  var key = rowKey(btn);
  if (!key || inFlightOps[key] && inFlightOps[key].speedtest) return;
  markOp(key, 'speedtest', true);
  applyNodeView();
  fetchJSON('/api/nodes/speedtest', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({key:key})})
    .then(function(j) { if (j.error) notify('测速失败: ' + j.error, 'error', 7000); else notify('测速完成，结果已写入节点数据', 'success'); })
    .catch(function(err) { notify(String(err), 'error', 7000); })
    .finally(function() {
      markOp(key, 'speedtest', false);
      pollStatus(true); // pulls the freshly-measured speed_kbps back from the backend
    });
}

function manualVerifyObservationSummary(result) {
  var lines = [];
  var attempts = Number(result && result.attempts);
  if (result && typeof result.attempts === 'number' && isFinite(attempts) && attempts >= 0) {
    lines.push('本次连通尝试：' + Math.round(attempts) + ' 次');
  }
  if (result && typeof result.available === 'boolean') {
    lines.push('当前节点状态：' + (result.available ? '仍可用' : '已下线'));
  }
  var failures = Number(result && result.consecutive_failures);
  if (result && typeof result.consecutive_failures === 'number' && isFinite(failures) && failures >= 0) {
    lines.push('健康失败观察：' + Math.round(failures) + (failures > 0 ? '（已终态过滤，可由完整全检或人工验证成功恢复）' : ''));
  }
  return lines;
}

function runVerify(btn) {
  var key = rowKey(btn);
  if (!key || inFlightOps[key] && inFlightOps[key].verify) return;
  markOp(key, 'verify', true);
  applyNodeView();
  fetchJSON('/api/nodes/verify', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({key:key})})
    .then(function(j) {
      var observation = manualVerifyObservationSummary(j);
      if (!j.reachable) {
        var failedMessage = '验证失败：本次手动复检未能连通目标。';
        if (observation.length) failedMessage += '\n' + observation.join('\n');
        failedMessage += '\n本次手动请求内部最多尝试 3 次；最终失败后节点已立即从可路由池过滤，不会继续进入轻量自动复检，但仍会参加周期性的完整全检。';
        showResultDialog('节点复检未通过', failedMessage);
        return;
      }
      var msg = '真实出口IP: ' + (j.exit_ip || '未知') + (j.city ? '(' + j.city + ')' : '') + '\n国家: ' + (j.country || '未知');
      msg += '\n本机直连出口(判断透明代理的对比基准): ' + (j.baseline_exit || '未知(探测失败)');
      if (observation.length) msg = observation.join('\n') + '\n\n' + msg;
      if (j.label_match_known === false) {
        msg += '\n\n⚠️ 缺少可比较的有效地区标签，无法判断是否一致；若本次获取到新地区，已正常保存。';
      } else if (!j.label_matched) {
        msg += '\n\n⚠️ 与列表标签不符(之前记录: ' + (j.prev_country || '未知') + ' / ' + (j.prev_exit_ip || '未知') + ')\n已用最新结果刷新该节点标签。';
      } else {
        msg += '\n\n✅ 与列表标签一致。';
      }
      showResultDialog('节点复检结果', msg);
    })
    .catch(function(err) { notify(String(err), 'error', 7000); })
    .finally(function() {
      markOp(key, 'verify', false);
      pollStatus(true);
    });
}

var listenerBindings = [];
var listenerGroupsLoaded = false;
var listenerRequest = null;

function listenerModeLabel(mode) {
  return {group:'指定分组', fixed:'固定节点', rules:'全局分流规则'}[mode] || mode || '未知';
}

function listenerStatus(view) {
  if (view.error) return '<span class="listener-state error">错误</span><span class="listener-error">' + escapeHtml(view.error) + '</span>';
  if (!view.enabled) return '<span class="listener-state muted">已停用</span>';
  return view.listening ? '<span class="listener-state online">监听中</span>' : '<span class="listener-state muted">未监听</span>';
}

function listenerTarget(view) {
  if (view.mode === 'group') return '<span class="mono">' + escapeHtml(view.group || '') + '</span>';
  if (view.mode === 'fixed') return '<span class="mono listener-key">' + escapeHtml(view.node_key || '') + '</span>';
  return '<span class="small">复用主端口规则</span>';
}

function renderListeners(listeners) {
  listenerBindings = Array.isArray(listeners) ? listeners : [];
  var body = document.getElementById('listener-tbody');
  if (!body) return;
  if (!listenerBindings.length) {
    body.innerHTML = '<tr><td class="empty" colspan="7">尚未配置附加监听端口。可在下方新增一个专用 SOCKS5 入口。</td></tr>';
    return;
  }
  body.innerHTML = listenerBindings.map(function(view) {
    var address = view.listen_addr || (view.port ? ':' + view.port : '');
    var enabled = !!view.enabled;
    return '<tr data-listener-id="' + escapeHtml(view.id) + '">' +
      '<td data-label="名称"><strong>' + escapeHtml(view.name) + '</strong></td>' +
      '<td data-label="端口 / 地址" class="mono">' + escapeHtml(String(view.port || '')) + (address ? '<span class="listener-address">' + escapeHtml(address) + '</span>' : '') + '</td>' +
      '<td data-label="模式">' + escapeHtml(listenerModeLabel(view.mode)) + '</td>' +
      '<td data-label="组 / 固定节点">' + listenerTarget(view) + '</td>' +
      '<td data-label="启用"><label class="switch"><input type="checkbox" data-action="toggle-listener" aria-label="' + (enabled ? '停用' : '启用') + '监听端口 ' + escapeHtml(view.name) + '"' + (enabled ? ' checked' : '') + '><span class="slider"></span></label></td>' +
      '<td data-label="运行状态 / 错误">' + listenerStatus(view) + '</td>' +
      '<td data-label="操作"><div class="listener-row-actions"><button type="button" class="btn-sm" data-action="edit-listener">编辑</button><button type="button" class="btn-sm danger" data-action="delete-listener">删除</button></div></td></tr>';
  }).join('');
}

function requestListenerGroups() {
  if (listenerGroupsLoaded) return Promise.resolve();
  return fetchJSON('/api/groups').then(function(groups) {
    var select = document.getElementById('listener-group');
    if (!select) return;
    var reserved = ['ANY', 'DIRECT'];
    var names = reserved.concat((Array.isArray(groups) ? groups : []).map(function(g) { return g.name; }).filter(Boolean));
    names = names.filter(function(name, index) { return names.indexOf(name) === index; });
    select.innerHTML = '<option value="">选择分组</option>' + names.map(function(name) { return '<option value="' + escapeHtml(name) + '">' + escapeHtml(name) + '</option>'; }).join('');
    listenerGroupsLoaded = true;
  }).catch(function(err) { notify('无法加载分组：' + String(err), 'error', 7000); });
}

function updateListenerNodeKeys() {
  var list = document.getElementById('listener-node-keys');
  if (!list) return;
  var nodes = (nodePageData && Array.isArray(nodePageData.nodes)) ? nodePageData.nodes : [];
  list.innerHTML = nodes.map(function(node) { return '<option value="' + escapeHtml(node.key || '') + '">' + escapeHtml(node.proxy_url || node.addr || node.key || '') + '</option>'; }).join('');
}

function requestListenerNodeKeys() {
  if (nodePageData && Array.isArray(nodePageData.nodes)) {
    updateListenerNodeKeys();
    return Promise.resolve();
  }
  return fetchJSON('/api/nodes/page?page=1&page_size=100&available=1')
    .then(function(pageData) {
      if (!nodePageData) nodePageData = pageData && typeof pageData === 'object' ? pageData : {};
      if (!Array.isArray(nodePageData.nodes)) nodePageData.nodes = [];
      updateListenerNodeKeys();
    })
    .catch(function(err) {
      updateListenerNodeKeys();
      notify('固定节点建议加载失败，仍可手工填写节点 key：' + String(err), 'error', 7000);
    });
}

function syncListenerMode() {
  var form = document.getElementById('form-listener');
  if (!form) return;
  var mode = form.mode.value;
  var groupField = document.getElementById('listener-group-field');
  var nodeField = document.getElementById('listener-node-field');
  groupField.hidden = mode !== 'group';
  nodeField.hidden = mode !== 'fixed';
  form.group.required = mode === 'group';
  form.node_key.required = mode === 'fixed';
  if (mode !== 'group') form.group.value = '';
  if (mode !== 'fixed') form.node_key.value = '';
}

function requestListeners(force) {
  if (listenerRequest && !force) return listenerRequest;
  setListNotice('listener-notice', 'loading', '正在读取监听端口…');
  listenerRequest = Promise.all([fetchJSON('/api/listeners'), requestListenerGroups(), requestListenerNodeKeys()])
    .then(function(result) { renderListeners(result[0] && result[0].listeners); updateListenerNodeKeys(); setListNotice('listener-notice', '', ''); })
    .catch(function(err) { setListNotice('listener-notice', 'error', '无法读取监听端口：' + String(err)); })
    .finally(function() { listenerRequest = null; });
  return listenerRequest;
}

function listenerFromRow(element) {
  var row = element.closest ? element.closest('tr[data-listener-id]') : null;
  var id = row ? row.getAttribute('data-listener-id') : '';
  return listenerBindings.filter(function(item) { return item.id === id; })[0] || null;
}

function resetListenerForm() {
  var form = document.getElementById('form-listener');
  if (!form) return;
  form.reset(); form.id.value = ''; form.mode.value = 'group'; form.enabled.checked = true;
  setText('listener-form-title', '新增监听端口'); setText('listener-form-help', '选择分组、固定节点或复用全局分流规则。');
  setText('listener-submit', '新增监听'); document.getElementById('listener-cancel').hidden = true;
  syncListenerMode();
}

function editListener(element) {
  var item = listenerFromRow(element); if (!item) return;
  var form = document.getElementById('form-listener');
  form.id.value = item.id || ''; form.name.value = item.name || ''; form.port.value = item.port || '';
  form.mode.value = item.mode || 'group'; form.group.value = item.group || ''; form.node_key.value = item.node_key || ''; form.enabled.checked = !!item.enabled;
  setText('listener-form-title', '编辑监听端口'); setText('listener-form-help', '保存会热更新该端口；新连接立即按新配置路由。');
  setText('listener-submit', '保存修改'); document.getElementById('listener-cancel').hidden = false;
  syncListenerMode(); form.scrollIntoView({block:'nearest', behavior:'smooth'}); form.name.focus();
}

function saveListener(form) {
  var body = {id:form.id.value.trim(), name:form.name.value.trim(), port:Number(form.port.value), mode:form.mode.value, group:form.mode.value === 'group' ? form.group.value : '', node_key:form.mode.value === 'fixed' ? form.node_key.value.trim() : '', enabled:!!form.enabled.checked};
  var updating = !!body.id;
  fetchJSON(updating ? '/api/listeners/update' : '/api/listeners', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(body)})
    .then(function() { notify(updating ? '监听端口已更新' : '监听端口已新增', 'success'); resetListenerForm(); return requestListeners(true); })
    .catch(function(err) { notify(String(err), 'error', 7000); });
}

function updateListenerEnabled(element) {
  var item = listenerFromRow(element); if (!item) return;
  var body = {id:item.id, name:item.name, port:item.port, mode:item.mode, group:item.group || '', node_key:item.node_key || '', enabled:!!element.checked};
  element.disabled = true;
  fetchJSON('/api/listeners/update', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(body)})
    .then(function() { notify(body.enabled ? '监听端口已启用' : '监听端口已停用', 'success'); return requestListeners(true); })
    .catch(function(err) { element.checked = !body.enabled; notify(String(err), 'error', 7000); })
    .finally(function() { element.disabled = false; });
}

function deleteListener(element) {
  var item = listenerFromRow(element); if (!item || !confirm('删除监听端口 “' + item.name + '”？对应监听器会立即关闭。')) return;
  fetchJSON('/api/listeners/delete', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({id:item.id})})
    .then(function() { notify('监听端口已删除', 'success'); if (document.getElementById('listener-id').value === item.id) resetListenerForm(); return requestListeners(true); })
    .catch(function(err) { notify(String(err), 'error', 7000); });
}

var validTabs = ['nodes','candidates','failed-candidates','proxyip','sources','rules','groups','listeners'];
function showTab(name) {
  if (validTabs.indexOf(name) < 0) name = 'nodes';
  var viewMeta = {
    nodes: ['转发代理池','健康节点、真实出口与全量复检。'],
    candidates: ['候选待检','等待正式检测的候选；失败会隔离到失败节点页。'],
    'failed-candidates': ['失败节点','正式检查失败或策略排除的候选；只能手动重新检测。'],
    'proxyip': ['Cloudflare ProxyIP','独立的 Worker 外部资源与专用验证。'],
    sources: ['来源管理','远程订阅、本地导入与库存保留策略。'],
    rules: ['分流规则','从上到下构建可预测的路由决策。'],
    groups: ['分组策略','组合节点、地区、协议与来源。'],
    listeners: ['监听端口','独立 SOCKS5 入口与专属路由方式。']
  };
  document.body.dataset.view = name;
  setText('page-title', viewMeta[name][0]);
  setText('page-description', viewMeta[name][1]);
  document.title = viewMeta[name][0] + ' · Proxy Atlas';
  var previousTab = currentTab;
  var leavingNodes = currentTab === 'nodes' && name !== 'nodes';
  var leavingCandidates = currentTab === 'candidates' && name !== 'candidates';
  var leavingFailed = currentTab === 'failed-candidates' && name !== 'failed-candidates';
  var leavingProxyIP = currentTab === 'proxyip' && name !== 'proxyip';
  currentTab = name;
  var panels = document.querySelectorAll('.tab-panel');
  for (var i = 0; i < panels.length; i++) {
    panels[i].style.display = 'none';
    panels[i].setAttribute('aria-hidden', 'true');
  }
  var target = document.getElementById('tab-' + name);
  if (target) {
    target.style.display = '';
    target.setAttribute('aria-hidden', 'false');
  }
  var links = document.querySelectorAll('.tab-link');
  for (var j = 0; j < links.length; j++) {
    var active = links[j].dataset.tab === name;
    links[j].classList.toggle('active', active);
    links[j].setAttribute('aria-selected', active ? 'true' : 'false');
    links[j].setAttribute('tabindex', active ? '0' : '-1');
  }
  if (leavingNodes || !pageIsVisible()) abortNodeRequest();
  if (leavingCandidates || !pageIsVisible()) abortCandidateRequest();
  if (leavingFailed || !pageIsVisible()) abortFailedRequest();
  if (leavingProxyIP || !pageIsVisible()) abortProxyIPRequest();
  if (name === 'nodes' && pageIsVisible()) requestNodes(true);
  if (name === 'candidates' && pageIsVisible()) requestCandidates(true);
  if (name === 'failed-candidates' && pageIsVisible()) requestFailedCandidates(true);
  if (name === 'proxyip' && pageIsVisible()) requestProxyIPs(true);
  if (name === 'listeners' && pageIsVisible()) requestListeners(true);
  if (previousTab !== name && target) {
    requestAnimationFrame(function(){ target.scrollIntoView({block:'start', behavior:'auto'}); });
  }
}

function syncTabFromHash() {
  var requested = (location.hash || '#nodes').slice(1);
  if (validTabs.indexOf(requested) < 0) {
    requested = 'nodes';
    history.replaceState(null, '', location.pathname + location.search + '#nodes');
  }
  showTab(requested);
}

window.addEventListener('hashchange', syncTabFromHash);
window.addEventListener('resize', function() {
  if (viewportPageSizeTimer) clearTimeout(viewportPageSizeTimer);
  viewportPageSizeTimer = setTimeout(function() {
    viewportPageSizeTimer = null;
    applyResponsiveCatalogPageSizes();
  }, 150);
});
document.querySelector('.tabs').addEventListener('keydown', function(e) {
  if (['ArrowLeft','ArrowRight','Home','End'].indexOf(e.key) < 0) return;
  var links = Array.prototype.slice.call(document.querySelectorAll('.tab-link'));
  var index = links.indexOf(document.activeElement);
  if (index < 0) return;
  e.preventDefault();
  if (e.key === 'Home') index = 0;
  else if (e.key === 'End') index = links.length - 1;
  else index = (index + (e.key === 'ArrowRight' ? 1 : -1) + links.length) % links.length;
  location.hash = links[index].dataset.tab;
  links[index].focus();
});
document.addEventListener('visibilitychange', function() {
  if (!pageIsVisible()) {
    abortNodeRequest();
    abortCandidateRequest();
    abortFailedRequest();
    abortProxyIPRequest();
    return;
  }
  pollStatus(true);
  schedulePoll(15000);
});

document.addEventListener('keydown', function(e) {
  trapModalFocus(e);
  if (e.key === 'Escape') { closeCandidateCountryPicker(); closeResultDialog(); }
});

document.addEventListener('click', function(event) {
  var actionElement = event.target.closest ? event.target.closest('[data-action]') : null;
  if (!actionElement) return;
  if (actionElement.disabled || actionElement.getAttribute('aria-disabled') === 'true') {
    if (actionElement.getAttribute('data-action') !== 'candidate-country-backdrop' &&
        actionElement.getAttribute('data-action') !== 'result-dialog-backdrop') return;
  }
  switch (actionElement.getAttribute('data-action')) {
    case 'refresh': doRefresh(actionElement); break;
    case 'save-check-url': saveCheckURL(actionElement); break;
    case 'save-check-options': saveCheckOptions(actionElement); break;
    case 'refresh-baseline': refreshBaselineExit(actionElement); break;
    case 'rotate-node': rotateNode(actionElement); break;
    case 'node-stats': showNodeStats(actionElement.getAttribute('data-key') || ''); break;
    case 'set-auto': setAuto(); break;
    case 'open-node-country-picker': openNodeCountryPicker(); break;
    case 'open-candidate-country-picker': openCandidateCountryPicker(); break;
    case 'export-nodes': exportNodes(actionElement.getAttribute('data-format')); break;
    case 'clear-unavailable': clearUnavailable(); break;
    case 'refresh-source': refreshSource(actionElement); break;
    case 'save-source-auto-refresh': saveSourceAutoRefresh(actionElement); break;
    case 'delete-source':
      if (confirm('删除来源 ' + (actionElement.getAttribute('data-source-name') || '') + '?')) postJSON('/api/sources/delete', {id:actionElement.getAttribute('data-source-id')}, reloadOrAlert);
      break;
    case 'preset-gfw':
      if (confirm('用 GFW 分流预设覆盖当前所有规则?')) postJSON('/api/rules/preset-gfw', {}, reloadOrAlert);
      break;
    case 'move-rule': postJSON('/api/rules/move', {id:actionElement.getAttribute('data-rule-id'), delta:Number(actionElement.getAttribute('data-delta'))}, reloadOrAlert); break;
    case 'delete-rule':
      if (confirm('删除规则?')) postJSON('/api/rules/delete', {id:actionElement.getAttribute('data-rule-id')}, reloadOrAlert);
      break;
    case 'save-default-group': postJSON('/api/rules/default', {group:document.getElementById('default-group-select').value}, reloadOrAlert); break;
    case 'delete-group':
      if (confirm('删除分组 ' + (actionElement.getAttribute('data-group-name') || '') + '? 若仍有规则引用，请先删除或改写对应规则。')) postJSON('/api/groups/delete', {id:actionElement.getAttribute('data-group-id')}, reloadOrAlert);
      break;
    case 'candidate-country-backdrop': candidateCountryBackdrop(event); break;
    case 'close-candidate-country-picker': closeCandidateCountryPicker(); break;

    case 'result-dialog-backdrop': resultDialogBackdrop(event); break;
    case 'close-result-dialog': closeResultDialog(); break;
    case 'choose-candidate-protocol': chooseCandidateProtocol(actionElement.getAttribute('data-protocol') || ''); break;
    case 'set-candidate-continent': setCandidateContinentFilter(actionElement.getAttribute('data-continent') || ''); break;
    case 'choose-candidate-country': chooseCandidateCountry(actionElement.getAttribute('data-country') || ''); break;
    case 'proxyip-verify': runProxyIPVerify(actionElement); break;
    case 'candidate-batch-check': startCandidateBatchCheck(actionElement); break;
    case 'failed-retry-selected': retryFailedCandidates(); break;
    case 'failed-retry-all': retryAllFailedCandidates(); break;
    case 'cancel-candidate-check': cancelCandidateCheck(); break;
    case 'failed-select': toggleFailedSelection(actionElement); return;
    case 'failed-select-page': toggleFailedPageSelection(actionElement); return;
    case 'goto-failed-page': gotoFailedPage(actionElement.getAttribute('data-page')); break;
    case 'open-proxyip-country-picker': openProxyIPCountryPicker(); break;
    case 'goto-proxyip-page': gotoProxyIPPage(actionElement.getAttribute('data-page')); break;
    case 'node-select': toggleNodeSelection(actionElement); return;
    case 'node-select-page': toggleNodePageSelection(actionElement); return;
    case 'copy-selected-nodes': copySelectedNodes(actionElement); break;
    case 'speedtest-selected-nodes': speedtestSelectedNodes(); break;
    case 'filter-node-availability': filterNodeAvailability(actionElement.getAttribute('data-availability') || ''); break;
    case 'candidate-select': toggleCandidateSelection(actionElement); return;
    case 'candidate-select-page': toggleCandidatePageSelection(actionElement); return;
    case 'candidate-speedtest': speedtestCandidates([rowKey(actionElement)]); break;
    case 'candidate-speedtest-selected': speedtestCandidates(selectedCandidateList()); break;
    case 'candidate-delete': deleteCandidate(actionElement); break;
    case 'candidate-delete-selected': deleteCandidates(selectedCandidateList()); break;
    case 'copy': copyAddrFrom(actionElement); break;
    case 'toggle-candidate-details': toggleCandidateDetails(actionElement); break;
    case 'goto-candidate-page': gotoCandidatePage(actionElement.getAttribute('data-page')); break;
    case 'switch-node': switchNode(actionElement); break;
    case 'speedtest': runSpeedtest(actionElement); break;
    case 'verify': runVerify(actionElement); break;
    case 'delete-node': deleteNode(actionElement); break;
    case 'details':
      if (actionElement.closest('#candidate-tbody')) toggleCandidateDetails(actionElement);
      else toggleNodeDetails(actionElement);
      break;
    case 'toggle-node-details': toggleNodeDetails(actionElement); break;
    case 'goto-node-page': gotoPage(actionElement.getAttribute('data-page')); break;
    case 'refresh-listeners': requestListeners(true); break;
    case 'edit-listener': editListener(actionElement); break;
    case 'toggle-listener': updateListenerEnabled(actionElement); return;
    case 'delete-listener': deleteListener(actionElement); break;
    case 'cancel-listener-edit': resetListenerForm(); break;
    default: return;
  }
  if (actionElement && (actionElement.tagName === 'INPUT' || actionElement.tagName === 'SELECT' || actionElement.tagName === 'TEXTAREA' || actionElement.tagName === 'LABEL' || actionElement.type === 'checkbox' || actionElement.type === 'radio')) {
    return;
  }
  event.preventDefault();
});

syncNodePageSizeSelect();
syncCandidatePageSizeSelect();
syncFailedPageSizeSelect();
syncProxyIPPageSizeSelect();
syncTabFromHash();
loadCheckOptions();
loadBaselineExit();
// Restore any candidate check operation that was already queued or running
// before this page load. Without this, a page reload loses visibility of
// the in-flight task and the button appears disabled without explanation.
(function restoreInFlightCandidateCheck() {
  fetchJSON('/api/candidates/batch-check/status').then(function(operation) {
    if (!operation || ['queued', 'running'].indexOf(operation.status) < 0) return;
    var statusURL = operation.kind === 'failed_retry'
      ? '/api/failed-candidates/retry/status'
      : '/api/candidates/batch-check/status';
    pollCandidateCheckOperation(statusURL, String(operation.id || ''));
  }).catch(function() {});
}());
pollStatus(false);
schedulePoll(15000);


function sourceImportCandidateCount(result) {
  var counts = result && result.counts;
  var values = [
    result && result.candidate_count,
    result && result.imported_count,
    result && result.parsed_count,
    counts && counts.candidates,
    counts && counts.imported,
    counts && counts.parsed
  ];
  for (var i = 0; i < values.length; i++) {
    var value = Number(values[i]);
    if (isFinite(value) && value >= 0) return Math.round(value);
  }
  return null;
}

function sourceImportRefreshSummary(result) {
  var operation = result && (result.refresh || result.operation || result.refresh_operation);
  if (!operation) return '刷新操作已提交。';
  if (operation.coalesced) return '刷新请求已与现有任务合并。';
  if (operation.status === 'running') return '刷新任务正在运行。';
  if (operation.status === 'queued') return '刷新任务已排队。';
  return '刷新操作已创建。';
}

document.getElementById('form-import-source').addEventListener('submit', function(e) {
  e.preventDefault();
  var form = e.target;
  var name = String(form.elements.name.value || '').trim();
  var fileInput = form.elements.file;
  var file = fileInput && fileInput.files ? fileInput.files[0] : null;
  var status = document.getElementById('source-import-status');
  var submit = document.getElementById('source-import-submit');
  if (!name || !file) {
    notify('请填写来源名称并选择代理列表文本文件', 'error');
    return;
  }
  if (file.size > 16 * 1024 * 1024) {
    notify('文件超过 16 MiB 上限，未发送到服务端', 'error', 7000);
    return;
  }
  var originalLabel = submit.textContent;
  submit.disabled = true;
  submit.textContent = '导入中…';
  if (status) status.textContent = '正在安全上传并创建本地来源…';
  var formData = new FormData();
  formData.append('name', name);
  formData.append('file', file);
  fetchJSON('/api/sources/import', {method:'POST', body:formData})
    .then(function(result) {
      var count = sourceImportCandidateCount(result);
      var message = count === null
        ? '本地来源已创建。'
        : '本地来源已创建，识别 ' + formatCount(count) + ' 条候选。';
      message += sourceImportRefreshSummary(result) + ' 页面即将更新来源列表。';
      form.reset();
      if (status) status.textContent = message;
      notify(message, 'success', 7000);
      setTimeout(function(){ location.hash = 'sources'; location.reload(); }, 1800);
    })
    .catch(function(err) {
      var message = '导入失败：' + String(err);
      if (status) status.textContent = message;
      notify(message, 'error', 7000);
    })
    .finally(function() {
      submit.disabled = false;
      submit.textContent = originalLabel;
    });
});

document.getElementById('form-add-source').addEventListener('submit', function(e) {
  e.preventDefault();
  var f = e.target;
  postJSON('/api/sources', {
    name: f.name.value, url: f.url.value, format: f.format.value, protocol: f.protocol.value,
    allow_private: !!f.allow_private.checked,
    allow_empty: !!f.allow_empty.checked
  }, function(err) { if (err) { notify(err, 'error', 7000); } else { location.hash = 'sources'; location.reload(); } });
});

document.getElementById('form-add-rule').addEventListener('submit', function(e) {
  e.preventDefault();
  var f = e.target;
  postJSON('/api/rules', {
    type: f.type.value, value: f.value.value, group: f.group.value
  }, function(err) { if (err) { notify(err, 'error', 7000); } else { location.hash = 'rules'; location.reload(); } });
});

document.getElementById('form-add-group').addEventListener('submit', function(e) {
  e.preventDefault();
  var f = e.target;
  function splitList(v) { return v.split(',').map(function(s){ return s.trim(); }).filter(Boolean); }
  postJSON('/api/groups', {

    name: f.name.value, strategy: f.strategy.value, nodes: splitList(f.nodes.value),
    countries: splitList(f.countries.value), protocols: splitList(f.protocols.value), sources: splitList(f.sources.value)
  }, function(err) { if (err) { notify(err, 'error', 7000); } else { location.hash = 'groups'; location.reload(); } });
});

document.getElementById('form-listener').addEventListener('submit', function(e) {
  e.preventDefault();
  saveListener(e.target);
});
document.getElementById('listener-mode').addEventListener('change', syncListenerMode);
