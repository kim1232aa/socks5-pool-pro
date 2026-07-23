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
        var requestError = new Error((data && data.error) || ('请求失败 (HTTP ' + r.status + ')'));
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

function protoBadge(p) { return '<span class="proto proto-' + escapeHtml(p) + '">' + escapeHtml(p) + '</span>'; }

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
  return '<span title="' + escapeHtml(title) + '">' + speedText + '</span><span class="speed-meta">' + escapeHtml(testedText) + '<br>' + bytesText + ' / ' + durationText + '</span>';
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
var proxyIPVerifyCache = Object.create(null);
var expandedNodeRows = Object.create(null);
var expandedCandidateRows = Object.create(null);
var candidateContinentFilter = '';
var candidateCountryTrigger = null;
var countryPickerScope = 'candidates';
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
    {value:'https', label:'HTTPS', note:'HTTP CONNECT 来源标签'},
    {value:'proxyip', label:'ProxyIP', note:'Cloudflare 资源 · 仅 443 / 纯 IP'}
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
  return countryPickerScope === 'nodes' ? nodeCountryPickerSummaries() : candidateCountrySummaries();
}

function pickerUnknownCountryCounts() {
  return countryPickerScope === 'nodes' ? nodeUnknownCountryCounts() : {total:candidateUnknownCountryTotal(), available:0};
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
  var inputId = countryPickerScope === 'nodes' ? 'f-country' : 'cf-country';
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
  var proxyIPMode = String((document.getElementById('cf-proto') || {}).value || '').toLowerCase() === 'proxyip';
  var prefix = proxyIPMode ? 'Cloudflare ProxyIP · ' : '';
  button.textContent = prefix + (value === '__unknown__' ? '来源地区未知' : (value ? countryLabel(value) : '全部来源地区'));
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
  } else {
    var proxyIPMode = String((document.getElementById('cf-proto') || {}).value || '').toLowerCase() === 'proxyip';
    if (title) title.textContent = proxyIPMode ? '选择 Cloudflare ProxyIP 来源地区' : '选择候选来源地区';
    if (mapTitle) mapTitle.textContent = '先选大洲，再选国家或地区';
    if (note) note.textContent = proxyIPMode
      ? '只浏览端口集合含 443 的纯 IP 资源；它不接受 SOCKS/HTTP 的端口与认证参数。'
      : '地区来自来源元数据，不等于经过代理拨号实测的出口地区。';
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
  var input = document.getElementById(countryPickerScope === 'nodes' ? 'f-country' : 'cf-country');
  if (!input) return;
  input.value = country;
  if (countryPickerScope === 'nodes') updateNodeCountryButton();
  else updateCandidateCountryButton();
  closeCandidateCountryPicker();
  if (countryPickerScope === 'nodes') onFilterChange();
  else onCandidateFilterChange();
}

function candidateStatusTotal(status) {
  var total = 0;
  candidateFacetList('statuses').forEach(function(item) {
    if (String((item && item.status) || '') === status) total = Number(item.total || 0);
  });
  return Math.max(0, total);
}

