package main

const dashboardHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>SOCKS5 Pool Status</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:system-ui,-apple-system,sans-serif;background:#0f172a;color:#e2e8f0;padding:12px}
.container{max-width:1100px;margin:0 auto}
h1{font-size:1.3rem;color:#38bdf8}
a{color:#38bdf8}
.top{display:flex;justify-content:space-between;align-items:center;flex-wrap:wrap;gap:8px}
.gh-link{color:#64748b;text-decoration:none;display:inline-flex;align-items:center;gap:4px;font-size:0.8rem}
.gh-link svg{width:18px;height:18px;fill:currentColor}
.stats{background:#1e293b;border-radius:8px;padding:12px 16px;margin:12px 0;display:flex;justify-content:space-between;align-items:center;flex-wrap:wrap;gap:12px}
.stat-item{font-size:0.8rem;color:#94a3b8}
.stat-item span{color:#e2e8f0;font-family:monospace}
.btn{background:#38bdf8;color:#0f172a;border:none;padding:6px 14px;border-radius:6px;cursor:pointer;font-weight:bold;font-size:0.8rem}
.btn:hover{background:#7dd3fc}
.btn:disabled{background:#334155;color:#64748b;cursor:not-allowed}
.btn-sm{background:#334155;color:#e2e8f0;border:none;padding:4px 10px;border-radius:5px;cursor:pointer;font-size:0.75rem;margin-right:4px}
.btn-sm:hover{background:#475569}
.btn-sm.danger{background:#450a0a;color:#fca5a5}
.btn-sm.danger:hover{background:#7f1d1d}
.tabs{display:flex;gap:4px;margin:16px 0 0;border-bottom:1px solid #1e293b}
.tab-link{padding:8px 14px;font-size:0.85rem;color:#94a3b8;text-decoration:none;border-bottom:2px solid transparent}
.tab-link.active{color:#38bdf8;border-color:#38bdf8}
.tab-panel{padding:14px 0}
.group-cards{display:flex;flex-wrap:wrap;gap:10px;margin-bottom:14px}
.group-card{background:#1e293b;border-radius:8px;padding:10px 14px;min-width:150px}
.group-card.direct{opacity:0.7}
.gc-name{font-weight:bold;color:#38bdf8;font-size:0.9rem}
.gc-strategy{color:#94a3b8;font-size:0.75rem}
.gc-count{font-size:0.8rem;margin-top:4px}
.gc-current{font-size:0.75rem;color:#4ade80;font-family:monospace;word-break:break-all}
table{width:100%;border-collapse:collapse;font-size:0.85rem}
th{text-align:left;color:#94a3b8;font-size:0.75rem;padding:6px 8px;border-bottom:1px solid #1e293b}
td{padding:6px 8px;border-bottom:1px solid #1e293b1a}
tr:hover td{background:#1e293b55}
.mono{font-family:monospace}
.small{font-size:0.75rem;color:#94a3b8}
.note{color:#64748b;font-size:0.75rem;margin:8px 0}
.note-inline{color:#64748b;font-size:0.7rem}
.empty{text-align:center;padding:30px;color:#64748b}
.proto{padding:1px 7px;border-radius:4px;font-size:0.7rem;font-weight:bold}
.proto-socks5{background:#0c4a6e;color:#7dd3fc}
.proto-http{background:#14532d;color:#86efac}
.proto-https{background:#3b0764;color:#d8b4fe}
.proto-proxyip{background:#451a03;color:#fdba74}
.current-node{background:#0c2a1a;border:1px solid #14532d;border-radius:8px;padding:10px 14px;margin:12px 0;font-size:0.9rem;color:#94a3b8}
.current-node .cn-addr{color:#4ade80;font-family:monospace;font-weight:bold;font-size:1rem}
.current-node .cn-meta{color:#64748b;font-size:0.78rem;margin-left:6px}
tr.active td{background:#14311f !important}
tr.unavail{opacity:0.5}
.badge-inuse{background:#065f46;color:#4ade80;padding:1px 7px;border-radius:4px;font-size:0.68rem;font-weight:bold}
.badge-unavail{background:#3f1d1d;color:#fca5a5;padding:1px 7px;border-radius:4px;font-size:0.68rem;font-weight:bold}
.exit-diff{color:#fbbf24}
.table-scroll{overflow-x:auto}
.filter-bar{display:flex;flex-wrap:wrap;gap:8px;align-items:center;margin:10px 0}
.filter-bar .chk{font-size:0.8rem;color:#94a3b8;display:flex;align-items:center;gap:4px}
.anon{padding:1px 6px;border-radius:4px;font-size:0.68rem;font-weight:bold}
.anon-elite{background:#064e3b;color:#6ee7b7}
.anon-anonymous{background:#1e3a8a;color:#93c5fd}
.anon-transparent{background:#7f1d1d;color:#fca5a5}
.anon-unknown{background:#334155;color:#94a3b8}
.score{font-weight:bold}
.score-hi{color:#4ade80}.score-mid{color:#fbbf24}.score-lo{color:#f87171}
.copy-btn{cursor:pointer;color:#64748b;margin-left:6px;font-size:0.7rem}
.copy-btn:hover{color:#38bdf8}
.preset-bar{background:#1e293b;border-radius:8px;padding:12px 14px;margin:12px 0;font-size:0.82rem;color:#94a3b8;display:flex;flex-wrap:wrap;gap:10px;align-items:center}
.dot{display:inline-block;width:8px;height:8px;border-radius:50%}
form.inline{display:flex;flex-wrap:wrap;gap:8px;margin-top:10px;align-items:center}
input,select{background:#1e293b;border:1px solid #334155;color:#e2e8f0;padding:6px 8px;border-radius:6px;font-size:0.8rem}
input::placeholder{color:#64748b}
.switch{position:relative;display:inline-block;width:36px;height:20px}
.switch input{opacity:0;width:0;height:0}
.slider{position:absolute;cursor:pointer;inset:0;background:#334155;border-radius:20px;transition:.15s}
.slider:before{position:absolute;content:"";height:14px;width:14px;left:3px;bottom:3px;background:#e2e8f0;border-radius:50%;transition:.15s}
input:checked + .slider{background:#0ea5e9}
input:checked + .slider:before{transform:translateX(16px)}
details.proxyip-section{margin-top:18px}
summary{cursor:pointer;color:#94a3b8;font-size:0.85rem}
.default-group-editor{margin-top:12px;display:flex;gap:8px;align-items:center;font-size:0.8rem;color:#94a3b8}
.lock-badge{background:#78350f;color:#fbbf24;padding:1px 8px;border-radius:4px;font-size:0.72rem;font-weight:bold;margin-left:8px}
.auto-badge{background:#1e3a8a;color:#93c5fd;padding:1px 8px;border-radius:4px;font-size:0.72rem;font-weight:bold;margin-left:8px}
.pager{display:flex;gap:10px;align-items:center;justify-content:center;margin:12px 0;flex-wrap:wrap}
</style>
</head>
<body>
<div class="container">

<div class="top">
  <h1>SOCKS5 Proxy Pool</h1>
  <a class="gh-link" href="https://github.com/kim1232aa/socks5-pool-pro" target="_blank" rel="noopener"><svg viewBox="0 0 16 16"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z"/></svg>GitHub</a>
</div>

<div class="stats">
  <div class="stat-item">转发节点: <span id="stat-total">{{.Total}}</span></div>
  <div class="stat-item">ProxyIP(仅展示): <span id="stat-proxyip">{{.ProxyIPTotal}}</span></div>
  <div class="stat-item">上次刷新: <span id="stat-last">{{if .LastScrape}}{{.LastScrape}}{{else}}N/A{{end}}</span></div>
  <div class="stat-item">下次刷新: <span id="stat-next">{{if .NextScrape}}{{.NextScrape}}{{else}}N/A{{end}}</span></div>
  <button class="btn" onclick="doRefresh(this)">刷新代理池</button>
</div>

<div class="tabs">
  <a href="#nodes" class="tab-link" data-tab="nodes">节点</a>
  <a href="#sources" class="tab-link" data-tab="sources">来源</a>
  <a href="#rules" class="tab-link" data-tab="rules">分流规则</a>
  <a href="#groups" class="tab-link" data-tab="groups">分组策略</a>
</div>

<div id="tab-nodes" class="tab-panel">
  <div id="current-node-banner" class="current-node">当前使用节点: <span class="cn-addr">加载中...</span></div>

  <div id="group-cards-container" class="group-cards"></div>

  <p class="note">"国家/城市"是<b>真实出口 IP</b> 的定位(穿过节点探测),不是节点自身 IP。"匿名"=高匿(elite,不暴露)/普通(anonymous,可被识别为代理)/透明(transparent,泄露你的真实IP)。"评分"综合成功率/延迟/速度。默认已开启剔除"假代理"(出口IP==本机出口的透明节点),用 -require-ip-change=false 关闭。点节点上的<b>"使用"</b>即把默认(ANY)出口<b>手动锁定</b>到该节点,后台自动轮换会暂停;点上方横幅的<b>"恢复自动轮换"</b>可解锁。<b>节点不会被自动删除</b>:每轮刷新只标记"可用/不可用",不可用的节点默认被"隐藏不可用"过滤掉但仍保留在池中,下次测活成功会自动恢复显示;要彻底删除不可用节点,点"清理不可用"手动确认。</p>

  <div class="filter-bar">
    <input id="f-text" placeholder="搜索 IP/地址..." oninput="onFilterChange()">
    <select id="f-country" onchange="onFilterChange()"><option value="">全部国家</option></select>
    <select id="f-proto" onchange="onFilterChange()"><option value="">全部协议</option><option>socks5</option><option>http</option><option>https</option></select>
    <select id="f-sort" onchange="onFilterChange()">
      <option value="score">按评分↓</option>
      <option value="latency">按延迟↑</option>
      <option value="speed">按速度↓</option>
      <option value="country">按国家</option>
    </select>
    <select id="f-pagesize" onchange="onPageSizeChange()">
      <option value="20">每页20</option>
      <option value="50">每页50</option>
      <option value="100">每页100</option>
      <option value="100000">全部</option>
    </select>
    <label class="chk"><input type="checkbox" id="f-ipchanged" onchange="onFilterChange()"> 只看真正改IP的</label>
    <label class="chk"><input type="checkbox" id="f-hide-unavail" checked onchange="onFilterChange()"> 隐藏不可用</label>
    <button class="btn-sm" onclick="exportNodes('csv')" title="按延迟升序,UTF-8 BOM,Excel 可直接打开">导出CSV</button>
    <button class="btn-sm" onclick="exportNodes('tme')" title="Telegram SOCKS 链接(仅 socks5 节点)">导出t.me</button>
    <button class="btn-sm danger" onclick="clearUnavailable()" title="彻底删除所有标记为不可用的节点(需手动确认,不会自动触发)">清理不可用</button>
    <span id="node-count" class="small"></span>
  </div>

  <div class="table-scroll">
  <table>
  <thead><tr><th></th><th>协议</th><th>地址(节点IP)</th><th>出口IP</th><th>匿名</th><th>国家/城市</th><th>评分</th><th>延迟</th><th>速度</th><th>来源</th><th>操作</th></tr></thead>
  <tbody id="node-tbody"><tr><td colspan="11" class="empty">加载中...</td></tr></tbody>
  </table>
  </div>
  <div class="pager" id="node-pager"></div>

  <details class="proxyip-section">
    <summary>ProxyIP 节点(仅展示,不参与本地转发) - {{.ProxyIPTotal}} 个</summary>
    <p class="note">这些是 Cloudflare 边缘优选 IP,常用于 Worker/VLESS/Trojan 隧道脚本的反代地址,不支持通用 SOCKS5/HTTP 代理协议,因此不会被本地 SOCKS5 服务转发使用,仅供查看和导出参考。</p>
    {{if .ProxyIPs}}
    <table>
    <tr><th>地址</th><th>国家/城市</th><th>来源</th></tr>
    {{range .ProxyIPs}}
    <tr><td class="mono">{{.Addr}}</td><td>{{.Country}}{{if .City}}, {{.City}}{{end}}</td><td class="small">{{.Source}}</td></tr>
    {{end}}
    </table>
    {{else}}
    <p class="note">来源未启用或暂无数据。</p>
    {{end}}
  </details>
</div>

<div id="tab-sources" class="tab-panel" style="display:none">
  <table>
  <tr><th>名称</th><th>URL</th><th>格式</th><th>类型</th><th>启用</th><th>操作</th></tr>
  {{range .Sources}}
  <tr>
    <td>{{.Name}}{{if .Note}}<div class="note-inline">{{.Note}}</div>{{end}}</td>
    <td class="mono small">{{.URL}}</td>
    <td class="small">{{.Format}}{{if .Protocol}} ({{.Protocol}}){{end}}</td>
    <td class="small">{{if .Builtin}}内置{{else}}自定义{{end}}</td>
    <td>
      <label class="switch">
        <input type="checkbox" {{if .Enabled}}checked{{end}} onchange="postJSON('/api/sources/toggle',{id:'{{.ID}}',enabled:this.checked},reloadOrAlert)">
        <span class="slider"></span>
      </label>
    </td>
    <td><button class="btn-sm danger" onclick="if(confirm('删除来源 {{.Name}}?'))postJSON('/api/sources/delete',{id:'{{.ID}}'},reloadOrAlert)">删除</button></td>
  </tr>
  {{end}}
  </table>

  <form class="inline" id="form-add-source">
    <input name="name" placeholder="名称" required>
    <input name="url" placeholder="URL" required style="min-width:280px">
    <select name="format">{{range .Formats}}<option value="{{.}}">{{.}}</option>{{end}}</select>
    <input name="protocol" placeholder="协议(仅纯文本/JSON数组格式需要,如 socks5)">
    <button class="btn" type="submit">添加来源</button>
  </form>
  <p class="note">格式说明: text-regex = 文本中扫描 "scheme://ip:port"; edt-json = EDT-Pages 风格 JSON 数组; proxyip-json = Cloudflare ProxyIP 专用格式; plain-list = 每行一个 "ip:port"(需填协议); json-array = JSON 字符串数组,每项 "ip:port"(需填协议)。</p>
</div>

<div id="tab-rules" class="tab-panel" style="display:none">
  <div class="preset-bar">
    <b style="color:#e2e8f0">一键 GFW 分流</b>
    <span>国内域名/内网 直连(DIRECT),其余走代理(ANY);会覆盖当前规则。</span>
    <button class="btn" onclick="if(confirm('用 GFW 分流预设覆盖当前所有规则?'))postJSON('/api/rules/preset-gfw',{},reloadOrAlert)">启用 GFW 分流</button>
  </div>
  <table>
  <tr><th>#</th><th>类型</th><th>值</th><th>目标分组</th><th>操作</th></tr>
  {{range $i, $r := .Rules}}
  <tr>
    <td>{{$i}}</td>
    <td>{{$r.Type}}</td>
    <td class="mono">{{if eq $r.Type "MATCH"}}*{{else}}{{$r.Value}}{{end}}</td>
    <td>{{$r.Group}}</td>
    <td>
      {{if ne $r.Type "MATCH"}}
      <button class="btn-sm" onclick="postJSON('/api/rules/move',{id:'{{$r.ID}}',delta:-1},reloadOrAlert)">↑</button>
      <button class="btn-sm" onclick="postJSON('/api/rules/move',{id:'{{$r.ID}}',delta:1},reloadOrAlert)">↓</button>
      <button class="btn-sm danger" onclick="if(confirm('删除规则?'))postJSON('/api/rules/delete',{id:'{{$r.ID}}'},reloadOrAlert)">删除</button>
      {{else}}
      <span class="note-inline">兜底规则,不可删除/移动,可在下方修改默认分组</span>
      {{end}}
    </td>
  </tr>
  {{end}}
  </table>

  <form class="inline" id="form-add-rule">
    <select name="type">{{range .RuleTypes}}<option value="{{.}}">{{.}}</option>{{end}}</select>
    <input name="value" placeholder="值,如 netflix.com / 10.0.0.0/8 / cn / gfw">
    <select name="group" id="rule-target-select">{{range .GroupOptions}}<option value="{{.}}">{{.}}</option>{{end}}</select>
    <button class="btn" type="submit">添加规则</button>
  </form>
  <p class="note">规则按从上到下的顺序匹配目标域名/IP,命中即用对应目标转发;DOMAIN/DOMAIN-SUFFIX/DOMAIN-KEYWORD 匹配域名,IP-CIDR 匹配字面 IP 目标,GEOSITE 值填 <b>cn</b>(内置中国常用域名)或 <b>gfw</b>(内置常见被墙域名)。<b>目标可直接选国家</b>(列表里的 <span class="mono">COUNTRY:US</span> / <span class="mono">COUNTRY:JP</span> 等,表示"该国任意节点,自动挑最快的一个"),无需先建分组。例如:<span class="mono">DOMAIN-SUFFIX com → COUNTRY:US</span>,再把 <span class="mono">DOMAIN 111.com → COUNTRY:JP</span> 拖到它上面(越靠上越优先),就能实现"*.com 走美国、111.com 走日本"。若某国当前无可用节点,会自动回退到 ANY。最下面的 MATCH 是兜底规则,始终存在。</p>

  <div class="default-group-editor">
    <span>默认(兜底)分组:</span>
    <select id="default-group-select" data-default="{{.DefaultGroup}}">{{range .GroupOptions}}<option value="{{.}}" {{if eq . $.DefaultGroup}}selected{{end}}>{{.}}</option>{{end}}</select>
    <button class="btn-sm" onclick="postJSON('/api/rules/default',{group:document.getElementById('default-group-select').value},reloadOrAlert)">保存</button>
  </div>
</div>

<div id="tab-groups" class="tab-panel" style="display:none">
  <table>
  <tr><th>名称</th><th>类型</th><th>策略</th><th>过滤条件</th><th>成员数/当前</th><th>操作</th></tr>
  {{range .Groups}}
  {{if .ID}}
  <tr>
    <td>{{.Name}}</td>
    <td class="small">自定义</td>
    <td>
      <select onchange="postJSON('/api/groups/strategy',{id:'{{.ID}}',strategy:this.value},reloadOrAlert)">
        {{$cur := .Strategy}}
        {{range $.Strategies}}<option value="{{.}}" {{if eq . $cur}}selected{{end}}>{{.}}</option>{{end}}
      </select>
    </td>
    <td class="small">{{if .Nodes}}指定节点: {{range .Nodes}}{{.}} {{end}}<br>{{end}}{{if .Countries}}国家: {{range .Countries}}{{.}} {{end}}<br>{{end}}{{if .Protocols}}协议: {{range .Protocols}}{{.}} {{end}}<br>{{end}}{{if .Sources}}来源: {{range .Sources}}{{.}} {{end}}{{end}}</td>
    <td>{{.Count}} / {{if .Current}}{{.Current}}{{if .Dynamic}} (每连接轮换){{end}}{{else}}-{{end}}</td>
    <td><button class="btn-sm danger" onclick="if(confirm('删除分组 {{.Name}}? 引用它的规则会自动回退到 ANY'))postJSON('/api/groups/delete',{id:'{{.ID}}'},reloadOrAlert)">删除</button></td>
  </tr>
  {{end}}
  {{end}}
  </table>

  <form class="inline" id="form-add-group">
    <input name="name" placeholder="分组名称" required>
    <select name="strategy">{{range .Strategies}}<option value="{{.}}">{{.}}</option>{{end}}</select>
    <input name="nodes" placeholder="指定节点 ip:port,逗号分隔 (钉死到具体节点时用)">
    <input name="countries" placeholder="国家代码,逗号分隔,如 US,JP (留空=不限)">
    <input name="protocols" placeholder="协议,逗号分隔,如 socks5,http (留空=不限)">
    <input name="sources" placeholder="来源名称,逗号分隔 (留空=不限)">
    <button class="btn" type="submit">创建分组</button>
  </form>
  <p class="note">分组是从代理池里筛出的节点子集,配合分流规则使用。<b>要把某个域名固定走某一个节点</b>:在"指定节点"里填那个节点的 ip:port(可在节点标签页复制),建一个分组,再在分流规则里把该域名指向这个分组即可。筛选条件可组合(指定节点 / 国家 / 协议 / 来源)。策略: sticky=固定直到手动切换或失败, round-robin=每次新连接轮换, random=随机, latency=优先延迟最低, speed=优先测速结果最高(需先手动测速)。</p>
</div>

</div>
<script>
function postJSON(url, body, cb) {
  fetch(url, {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify(body)})
    .then(function(r){ return r.json().catch(function(){return {};}).then(function(j){ return {ok:r.ok, json:j}; }); })
    .then(function(res){ cb(res.ok ? null : ((res.json && res.json.error) || '请求失败')); })
    .catch(function(err){ cb(String(err)); });
}
function reloadOrAlert(err) { if (err) { alert(err); } else { location.reload(); } }

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

var allNodes = [];
var nodePage = 1;
var nodePageSize = 20;
var anyPinned = false;

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

// populateCountrySelect rebuilds #f-country grouped by continent
// (<optgroup>), each continent's countries sorted by live node count
// descending - mirrors EDT-Pages' "获取更多" region picker (continent
// group -> country, count in parens), but driven by our own real
// exit-IP-probed geo data rather than a trusted source feed.
function populateCountrySelect() {
  var sel = document.getElementById('f-country');
  if (!sel) return;
  var cur = sel.value;

  var counts = {}, continentOf = {};
  allNodes.forEach(function(n) {
    if (!n.country) return;
    counts[n.country] = (counts[n.country] || 0) + 1;
    if (!continentOf[n.country]) continentOf[n.country] = n.continent || '';
  });

  var byContinent = {};
  Object.keys(counts).forEach(function(c) {
    // Prefer the continent our own exit-IP probe actually measured;
    // fall back to the static table for nodes whose Country came straight
    // from a source feed (never went through LookupGeo).
    var cont = continentOf[c] || countryToContinent[c.toUpperCase()] || '';
    if (!byContinent[cont]) byContinent[cont] = [];
    byContinent[cont].push(c);
  });
  Object.keys(byContinent).forEach(function(cont) {
    byContinent[cont].sort(function(a, b) { return counts[b] - counts[a]; });
  });

  var opts = '<option value="">全部国家</option>';
  Object.keys(byContinent).sort(function(a, b) {
    return continentOrder.indexOf(a) - continentOrder.indexOf(b);
  }).forEach(function(cont) {
    var info = continentInfo[cont];
    var label = info ? (info.emoji + ' ' + info.name + ' / ' + cont) : '🏳️ 未知地区';
    opts += '<optgroup label="' + escapeHtml(label) + '">';
    byContinent[cont].forEach(function(c) {
      opts += '<option value="' + escapeHtml(c) + '">' + flagEmoji(c) + ' ' + escapeHtml(c) + '(' + counts[c] + ')</option>';
    });
    opts += '</optgroup>';
  });
  sel.innerHTML = opts;
  sel.value = cur;
}

function onNodesFetched(nodes) {
  allNodes = nodes || [];
  populateCountrySelect();
  populateRuleTargets();
  applyNodeView();
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
  allNodes.forEach(function(n){ if (n.country) countries[n.country] = true; });
  Object.keys(countries).sort().forEach(function(c){
    var o = document.createElement('option');
    o.value = 'COUNTRY:' + c;
    o.textContent = 'COUNTRY:' + c + '（该国任意节点）';
    o.setAttribute('data-country', '1');
    sel.appendChild(o);
  });
  var want = sel.getAttribute('data-default') || cur;
  if (want) { sel.value = want; }
}

function populateRuleTargets() {
  addCountryOptionsTo(document.getElementById('rule-target-select'));
  addCountryOptionsTo(document.getElementById('default-group-select'));
}

function onFilterChange() { nodePage = 1; applyNodeView(); }
function onPageSizeChange() {
  nodePageSize = parseInt(document.getElementById('f-pagesize').value, 10) || 20;
  nodePage = 1;
  applyNodeView();
}
function gotoPage(p) { nodePage = p; applyNodeView(); }
function setAuto() {
  postJSON('/api/nodes/auto', {}, function(err){ if (err) { alert(err); } else { pollStatus(); } });
}

function clearUnavailable() {
  if (!confirm('彻底删除所有标记为"不可用"的节点?这个操作不可撤销(可用节点不受影响)。')) return;
  fetch('/api/nodes/clear-unavailable', {method:'POST', headers:{'Content-Type':'application/json'}, body:'{}'})
    .then(function(r){ return r.json(); })
    .then(function(j){ alert('已清理 ' + (j.removed||0) + ' 个不可用节点'); pollStatus(); })
    .catch(function(err){ alert(String(err)); });
}

function applyNodeView() {
  var tbody = document.getElementById('node-tbody');
  if (!tbody) return;
  var banner = document.querySelector('#current-node-banner .cn-addr');
  var countEl = document.getElementById('node-count');

  var active = null;
  allNodes.forEach(function(n){ if (n.active) active = n; });

  var text = (document.getElementById('f-text').value || '').toLowerCase();
  var fc = document.getElementById('f-country').value;
  var fp = document.getElementById('f-proto').value;
  var onlyChanged = document.getElementById('f-ipchanged').checked;
  var hideUnavail = document.getElementById('f-hide-unavail').checked;
  var sort = document.getElementById('f-sort').value;

  var unavailCount = 0;
  allNodes.forEach(function(n){ if (n.available === false) unavailCount++; });

  var rows = allNodes.filter(function(n) {
    if (text && (n.addr + ' ' + (n.exit_ip||'')).toLowerCase().indexOf(text) < 0) return false;
    if (fc && n.country !== fc) return false;
    if (fp && n.protocol !== fp) return false;
    if (onlyChanged && !n.ip_changed) return false;
    if (hideUnavail && n.available === false) return false;
    return true;
  });

  rows.sort(function(a, b) {
    switch (sort) {
      case 'latency': return (a.latency_ms||1e9) - (b.latency_ms||1e9);
      case 'speed': return (b.speed_kbps||0) - (a.speed_kbps||0);
      case 'country': return (a.country||'').localeCompare(b.country||'');
      default: return (b.score||0) - (a.score||0);
    }
  });

  var pager = document.getElementById('node-pager');
  var total = rows.length;
  var pageCount = Math.max(1, Math.ceil(total / nodePageSize));
  if (nodePage > pageCount) nodePage = pageCount;
  if (nodePage < 1) nodePage = 1;
  var startIdx = (nodePage - 1) * nodePageSize;
  var pageRows = rows.slice(startIdx, startIdx + nodePageSize);

  if (countEl) {
    var suffix = unavailCount ? (' (另有 ' + unavailCount + ' 个不可用' + (hideUnavail ? '已隐藏' : '') + ')') : '';
    countEl.textContent = (total
      ? ('显示 ' + (startIdx + 1) + '-' + (startIdx + pageRows.length) + ' / 匹配 ' + total + ' / 共 ' + allNodes.length)
      : ('匹配 0 / 共 ' + allNodes.length)) + suffix;
  }

  if (!allNodes.length) {
    tbody.innerHTML = '<tr><td colspan="11" class="empty">暂无可用节点,等待下次抓取周期...</td></tr>';
    if (pager) pager.innerHTML = '';
    if (banner) banner.textContent = '无 (代理池为空)';
    return;
  }
  if (!rows.length) {
    tbody.innerHTML = '<tr><td colspan="11" class="empty">没有匹配的节点</td></tr>';
    if (pager) pager.innerHTML = '';
  } else {
    var html = '';
    pageRows.forEach(function(n) {
      var loc = (n.country ? flagEmoji(n.country) + ' ' : '') + escapeHtml(n.country || '') + (n.city ? ', ' + escapeHtml(n.city) : '');
      var lat = n.latency_ms ? n.latency_ms + 'ms' : '-';
      var spd = n.speed_kbps ? Math.round(n.speed_kbps) + ' kbps' : '-';
      var nodeIP = (n.addr || '').split(':')[0];
      var exit = n.exit_ip || '';
      var exitCell = exit
        ? '<span class="mono' + (exit !== nodeIP ? ' exit-diff' : '') + '">' + escapeHtml(exit) + '</span>'
        : '<span class="small">-</span>';
      html += '<tr class="' + (n.active ? 'active' : '') + (n.available === false ? ' unavail' : '') + '" data-key="' + escapeHtml(n.key) + '">' +
        '<td>' + (n.active ? '<span class="badge-inuse">使用中</span>' : (n.available === false ? '<span class="badge-unavail">不可用</span>' : '')) + '</td>' +
        '<td>' + protoBadge(n.protocol) + '</td>' +
        '<td class="mono">' + escapeHtml(n.addr) + '<span class="copy-btn" onclick="copyAddr(\'' + escapeHtml(n.addr) + '\',this)">复制</span></td>' +
        '<td>' + exitCell + '</td>' +
        '<td>' + anonBadge(n.anonymity) + '</td>' +
        '<td>' + loc + '</td>' +
        '<td>' + scoreCell(n.score) + '</td>' +
        '<td>' + lat + '</td>' +
        '<td class="speed-cell">' + spd + '</td>' +
        '<td class="small">' + escapeHtml(n.source || '') + '</td>' +
        '<td>' +
          '<button class="btn-sm" onclick="switchNode(this)">使用</button>' +
          '<button class="btn-sm" onclick="runSpeedtest(this)">测速</button>' +
          '<button class="btn-sm" onclick="runVerify(this)" title="立即重新拨号,查看真实出口IP/国家是否和标签一致">验证</button>' +
        '</td></tr>';
    });
    tbody.innerHTML = html;
    if (pager) {
      if (total <= nodePageSize) {
        pager.innerHTML = '';
      } else {
        pager.innerHTML =
          '<button class="btn-sm" ' + (nodePage <= 1 ? 'disabled' : '') + ' onclick="gotoPage(' + (nodePage - 1) + ')">上一页</button>' +
          '<span class="small">第 ' + nodePage + ' / ' + pageCount + ' 页</span>' +
          '<button class="btn-sm" ' + (nodePage >= pageCount ? 'disabled' : '') + ' onclick="gotoPage(' + (nodePage + 1) + ')">下一页</button>';
      }
    }
  }

  if (banner) {
    var lockUI = anyPinned
      ? '<span class="lock-badge">🔒 手动锁定</span><button class="btn-sm" onclick="setAuto()">恢复自动轮换</button>'
      : '<span class="auto-badge">🔄 自动轮换中</span>';
    var body = active
      ? escapeHtml(active.addr) + '<span class="cn-meta">' + protoBadge(active.protocol) + ' 出口 ' + escapeHtml(active.exit_ip || '?') + ' ' + escapeHtml(active.country || '') + '</span>'
      : (allNodes.length ? escapeHtml(allNodes[0].addr) + '<span class="cn-meta">(默认选择)</span>' : '无');
    banner.innerHTML = body + lockUI;
  }
}

function copyAddr(addr, el) {
  if (navigator.clipboard) { navigator.clipboard.writeText(addr); }
  if (el) { var t = el.textContent; el.textContent = '已复制'; setTimeout(function(){ el.textContent = t; }, 1000); }
}

function exportNodes(fmt) {
  var q = 'format=' + fmt;
  var c = document.getElementById('f-country').value; if (c) q += '&country=' + encodeURIComponent(c);
  var p = document.getElementById('f-proto').value; if (p) q += '&protocol=' + encodeURIComponent(p);
  if (document.getElementById('f-ipchanged').checked) q += '&only_changed=1';
  var a = document.createElement('a');
  a.href = '/api/nodes/export?' + q;
  document.body.appendChild(a); a.click(); a.remove();
}

function rowKey(btn) { var tr = btn.closest('tr'); return tr ? tr.getAttribute('data-key') : ''; }

function switchNode(btn) {
  postJSON('/api/nodes/switch', {key: rowKey(btn)}, function(err) {
    if (err) { alert(err); } else { pollStatus(); }
  });
}

function pollStatus() {
  fetch('/api/status').then(function(r){ return r.json(); }).then(function(d) {
    var elTotal = document.getElementById('stat-total');
    var elProxyip = document.getElementById('stat-proxyip');
    var elLast = document.getElementById('stat-last');
    var elNext = document.getElementById('stat-next');
    if (elTotal) elTotal.textContent = d.total;
    if (elProxyip) elProxyip.textContent = d.proxyip_total;
    if (elLast) elLast.textContent = d.last_scrape || 'N/A';
    if (elNext) elNext.textContent = d.next_scrape || 'N/A';
    var groups = d.groups || [];
    anyPinned = false;
    groups.forEach(function(g){ if (g.name === 'ANY') anyPinned = !!g.pinned; });
    renderGroups(groups);
    if (allNodes.length) applyNodeView(); // refresh banner lock state
  }).catch(function(){});
  fetch('/api/nodes').then(function(r){ return r.json(); }).then(function(nodes) {
    onNodesFetched(nodes || []);
  }).catch(function(){});
}
pollStatus();
setInterval(pollStatus, 15000);

function doRefresh(btn) {
  btn.disabled = true;
  var orig = btn.textContent;
  btn.textContent = '刷新中...';
  fetch('/api/refresh').then(function(){
    setTimeout(function(){ location.reload(); }, 15000);
  }).catch(function(){ btn.disabled = false; btn.textContent = orig; });
}

function runSpeedtest(btn) {
  var key = rowKey(btn);
  btn.disabled = true;
  var orig = btn.textContent;
  btn.textContent = '测速中...';
  fetch('/api/nodes/speedtest', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({key:key})})
    .then(function(r){ return r.json(); })
    .then(function(j) {
      btn.disabled = false;
      btn.textContent = orig;
      if (j.error) { alert('测速失败: ' + j.error); return; }
      var row = btn.closest('tr');
      var cell = row ? row.querySelector('.speed-cell') : null;
      if (cell) cell.textContent = Math.round(j.kbps) + ' kbps';
    })
    .catch(function(err) { btn.disabled = false; btn.textContent = orig; alert(String(err)); });
}

function runVerify(btn) {
  var key = rowKey(btn);
  btn.disabled = true;
  var orig = btn.textContent;
  btn.textContent = '验证中...';
  fetch('/api/nodes/verify', {method:'POST', headers:{'Content-Type':'application/json'}, body: JSON.stringify({key:key})})
    .then(function(r){ return r.json(); })
    .then(function(j) {
      btn.disabled = false;
      btn.textContent = orig;
      if (!j.reachable) { alert('验证失败:节点当前无法连通(可能已失效)'); pollStatus(); return; }
      var msg = '真实出口IP: ' + (j.exit_ip || '未知') + '\n国家: ' + (j.country || '未知');
      if (!j.label_matched) {
        msg += '\n\n⚠️ 与列表标签不符(之前记录: ' + (j.prev_country || '未知') + ' / ' + (j.prev_exit_ip || '未知') + ')\n已用最新结果刷新该节点标签。';
      } else {
        msg += '\n\n✅ 与列表标签一致。';
      }
      alert(msg);
      pollStatus();
    })
    .catch(function(err) { btn.disabled = false; btn.textContent = orig; alert(String(err)); });
}

function showTab(name) {
  var panels = document.querySelectorAll('.tab-panel');
  for (var i = 0; i < panels.length; i++) panels[i].style.display = 'none';
  var target = document.getElementById('tab-' + name);
  if (target) target.style.display = '';
  var links = document.querySelectorAll('.tab-link');
  for (var i = 0; i < links.length; i++) links[i].classList.toggle('active', links[i].dataset.tab === name);
}
var tabLinks = document.querySelectorAll('.tab-link');
for (var i = 0; i < tabLinks.length; i++) {
  tabLinks[i].addEventListener('click', function(e) {
    e.preventDefault();
    var name = e.currentTarget.dataset.tab;
    location.hash = name;
    showTab(name);
  });
}
showTab((location.hash || '#nodes').slice(1));

document.getElementById('form-add-source').addEventListener('submit', function(e) {
  e.preventDefault();
  var f = e.target;
  postJSON('/api/sources', {
    name: f.name.value, url: f.url.value, format: f.format.value, protocol: f.protocol.value
  }, function(err) { if (err) { alert(err); } else { location.hash = 'sources'; location.reload(); } });
});

document.getElementById('form-add-rule').addEventListener('submit', function(e) {
  e.preventDefault();
  var f = e.target;
  postJSON('/api/rules', {
    type: f.type.value, value: f.value.value, group: f.group.value
  }, function(err) { if (err) { alert(err); } else { location.hash = 'rules'; location.reload(); } });
});

document.getElementById('form-add-group').addEventListener('submit', function(e) {
  e.preventDefault();
  var f = e.target;
  function splitList(v) { return v.split(',').map(function(s){ return s.trim(); }).filter(Boolean); }
  postJSON('/api/groups', {
    name: f.name.value, strategy: f.strategy.value, nodes: splitList(f.nodes.value),
    countries: splitList(f.countries.value), protocols: splitList(f.protocols.value), sources: splitList(f.sources.value)
  }, function(err) { if (err) { alert(err); } else { location.hash = 'groups'; location.reload(); } });
});
</script>
</body>
</html>`