function candidateStatusBadge(status) {
  var labels = {
    known_available:'池内可用',
    known_unavailable:'池内不可用',
    checked_failed:'最近检测失败',
    policy_filtered:'连通但被策略排除',
    resource:'Cloudflare 资源（不路由）',
    deferred:'排队待检测'
  };
  var classes = {
    known_available:'available',
    known_unavailable:'unavailable',
    checked_failed:'failed',
    policy_filtered:'policy',
    resource:'resource',
    deferred:'deferred'
  };
  var key = String(status || 'deferred');
  return '<span class="candidate-state candidate-state-' + (classes[key] || 'unknown') + '">' + escapeHtml(labels[key] || '状态未知') + '</span>';
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
  var rows = document.querySelectorAll('#candidate-tbody tr[data-key]');
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
  var row = button && button.closest ? button.closest('#candidate-tbody tr[data-key]') : null;
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
  candidatePageData = pageData && typeof pageData === 'object' ? pageData : {};
  if (!Array.isArray(candidatePageData.candidates)) candidatePageData.candidates = [];
  ['statuses','sources','protocols','countries'].forEach(function(name) {
    if (!Array.isArray(candidatePageData[name])) candidatePageData[name] = [];
  });
  candidatePage = Number(candidatePageData.page) > 0 ? Number(candidatePageData.page) : 1;
  candidateSnapshotID = String(candidatePageData.snapshot_id || '');
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
  ['socks5','http','https','proxyip'].forEach(function(value) {
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

function showCandidateProtocol(protocol) {
  var sel = document.getElementById('cf-proto');
  if (sel) sel.value = protocol || '';
  candidatePage = 1;
  candidateQueryGeneration++;
  if (location.hash !== '#candidates') location.hash = 'candidates';
  else requestCandidates(true);
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
  var known = candidateStatusTotal('known_available') + candidateStatusTotal('known_unavailable');
  var deferred = candidateStatusTotal('deferred');
  var failed = candidateStatusTotal('checked_failed');
  var policyFiltered = candidateStatusTotal('policy_filtered');

  setText('candidate-total', formatCount(catalogTotal));
  setText('tab-link-candidates', '候选库 (' + formatCount(catalogTotal) + ')');
  setText('candidate-matching', formatCount(total));
  setText('stat-matching', formatCount(total));
  setText('candidate-known', formatCount(known));
  setText('candidate-deferred', formatCount(deferred));
  setText('candidate-country-unknown', formatCount(candidateUnknownCountryTotal()));
  var updated = formatCandidateUpdatedAt(data.updated_at);
  var phaseLabels = {checking:'检查中', complete:'已完成', partial:'部分来源失败（已保留旧目录）', loading:'生成中', restored:'已恢复目录，等待按当前标准复检'};
  var phase = data.phase ? (' · 快照' + (phaseLabels[data.phase] || data.phase)) : '';
  setText('candidate-count', (total ? ('显示 ' + formatCount(start + 1) + '-' + formatCount(start + rows.length) + ' · 匹配 ' + formatCount(total)) : '匹配 0') + ' · 完整目录 ' + formatCount(catalogTotal) + ' · 最近失败 ' + formatCount(failed) + (policyFiltered ? ' · 策略排除 ' + formatCount(policyFiltered) : '') + phase + (updated ? ' · 更新于 ' + updated : ''));

  if (!catalogTotal) {
    var emptyMessage = data.phase === 'loading' || data.phase === 'checking'
      ? '候选快照正在生成，完成后会自动显示。'
      : '完整候选快照尚未生成，请确认已启用来源后刷新。';
    tbody.innerHTML = '<tr><td colspan="6" class="empty">' + emptyMessage + '</td></tr>';
    renderCandidatePagers('');
    restoreCandidateFocus(savedFocus);
    return;
  }
  if (!total) {
    tbody.innerHTML = '<tr><td colspan="6" class="empty">没有符合当前筛选条件的候选</td></tr>';
    renderCandidatePagers('');
    restoreCandidateFocus(savedFocus);
    return;
  }

  tbody.innerHTML = rows.map(function(candidate) {
    var country = normalizedCountry(candidate.country);
    var location = country ? countryLabel(country) : '🏳️ 国家未知';
    if (candidate.city) location += ' · ' + String(candidate.city);
    var sources = Array.isArray(candidate.source_names) && candidate.source_names.length ? candidate.source_names.join(', ') : (candidate.source || '');
    var status = candidate.status || (candidate.routable === false ? 'resource' : 'deferred');
    var candidateKey = String(candidate.key || '');
    var candidateExpanded = !!expandedCandidateRows[candidateKey];
    return '<tr class="' + (candidateExpanded ? 'mobile-expanded' : '') + '" data-key="' + escapeHtml(candidateKey) + '">' +
      '<td data-label="状态">' + candidateStatusBadge(status) + '</td>' +
      '<td data-label="协议">' + protoBadge(candidate.protocol || '') + (candidate.has_auth ? '<span class="auth-badge" title="该上游候选需要用户名/密码；凭据不会在目录接口中返回">需认证</span>' : '') + '</td>' +
      '<td data-label="候选地址" class="mono">' + escapeHtml(candidate.addr || '') + '<button type="button" class="copy-btn" data-action="copy" data-copy-address="' + escapeHtml(candidate.addr || '') + '" aria-label="复制候选地址">复制</button><button type="button" class="mobile-detail-toggle" data-action="details" aria-expanded="' + (candidateExpanded ? 'true' : 'false') + '">' + (candidateExpanded ? '收起' : '详情') + '</button></td>' +
      '<td data-label="来源标注地区">' + escapeHtml(location) + '</td>' +
      '<td data-label="来源" class="small mobile-secondary">' + escapeHtml(sources) + '<span class="candidate-readonly"> · 只读候选</span></td>' +
      '<td data-label="专用验证" class="candidate-verify-cell mobile-secondary">' + proxyIPVerifyCellHTML(candidate.key, candidate.protocol) + '</td></tr>';
  }).join('');

  if (total <= pageSize) {
    renderCandidatePagers('');
  } else {
    renderCandidatePagers(
      '<button type="button" class="btn-sm" data-action="goto-candidate-page" data-page="' + (page - 1) + '" ' + (page <= 1 ? 'disabled' : '') + '>上一页</button>' +
      '<span class="small">第 ' + page + ' / ' + pageCount + ' 页</span>' +
      '<button type="button" class="btn-sm" data-action="goto-candidate-page" data-page="' + (page + 1) + '" ' + (page >= pageCount ? 'disabled' : '') + '>下一页</button>');
  }
  restoreCandidateFocus(savedFocus);
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
    tbody.innerHTML = '<tr><td colspan="12" class="empty">池内暂无节点，等待下次抓取周期...</td></tr>';
    renderNodePagers('');
    if (banner) banner.textContent = '无 (代理池为空)';
    restoreNodeFocus(savedFocus);
    return;
  }
  if (!total) {
    tbody.innerHTML = '<tr><td colspan="12" class="empty">没有匹配的节点</td></tr>';
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
        '<button type="button" class="mobile-detail-toggle" data-action="details" aria-expanded="' + (rowExpanded ? 'true' : 'false') + '">' + (rowExpanded ? '收起' : '详情') + '</button></div>';
      html += '<tr class="' + (n.active ? 'active ' : '') + (n.available === false ? 'unavail ' : '') + (rowExpanded ? 'mobile-expanded' : '') + '" data-key="' + escapeHtml(n.key) + '">' +
        '<td data-label="状态">' + (n.active ? '<span class="badge-inuse">使用中</span>' : (n.source_retired ? '<span class="badge-unavail">来源已停用</span>' : (n.health_invalidated ? '<span class="badge-unavail">等待当前标准复检</span>' : (n.policy_excluded ? '<span class="badge-unavail">出口策略排除</span>' : (n.available === false ? '<span class="badge-unavail">暂不可用</span>' : '<span class="small">可用</span>'))))) + '</td>' +
        '<td data-label="协议">' + protoBadge(n.protocol) + '</td>' +
        '<td data-label="地址(节点IP)" class="mono">' + escapeHtml(n.addr) + '<button type="button" class="copy-btn" data-action="copy" data-copy-address="' + escapeHtml(n.addr) + '" aria-label="复制节点地址">复制</button></td>' +
        '<td data-label="出口IP" class="mobile-secondary">' + exitCell + '</td>' +
        '<td data-label="匿名" class="mobile-secondary">' + anonBadge(n.anonymity) + '</td>' +
        '<td data-label="国家/城市">' + (loc || '<span class="small">-</span>') + '</td>' +
        '<td data-label="评分">' + scoreCell(n.score) + '</td>' +
        '<td data-label="成功/失败" class="small mobile-secondary">' + sf + '</td>' +
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

  if (banner) {
    var lockUI = anyPinned
      ? '<span class="lock-badge">🔒 手动锁定</span><button type="button" class="btn-sm" data-action="set-auto">恢复自动轮换</button>'
      : '<span class="auto-badge">🔄 自动轮换中</span>';
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
  if (typeof d.proxyip_total === 'number') setText('stat-proxyip', d.proxyip_total);
  var lastDisplay = localizedStatusTime(d.last_scrape_at, d.last_scrape, 'N/A');
  var nextDisplay = localizedStatusTime(d.next_scrape_at, d.next_scrape, 'N/A');
  setText('stat-last', lastDisplay);
  setText('stat-next', nextDisplay);
  setText('timeline-last', localizedStatusTime(d.last_scrape_at, d.last_scrape, '尚未刷新'));
  setText('timeline-next', localizedStatusTime(d.next_scrape_at, d.next_scrape, '等待调度'));
  lastKnownScrape = d.last_scrape_at || d.last_scrape || '';
  lastKnownNextScrape = d.next_scrape_at || d.next_scrape || '';

  if (typeof d.candidate_total === 'number') {
    var candidateTotal = Math.max(0, Number(d.candidate_total));
    setText('tab-link-candidates', '候选库 (' + formatCount(candidateTotal) + ')');
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
function nodePageURL() {
  var q = [
    'page=' + encodeURIComponent(nodePage),
    'page_size=' + encodeURIComponent(nodePageSize)
  ];
  var text = (document.getElementById('f-text').value || '').trim();
  var country = document.getElementById('f-country').value;
  var protocol = document.getElementById('f-proto').value;
  var sort = document.getElementById('f-sort').value;
  if (text) q.push('search=' + encodeURIComponent(text));
  if (country) q.push('country=' + encodeURIComponent(country));
  if (protocol) q.push('protocol=' + encodeURIComponent(protocol));
  if (sort) q.push('sort=' + encodeURIComponent(sort));
  if (document.getElementById('f-ipchanged').checked) q.push('only_changed=1');
  if (document.getElementById('f-hide-unavail').checked) q.push('available=1');
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
  nodesRequest = fetchJSON(nodePageURL(), options)
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
  var text = (document.getElementById('cf-text').value || '').trim();
  var source = document.getElementById('cf-source').value;
  var protocol = document.getElementById('cf-proto').value;
  var country = document.getElementById('cf-country').value;
  var status = document.getElementById('cf-status').value;
  if (text) q.push('search=' + encodeURIComponent(text));
  if (source) q.push('source=' + encodeURIComponent(source));
  if (protocol) q.push('protocol=' + encodeURIComponent(protocol));
  if (country) q.push('country=' + encodeURIComponent(country));
  if (status) q.push('status=' + encodeURIComponent(status));
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
  candidatesRequest = fetchJSON(candidatePageURL(), options)
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

function requestCurrentCatalog(force) {
  if (currentTab === 'nodes') return requestNodes(!!force);
  if (currentTab === 'candidates') return requestCandidates(!!force);
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
    lines.push('连续失败观察：' + Math.round(failures) + '/3');
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
        if (typeof j.available === 'boolean' || typeof j.consecutive_failures === 'number') {
          failedMessage += '\n本次手动请求（内部最多 3 次连通尝试）只记为 1 次健康观察；连续 3 次失败观察才会下线。';
        }
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

function showTab(name) {
  var validTabs = ['nodes','candidates','sources','rules','groups'];
  if (validTabs.indexOf(name) < 0) name = 'nodes';
  var viewMeta = {
    nodes: ['转发代理池','健康节点、真实出口与全量复检。'],
    candidates: ['候选库存','按资源类型与来源地区浏览完整快照。'],
    sources: ['来源订阅','抓取入口、格式与库存保留策略。'],
    rules: ['分流规则','从上到下构建可预测的路由决策。'],
    groups: ['分组策略','组合节点、地区、协议与来源。']
  };
  document.body.dataset.view = name;
  setText('page-title', viewMeta[name][0]);
  setText('page-description', viewMeta[name][1]);
  document.title = viewMeta[name][0] + ' · Proxy Atlas';
  var previousTab = currentTab;
  var leavingNodes = currentTab === 'nodes' && name !== 'nodes';
  var leavingCandidates = currentTab === 'candidates' && name !== 'candidates';
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
  if (name === 'nodes' && pageIsVisible()) requestNodes(true);
  if (name === 'candidates' && pageIsVisible()) requestCandidates(true);
  if (previousTab !== name && target) {
    requestAnimationFrame(function(){ target.scrollIntoView({block:'start', behavior:'auto'}); });
  }
}

function syncTabFromHash() {
  var requested = (location.hash || '#nodes').slice(1);
  if (['nodes','candidates','sources','rules','groups'].indexOf(requested) < 0) {
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
    case 'show-candidate-protocol': showCandidateProtocol(actionElement.getAttribute('data-protocol')); break;
    case 'save-check-url': saveCheckURL(actionElement); break;
    case 'open-node-country-picker': openNodeCountryPicker(); break;
    case 'open-candidate-country-picker': openCandidateCountryPicker(); break;
    case 'export-nodes': exportNodes(actionElement.getAttribute('data-format')); break;
    case 'clear-unavailable': clearUnavailable(); break;
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
    case 'copy': copyAddrFrom(actionElement); break;
    case 'toggle-candidate-details': toggleCandidateDetails(actionElement); break;
    case 'goto-candidate-page': gotoCandidatePage(actionElement.getAttribute('data-page')); break;
    case 'switch-node': switchNode(actionElement); break;
    case 'speedtest': runSpeedtest(actionElement); break;
    case 'details':
      if (actionElement.closest('#candidate-tbody')) toggleCandidateDetails(actionElement);
      else toggleNodeDetails(actionElement);
      break;
    case 'toggle-node-details': toggleNodeDetails(actionElement); break;
    case 'goto-node-page': gotoPage(actionElement.getAttribute('data-page')); break;
    case 'set-auto': setAuto(); break;
    default: return;
  }
  event.preventDefault();
});

syncNodePageSizeSelect();
syncCandidatePageSizeSelect();
syncTabFromHash();
pollStatus(false);
schedulePoll(15000);

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
