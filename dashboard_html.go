package main

const dashboardHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>Proxy Atlas · 代理节点控制台</title>
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<meta name="theme-color" content="#111827">
<style>
:root{--canvas:#f4f6fb;--surface:#fff;--surface-soft:#f8f9fc;--ink:#172033;--muted:#697386;--line:#e3e7ef;--brand:#6558f5;--brand-deep:#4d40df;--cyan:#07a9c2;--green:#0f9f73;--amber:#dc8618;--red:#d64d62;--nav:#111827;--shadow:0 12px 32px rgba(27,34,52,.08);--radius:16px}
*{margin:0;padding:0;box-sizing:border-box}
html{scroll-behavior:smooth}
body{font-family:Inter,"Noto Sans SC","PingFang SC",system-ui,-apple-system,sans-serif;background:var(--canvas);color:var(--ink);min-width:320px;line-height:1.45}
button,input,select{font:inherit}
button:focus-visible,input:focus-visible,select:focus-visible,a:focus-visible,summary:focus-visible{outline:3px solid rgba(101,88,245,.34);outline-offset:2px}
.sr-only{position:absolute;width:1px;height:1px;padding:0;margin:-1px;overflow:hidden;clip:rect(0,0,0,0);white-space:nowrap;border:0}
.skip-link{position:fixed;left:16px;top:-80px;z-index:3000;background:#fff;color:var(--brand-deep);padding:10px 14px;border-radius:10px;box-shadow:var(--shadow);font-weight:700}.skip-link:focus{top:16px}
.app-shell{display:grid;grid-template-columns:252px minmax(0,1fr);min-height:100vh}
.sidebar{position:sticky;top:0;height:100vh;padding:26px 18px 20px;background:var(--nav);color:#d8deea;display:flex;flex-direction:column;overflow:hidden;z-index:100}
.sidebar:before,.sidebar:after{content:"";position:absolute;border-radius:50%;filter:blur(1px);pointer-events:none}.sidebar:before{width:230px;height:230px;left:-110px;top:-120px;background:rgba(101,88,245,.3)}.sidebar:after{width:190px;height:190px;right:-125px;bottom:10%;background:rgba(7,169,194,.14)}
.brand{position:relative;display:flex;align-items:center;gap:12px;padding:0 9px 25px;border-bottom:1px solid rgba(255,255,255,.08)}
.brand-mark{width:42px;height:42px;display:grid;place-items:center;border-radius:13px;background:linear-gradient(145deg,#7b70ff,#5041da);box-shadow:0 10px 26px rgba(101,88,245,.35);font-weight:900;color:#fff;letter-spacing:-1px}
.brand-copy strong{display:block;color:#fff;font-size:.98rem;letter-spacing:.01em}.brand-copy span{font-size:.7rem;color:#8f9aaf;letter-spacing:.12em;text-transform:uppercase}
.nav-label{position:relative;margin:24px 11px 9px;color:#68758b;font-size:.66rem;font-weight:800;letter-spacing:.13em;text-transform:uppercase}
.tabs{position:relative;display:grid;gap:6px}
.tab-link{display:flex;align-items:center;gap:12px;min-height:45px;padding:10px 12px;border-radius:11px;color:#929db0;text-decoration:none;font-size:.84rem;font-weight:650;transition:background .16s,color .16s,transform .16s}
.tab-link:before{content:attr(data-icon);width:25px;text-align:center;font-size:1.05rem;filter:grayscale(.35)}
.tab-link:hover{color:#fff;background:rgba(255,255,255,.06);transform:translateX(2px)}
.tab-link.active{color:#fff;background:linear-gradient(100deg,rgba(101,88,245,.95),rgba(101,88,245,.7));box-shadow:0 8px 20px rgba(0,0,0,.22)}
.tab-link.active:after{content:"";width:6px;height:6px;border-radius:50%;margin-left:auto;background:#bdf4e5;box-shadow:0 0 0 4px rgba(189,244,229,.1)}
.sidebar-foot{position:relative;margin-top:auto;padding:16px 10px 2px;border-top:1px solid rgba(255,255,255,.08)}
.system-online{display:flex;align-items:center;gap:8px;color:#a9b3c3;font-size:.74rem}.system-online:before{content:"";width:8px;height:8px;border-radius:50%;background:#26d99a;box-shadow:0 0 0 4px rgba(38,217,154,.1)}
.gh-link{color:#7f8ba0;text-decoration:none;display:inline-flex;align-items:center;gap:7px;font-size:.72rem;margin-top:10px}.gh-link:hover{color:#fff}.gh-link svg{width:16px;height:16px;fill:currentColor}
.main-shell{min-width:0;padding:0 32px 42px}
.container{width:min(100%,1540px);margin:0 auto}
.topbar{display:flex;align-items:center;justify-content:space-between;gap:22px;padding:27px 0 21px}
.eyebrow{display:block;color:var(--brand);font-size:.67rem;font-weight:850;letter-spacing:.14em;text-transform:uppercase;margin-bottom:4px}
h1{font-size:clamp(1.45rem,2.4vw,2rem);line-height:1.2;letter-spacing:-.035em;color:var(--ink)}
.page-description{margin-top:5px;color:var(--muted);font-size:.82rem}
.topbar-actions{display:flex;align-items:center;justify-content:flex-end;gap:12px;flex-wrap:wrap}
.refresh-status{max-width:330px;text-align:right;min-height:1em}
.btn,.btn-sm{border:0;cursor:pointer;border-radius:10px;font-weight:750;transition:transform .12s,box-shadow .12s,background .12s;color:var(--ink)}
.btn{min-height:42px;padding:9px 17px;background:linear-gradient(135deg,var(--brand),var(--brand-deep));color:#fff;box-shadow:0 8px 18px rgba(101,88,245,.22);font-size:.8rem}
.btn:hover{transform:translateY(-1px);box-shadow:0 11px 25px rgba(101,88,245,.3)}
.btn:disabled,.btn-sm:disabled{background:#e6e9ef;color:#9aa3b2;box-shadow:none;cursor:not-allowed;transform:none}
.btn-sm{min-height:34px;padding:6px 11px;background:#eef0f7;color:#49546a;font-size:.72rem;margin-right:4px}
.btn-sm:hover{background:#e4e6f4;color:var(--brand-deep)}
.btn-sm.danger{background:#fff0f2;color:#bf3c52}.btn-sm.danger:hover{background:#ffe1e6}
.stats{display:grid;grid-template-columns:repeat(7,minmax(0,1fr));gap:11px;margin:0 0 14px}
.stat-item{position:relative;min-height:98px;padding:17px;background:var(--surface);border:1px solid var(--line);border-radius:var(--radius);color:var(--muted);font-size:.72rem;overflow:hidden;box-shadow:0 4px 16px rgba(27,34,52,.025)}
.stat-item:after{content:"";position:absolute;width:42px;height:42px;border-radius:50%;right:-18px;top:-18px;background:rgba(101,88,245,.09)}
.stat-item span{display:block;color:var(--ink);font:750 1.32rem/1.2 ui-monospace,SFMono-Regular,Menlo,monospace;margin-top:13px;overflow:hidden;text-overflow:ellipsis}
.stat-item:nth-child(2){border-color:#cbeee2}.stat-item:nth-child(2):after{background:rgba(15,159,115,.14)}.stat-item:nth-child(2) span{color:var(--green)}
.stat-item:nth-child(3){border-color:#f4d8dd}.stat-item:nth-child(3):after{background:rgba(214,77,98,.12)}.stat-item:nth-child(3) span{color:var(--red)}
.stat-item:nth-child(5){border-color:#e3dafb}.stat-item:nth-child(5) span{color:var(--brand)}
.stat-item:nth-child(6) span,.stat-item:nth-child(7) span{font-size:.76rem;line-height:1.45;white-space:normal}
.health-card{display:grid;grid-template-columns:minmax(0,1.35fr) minmax(280px,.65fr);gap:14px;margin-bottom:22px}
.checkurl-bar,.refresh-timeline{background:var(--surface);border:1px solid var(--line);border-radius:var(--radius);padding:17px 18px;box-shadow:0 4px 16px rgba(27,34,52,.025)}
.checkurl-bar{display:grid;grid-template-columns:auto minmax(160px,1fr) auto;align-items:center;gap:10px}
.checkurl-label{font-size:.75rem;color:var(--ink);font-weight:800;white-space:nowrap}.checkurl-hint{grid-column:1/-1;color:var(--muted);font-size:.7rem;line-height:1.55}
.refresh-timeline{display:grid;grid-template-columns:1fr 1fr;gap:13px}.timeline-item{font-size:.69rem;color:var(--muted)}.timeline-item strong{display:block;margin-top:5px;color:var(--ink);font-size:.78rem;font-weight:700}
input,select,.country-picker-trigger{min-height:38px;background:#fbfcfe;border:1px solid #dce1e9;color:var(--ink);padding:7px 10px;border-radius:10px;font-size:.76rem;transition:border .15s,box-shadow .15s}
input:hover,select:hover,.country-picker-trigger:hover{border-color:#b8c0ce}input:focus,select:focus{border-color:var(--brand);box-shadow:0 0 0 3px rgba(101,88,245,.1)}input::placeholder{color:#a1a9b7}
.content-stage{min-width:0}.tab-panel{padding:0;animation:panel-in .22s ease-out}@keyframes panel-in{from{opacity:.35;transform:translateY(4px)}to{opacity:1;transform:none}}
.panel-heading{display:flex;justify-content:space-between;align-items:flex-end;gap:18px;margin:5px 0 14px}.panel-heading h2{font-size:1.13rem;letter-spacing:-.02em}.panel-heading p{color:var(--muted);font-size:.75rem;margin-top:4px}.panel-kicker{color:var(--brand);font-size:.65rem;font-weight:850;letter-spacing:.12em;text-transform:uppercase}
.scope-intro{display:flex;gap:12px;align-items:flex-start;background:#f0efff;border:1px solid #dedafc;border-radius:13px;padding:13px 15px;margin:0 0 14px;color:#5e6475;font-size:.75rem;line-height:1.55}
.scope-intro strong{display:block;color:#343050;font-size:.84rem;margin-bottom:2px}.scope-icon{font-size:1.25rem;line-height:1.2;flex:0 0 auto}.scope-tag{display:inline-block;padding:2px 7px;border-radius:999px;background:#dedafb;color:#5145c9;font-size:.63rem;font-weight:800;margin-left:5px;vertical-align:1px}
.current-node{position:relative;background:linear-gradient(120deg,#14233e,#183654);border:1px solid rgba(255,255,255,.08);border-radius:var(--radius);padding:19px 21px;margin:0 0 13px;font-size:.8rem;color:#96a9c0;box-shadow:0 11px 28px rgba(20,35,62,.16);overflow:hidden}.current-node:after{content:"";position:absolute;width:170px;height:170px;right:-70px;top:-80px;border-radius:50%;background:rgba(38,217,154,.08)}
.current-node:before{content:"当前出口";display:block;color:#71869f;font-size:.62rem;font-weight:800;letter-spacing:.14em;text-transform:uppercase;margin-bottom:8px}.current-node .cn-addr{color:#fff;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-weight:780;font-size:1rem}.current-node .cn-meta{color:#9caec1;font-size:.72rem;margin-left:8px}
.lock-badge,.auto-badge{display:inline-block;padding:3px 8px;border-radius:999px;font-size:.65rem;font-weight:800;margin-left:10px}.lock-badge{background:rgba(242,182,74,.16);color:#ffd484}.auto-badge{background:rgba(38,217,154,.15);color:#83eac6}
.group-cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(175px,1fr));gap:10px;margin-bottom:14px}.group-card{background:var(--surface);border:1px solid var(--line);border-radius:13px;padding:13px 15px;min-width:0}.group-card.direct{background:#f8fafc}.gc-name{font-weight:800;color:var(--brand-deep);font-size:.8rem}.gc-strategy{color:var(--muted);font-size:.68rem;margin-top:2px}.gc-count{font-size:.72rem;margin-top:8px}.gc-current{font-size:.66rem;color:var(--green);font-family:ui-monospace,SFMono-Regular,Menlo,monospace;word-break:break-all;margin-top:3px}
.node-summary{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:10px;margin:13px 0}.node-summary-item{background:var(--surface);border:1px solid var(--line);border-radius:13px;padding:13px 15px;color:var(--muted);font-size:.68rem}.node-summary-item strong{display:block;color:var(--ink);font:760 1.02rem ui-monospace,SFMono-Regular,Menlo,monospace;margin-top:5px}.candidate-summary{grid-template-columns:repeat(5,minmax(0,1fr))}
.scrape-flow{display:flex;align-items:center;gap:9px;flex-wrap:wrap;background:#fffaf1;border:1px solid #f2e3c8;border-radius:13px;padding:12px 15px;margin:0 0 13px;color:#8a6b39;font-size:.69rem}.scrape-step{white-space:nowrap}.scrape-step strong{color:#5c4626;font-family:ui-monospace,SFMono-Regular,Menlo,monospace}.scrape-arrow{color:#c9ae7c}.scrape-meta{margin-left:auto}
.filter-bar{display:flex;flex-wrap:wrap;gap:8px;align-items:center;background:var(--surface);border:1px solid var(--line);border-radius:var(--radius) var(--radius) 0 0;padding:13px 14px;margin:0}.filter-bar #f-text,.filter-bar #cf-text{min-width:210px;flex:1 1 230px}.filter-bar .chk{font-size:.7rem;color:#586277;display:flex;align-items:center;gap:6px;min-height:38px;padding:0 4px}.filter-bar input[type="checkbox"]{width:17px;height:17px;accent-color:var(--brand)}.country-picker-trigger{cursor:pointer;text-align:left;min-width:155px;background:#fbfcfe}.candidate-filter-note{flex-basis:100%;color:var(--muted);font-size:.67rem;line-height:1.5;border-top:1px dashed var(--line);padding-top:9px}
.table-scroll{overflow:auto;max-width:100%;-webkit-overflow-scrolling:touch;background:var(--surface);border:1px solid var(--line);border-top:0;border-radius:0 0 var(--radius) var(--radius);box-shadow:var(--shadow);scrollbar-width:thin}.node-table{min-width:1160px}.candidate-table{min-width:1080px}.management-table{min-width:760px}.proxyip-table{min-width:520px}table{width:100%;border-collapse:separate;border-spacing:0;font-size:.76rem}th{position:sticky;top:0;z-index:3;text-align:left;color:#7a8497;font-size:.65rem;letter-spacing:.025em;font-weight:800;padding:12px 11px;border-bottom:1px solid var(--line);background:#f9fafc;white-space:nowrap}td{padding:11px;border-bottom:1px solid #edf0f4;vertical-align:middle}tbody tr:last-child td{border-bottom:0}tr:hover td{background:#fafbfe}tr.active td{background:#eefbf7!important}tr.unavail{opacity:.56}
.mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace}.small{font-size:.69rem;color:var(--muted)}.note{color:var(--muted);font-size:.69rem;line-height:1.6;margin:10px 3px 14px}.note b{color:#4b5568}.note-inline{color:var(--muted);font-size:.65rem}.empty{text-align:center;padding:52px 20px!important;color:#8b95a5}.empty:before{content:"○";display:block;margin:0 auto 9px;width:32px;height:32px;line-height:29px;border:2px solid #d9dee7;border-radius:50%;color:#b3bbc8;font-size:1rem}
.proto,.anon,.badge-inuse,.badge-unavail,.candidate-state,.auth-badge{display:inline-block;padding:3px 7px;border-radius:999px;font-size:.61rem;font-weight:800;white-space:nowrap}.proto-socks5{background:#e6f4ff;color:#1474a8}.proto-http{background:#e6f8f1;color:#087d59}.proto-https{background:#f0eaff;color:#6941c6}.proto-proxyip{background:#fff1dd;color:#aa6208}.anon-elite{background:#e6f8f1;color:#087d59}.anon-anonymous{background:#eaf0ff;color:#3561b5}.anon-transparent{background:#fff0f2;color:#bf3c52}.anon-unknown{background:#eef0f4;color:#697386}.badge-inuse{background:#ddf8ee;color:#087d59}.badge-unavail{background:#fff0f2;color:#bf3c52}.exit-diff{color:var(--amber)}.score{font-weight:850}.score-hi{color:var(--green)}.score-mid{color:var(--amber)}.score-lo{color:var(--red)}
.speed-meta{display:block;color:#929aaa;font-size:.6rem;line-height:1.4;margin-top:3px;white-space:nowrap}.copy-btn{cursor:pointer;color:#7c8698;margin-left:5px;font-size:.63rem;background:#f0f2f7;border:0;padding:4px 6px;border-radius:6px}.copy-btn:hover{color:var(--brand-deep);background:#e8e7fb}.row-actions{display:flex;flex-wrap:nowrap;gap:4px;min-width:185px}.row-actions .btn-sm{margin:0}
.pager{display:flex;gap:9px;align-items:center;justify-content:center;margin:14px 0;flex-wrap:wrap}.candidate-pager-top{display:none}.candidate-pager-top:empty{display:none}
.candidate-state-available{background:#ddf8ee;color:#087d59}.candidate-state-unavailable{background:#fff0f2;color:#bf3c52}.candidate-state-failed{background:#fff1dd;color:#aa6208}.candidate-state-policy{background:#fff6d8;color:#8a6a00}.candidate-state-resource{background:#f0eaff;color:#6941c6}.candidate-state-deferred,.candidate-state-unknown{background:#eef0f4;color:#697386}.candidate-readonly{color:#9099a8;font-size:.65rem;white-space:nowrap}.auth-badge{margin-left:5px;background:#fff4d9;color:#906513}.candidate-verify-cell{min-width:250px}.proxyip-verify{display:grid;gap:6px;min-width:0}.proxyip-verify-actions,.proxyip-verify-summary{display:flex;align-items:center;gap:6px;flex-wrap:wrap}.proxyip-verify .btn-sm{margin:0;width:max-content}.proxyip-verify-state{font-size:.67rem;font-weight:800}.proxyip-verify-ok{color:var(--green)}.proxyip-verify-unavailable,.proxyip-verify-error{color:var(--red)}.proxyip-verify-latency,.proxyip-verify-support{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.65rem;color:#556074}.proxyip-verify-note{display:block;color:var(--muted);font-size:.62rem;line-height:1.4}
.protocol-quick{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:10px;margin:11px 0}.protocol-card{position:relative;background:var(--surface);border:1px solid var(--line);color:#586277;border-radius:13px;padding:12px 14px;cursor:pointer;text-align:left;min-width:0;overflow:hidden}.protocol-card:after{content:"";position:absolute;right:-13px;top:-13px;width:38px;height:38px;border-radius:50%;background:#f0efff}.protocol-card:hover,.protocol-card.active{border-color:#a9a1f7;background:#f8f7ff;box-shadow:0 8px 20px rgba(101,88,245,.09)}.protocol-card.active:before{content:"";position:absolute;inset:0 auto 0 0;width:3px;background:var(--brand)}.protocol-card strong{display:block;font-size:.69rem;text-transform:uppercase}.protocol-card span{display:block;font:760 .95rem ui-monospace,SFMono-Regular,Menlo,monospace;color:var(--ink);margin-top:5px}.protocol-card small{display:block;color:var(--muted);font-size:.61rem;margin-top:3px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.preset-bar,.checkurl-bar,form.inline,.default-group-editor,details.proxyip-section{background:var(--surface);border:1px solid var(--line);border-radius:var(--radius);box-shadow:0 4px 16px rgba(27,34,52,.025)}.preset-bar{padding:15px 17px;margin:0 0 13px;font-size:.75rem;color:var(--muted);display:flex;flex-wrap:wrap;gap:10px;align-items:center}.preset-bar b{color:var(--ink)!important}form.inline{display:flex;flex-wrap:wrap;gap:8px;margin-top:13px;padding:15px;align-items:center}.form-title{flex-basis:100%;display:flex;align-items:baseline;gap:9px;font-weight:820;font-size:.8rem;margin-bottom:4px}.form-title span{color:var(--muted);font-size:.66rem;font-weight:500}.default-group-editor{margin-top:13px;padding:14px;display:flex;gap:8px;align-items:center;font-size:.73rem;color:var(--muted)}details.proxyip-section{margin-top:14px;padding:14px 16px}summary{cursor:pointer;color:#4e596d;font-size:.76rem;font-weight:750}
.list-notice{border:1px solid #dce1eb;border-top:0;padding:9px 14px;background:#f9fafc;color:#697386;font-size:.69rem}.list-notice[data-tone="loading"]:before{content:"";display:inline-block;width:12px;height:12px;margin-right:7px;border:2px solid #d4d8e8;border-top-color:var(--brand);border-radius:50%;vertical-align:-2px;animation:spin .75s linear infinite}.list-notice[data-tone="error"]{background:#fff5f6;color:#b83950;border-color:#f2cbd2}@keyframes spin{to{transform:rotate(360deg)}}
.danger-zone{margin-top:16px;border:1px solid #f0d9dd;border-radius:13px;background:#fff9fa}.danger-zone>summary{padding:12px 15px;color:#a94557;list-style-position:inside}.danger-zone>div{padding:0 15px 14px}.danger-zone p{font-size:.68rem;color:#7c6670;line-height:1.5;margin-bottom:9px}
.security-optin{flex-basis:100%;display:flex;align-items:flex-start;gap:9px;padding:11px 12px;border:1px solid #f0d49f;border-radius:11px;background:#fffbf2;color:#735b2e;font-size:.69rem;line-height:1.5}.security-optin input{width:18px;height:18px;min-height:0;margin-top:1px;accent-color:var(--amber);flex:0 0 auto}.security-optin strong{display:block;color:#694b13}.private-source-badge{display:inline-block;margin-left:6px;padding:2px 6px;border-radius:999px;background:#fff1d6;color:#99620b;font-size:.58rem;font-weight:850;white-space:nowrap}
.mobile-detail-toggle{display:none}.node-pager-top{display:none}.node-pager-top:empty{display:none}
.toast-region{position:fixed;right:20px;top:20px;z-index:2200;display:grid;gap:8px;width:min(360px,calc(100vw - 24px));pointer-events:none}.toast{pointer-events:auto;background:#172033;color:#fff;border-radius:12px;padding:12px 14px;box-shadow:0 15px 35px rgba(17,24,39,.28);font-size:.75rem;animation:toast-in .2s ease-out}.toast.error{background:#9e3044}.toast.success{background:#087d59}@keyframes toast-in{from{opacity:0;transform:translateY(-8px)}to{opacity:1;transform:none}}
.result-overlay{position:fixed;inset:0;z-index:2100;background:rgba(17,24,39,.68);display:grid;place-items:center;padding:18px;backdrop-filter:blur(5px)}.result-overlay[hidden]{display:none}.result-dialog{width:min(520px,100%);background:#fff;border-radius:18px;box-shadow:0 25px 80px rgba(0,0,0,.3);overflow:hidden}.result-dialog-head{display:flex;align-items:center;justify-content:space-between;padding:16px 18px;border-bottom:1px solid var(--line)}.result-dialog-head h2{font-size:1rem}.result-dialog-close{border:0;background:#eef0f4;color:#697386;border-radius:8px;width:34px;height:34px;cursor:pointer}.result-dialog-body{padding:18px;color:#4d586c;font-size:.77rem;line-height:1.7;white-space:pre-wrap;max-height:65vh;overflow:auto}.result-dialog-foot{padding:0 18px 18px;text-align:right}
.switch{position:relative;display:inline-block;width:42px;height:26px}.switch input{opacity:0;width:0;height:0}.slider{position:absolute;cursor:pointer;inset:0;background:#cdd3de;border-radius:20px;transition:.15s}.slider:before{position:absolute;content:"";height:18px;width:18px;left:4px;bottom:4px;background:#fff;border-radius:50%;box-shadow:0 2px 5px rgba(0,0,0,.16);transition:.15s}input:checked+.slider{background:var(--brand)}input:checked+.slider:before{transform:translateX(16px)}.switch input:focus-visible+.slider{outline:3px solid rgba(101,88,245,.3);outline-offset:2px}
.country-picker-overlay{position:fixed;inset:0;z-index:1000;background:rgba(17,24,39,.72);display:flex;align-items:center;justify-content:center;padding:22px;backdrop-filter:blur(6px)}.country-picker-overlay[hidden]{display:none}.country-picker{width:min(940px,100%);max-height:min(760px,calc(100vh - 44px));background:var(--surface);border:1px solid rgba(255,255,255,.3);border-radius:20px;box-shadow:0 30px 90px rgba(0,0,0,.3);display:flex;flex-direction:column;overflow:hidden}.country-picker-head{display:flex;align-items:center;gap:10px;padding:17px 19px;border-bottom:1px solid var(--line);background:#fbfbfe}.country-picker-head h2{font-size:1.03rem;color:var(--ink);flex:1}.country-picker-head input{min-width:240px}.country-picker-close{background:#eef0f4;border:0;color:#697386;font-size:1.35rem;line-height:1;cursor:pointer;padding:6px 10px;border-radius:9px}.country-picker-close:hover{background:#e3e6ed;color:var(--ink)}.country-picker-body{display:grid;grid-template-columns:minmax(290px,42%) 1fr;min-height:0;overflow:hidden}.country-map{padding:17px;background:radial-gradient(circle at 50% 45%,#edf0ff 0,#f5f6fb 60%,#f9fafc 100%);border-right:1px solid var(--line);overflow:auto}.country-map-title{font-size:.69rem;color:var(--muted);margin-bottom:11px}.country-map-grid{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));grid-template-rows:repeat(3,78px);gap:8px;grid-template-areas:"na na eu as" "sa sa af as" "oc oc an unknown"}.continent-tile{border:1px solid #dce1eb;background:rgba(255,255,255,.82);color:#556074;border-radius:12px;padding:8px;cursor:pointer;display:flex;flex-direction:column;justify-content:center;align-items:center;text-align:center;min-width:0}.continent-tile:hover,.continent-tile.active{border-color:#9f96f2;background:var(--brand);color:#fff;box-shadow:0 8px 18px rgba(101,88,245,.18)}.continent-tile strong{font-size:.7rem;white-space:nowrap}.continent-tile span{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.66rem;color:#8690a1;margin-top:3px}.continent-tile.active span{color:#e2dfff}.continent-na{grid-area:na}.continent-sa{grid-area:sa}.continent-eu{grid-area:eu}.continent-as{grid-area:as}.continent-af{grid-area:af}.continent-oc{grid-area:oc}.continent-an{grid-area:an}.continent-unknown{grid-area:unknown}.country-map-note{font-size:.66rem;color:var(--muted);line-height:1.5;margin-top:11px}.country-list-pane{min-width:0;display:flex;flex-direction:column;overflow:hidden}.country-list-toolbar{display:flex;gap:7px;align-items:center;padding:12px 14px;border-bottom:1px solid var(--line)}.country-list{overflow:auto;padding:10px 13px 15px;scrollbar-width:thin}.country-continent-group{margin-bottom:13px}.country-continent-title{display:flex;justify-content:space-between;color:#7c8698;font-size:.67rem;font-weight:800;padding:6px 3px;border-bottom:1px solid #edf0f4;margin-bottom:5px}.country-option{width:100%;display:grid;grid-template-columns:32px 1fr auto;gap:7px;align-items:center;background:transparent;border:0;color:var(--ink);border-radius:9px;padding:9px;cursor:pointer;text-align:left}.country-option:hover,.country-option.active{background:#f1f0ff}.country-option.active{box-shadow:inset 3px 0 var(--brand)}.country-option-code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-weight:800}.country-option-count{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;color:var(--muted);font-size:.67rem}.country-option-empty{padding:34px 10px;text-align:center;color:var(--muted)}body.modal-open{overflow:hidden}
@media(max-width:1260px){.stats{grid-template-columns:repeat(4,minmax(0,1fr))}.stat-item:nth-child(6),.stat-item:nth-child(7){min-height:78px}.health-card{grid-template-columns:1fr}.candidate-summary{grid-template-columns:repeat(3,minmax(0,1fr))}}
@media(max-width:920px){.app-shell{display:block}.sidebar{position:fixed;top:auto;bottom:0;left:0;right:0;width:100%;height:auto;padding:7px max(8px,env(safe-area-inset-right)) calc(7px + env(safe-area-inset-bottom)) max(8px,env(safe-area-inset-left));display:block;z-index:900;box-shadow:0 -10px 30px rgba(17,24,39,.2)}.sidebar:before,.sidebar:after,.brand,.nav-label,.sidebar-foot{display:none}.tabs{display:grid;grid-template-columns:repeat(5,minmax(0,1fr));gap:4px}.tab-link{position:relative;min-height:51px;padding:5px 2px;display:flex;flex-direction:column;justify-content:center;gap:1px;text-align:center;font-size:.61rem;border-radius:9px}.tab-link:before{height:23px;font-size:1rem}.tab-link:hover{transform:none}.tab-link.active:after{position:absolute;margin:0;top:4px;width:4px;height:4px}.main-shell{padding:0 20px calc(84px + env(safe-area-inset-bottom))}.topbar{padding-top:21px}}
@media(max-width:700px){body{font-size:16px}.main-shell{padding-left:12px;padding-right:12px}.topbar{align-items:flex-start;padding:17px 1px 15px}.page-description{display:none}.topbar-actions{flex-direction:column-reverse;align-items:flex-end;gap:5px}.refresh-status{font-size:.62rem;max-width:180px}.btn{min-height:44px}.stats{grid-template-columns:repeat(2,minmax(0,1fr));gap:8px}.stat-item{min-height:84px;padding:13px}.stat-item span{font-size:1.08rem;margin-top:10px}.stat-item:nth-child(6) span,.stat-item:nth-child(7) span{font-size:.65rem}.health-card{gap:8px}.checkurl-bar{grid-template-columns:1fr;padding:14px}.checkurl-label,.checkurl-hint{white-space:normal}.checkurl-bar input,.checkurl-bar button{width:100%}.refresh-timeline{padding:14px}.panel-heading{align-items:flex-start}.panel-heading p{font-size:.69rem}.scope-intro{padding:11px 12px}.scope-icon{display:none}.current-node{line-height:1.6;padding:16px}.current-node .cn-addr{display:block;overflow-wrap:anywhere}.current-node .cn-meta{display:block;margin-left:0}.lock-badge,.auto-badge{margin:8px 6px 0 0}.group-cards{grid-template-columns:repeat(2,minmax(0,1fr))}.node-summary,.candidate-summary{grid-template-columns:repeat(2,minmax(0,1fr))}.candidate-summary .node-summary-item:first-child{grid-column:1/-1}.protocol-quick{grid-template-columns:repeat(2,minmax(0,1fr))}.scrape-flow{align-items:flex-start}.scrape-step{flex:1 1 calc(50% - 16px)}.scrape-arrow{display:none}.scrape-meta{flex-basis:100%;margin-left:0}.filter-bar{align-items:stretch;padding:11px;border-radius:14px}.filter-bar>input,.filter-bar>select,.filter-bar>.country-picker-trigger{flex:1 1 calc(50% - 4px);min-width:0}.filter-bar>#f-text,.filter-bar>#cf-text{flex-basis:100%}input,select,.btn,.btn-sm,.country-picker-trigger{min-height:44px;font-size:.83rem;padding:9px 11px}.filter-bar .chk{flex:1 1 calc(50% - 4px);min-height:44px;font-size:.78rem;padding:5px 2px}.node-table-scroll,.candidate-table-scroll{overflow:visible;background:transparent;border:0;box-shadow:none}.node-table,.node-table tbody,.candidate-table,.candidate-table tbody{display:block;min-width:0}.node-table thead,.candidate-table thead{display:none}.node-table tr,.candidate-table tr{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));background:var(--surface);border:1px solid var(--line);border-radius:14px;margin:10px 0;overflow:hidden;box-shadow:0 5px 16px rgba(27,34,52,.04)}.node-table td,.candidate-table td{display:block;min-width:0;padding:10px 11px;border-bottom:1px solid #edf0f4;overflow-wrap:anywhere}.node-table td:before,.candidate-table td:before{content:attr(data-label);display:block;color:#8a94a5;font-size:.63rem;font-weight:750;margin-bottom:4px}.node-table td[data-label="地址(节点IP)"],.node-table td[data-label="来源"],.node-table td[data-label="操作"],.candidate-table td[data-label="候选地址"],.candidate-table td[data-label="来源"],.candidate-table td[data-label="专用验证"]{grid-column:1/-1}.node-table tr:not(.mobile-expanded) td.mobile-secondary,.candidate-table tr:not(.mobile-expanded) td.mobile-secondary{display:none}.mobile-detail-toggle{display:inline-flex;align-items:center;justify-content:center;margin-left:6px;border:0;background:#f0efff;color:var(--brand-deep);border-radius:7px;padding:6px 8px;min-height:36px;font-size:.68rem;font-weight:750;cursor:pointer}.node-table td.empty,.candidate-table td.empty{grid-column:1/-1;padding:38px 12px!important}.node-table td.empty:before,.candidate-table td.empty:before{content:"○"}.node-table tr:hover td,.candidate-table tr:hover td{background:transparent}.node-table tr.active td{background:#eefbf799!important}.row-actions{min-width:0;display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:7px}.row-actions .btn-sm{width:100%;padding-left:4px;padding-right:4px;font-size:.72rem}.copy-btn{min-height:38px;padding:7px}.switch{height:44px}.switch .slider{top:9px;bottom:9px}summary{min-height:44px;display:flex;align-items:center}.pager .btn-sm{min-width:96px}.candidate-pager-top,.node-pager-top{display:flex;position:sticky;top:8px;z-index:20;margin:8px 0;padding:7px;background:rgba(255,255,255,.94);border:1px solid var(--line);border-radius:11px;box-shadow:var(--shadow);backdrop-filter:blur(8px)}form.inline{align-items:stretch;padding:12px}.form-title{display:block}.form-title span{display:block;margin-top:3px}form.inline input,form.inline select,form.inline .btn{flex:1 1 100%;min-width:0!important}.default-group-editor{align-items:stretch;flex-wrap:wrap}.default-group-editor select{flex:1 1 160px;min-width:0}.management-table{min-width:680px}.country-picker-overlay{padding:0;align-items:stretch}.country-picker{max-height:none;width:100%;border:0;border-radius:0}.country-picker-head{flex-wrap:wrap;padding:13px}.country-picker-head h2{font-size:.94rem}.country-picker-head input{order:3;flex:1 1 100%;min-width:0}.country-picker-body{grid-template-columns:1fr;grid-template-rows:auto 1fr}.country-map{border-right:0;border-bottom:1px solid var(--line);padding:10px 12px;overflow:visible}.country-map-title,.country-map-note{display:none}.country-map-grid{grid-template-columns:repeat(4,minmax(0,1fr));grid-template-rows:repeat(2,54px);grid-template-areas:"as eu na sa" "oc af an unknown";gap:5px}.continent-tile{padding:4px}.continent-tile strong{font-size:.6rem}.continent-tile span{font-size:.58rem}.country-list-toolbar{padding:8px 12px}.country-list-toolbar .btn-sm{min-width:0;flex:1}.toast-region{top:10px;right:12px}.result-overlay{padding:0;align-items:end}.result-dialog{border-radius:18px 18px 0 0;max-height:88vh}}
@media(max-width:430px){.stats{grid-template-columns:1fr 1fr}.stat-item:nth-child(6),.stat-item:nth-child(7){grid-column:span 1}.group-cards{grid-template-columns:1fr}.tabs{gap:2px}.tab-link{font-size:.56rem}.candidate-filter-note{display:none}body[data-view="candidates"]>.app-shell .health-card,body[data-view="sources"]>.app-shell .stats,body[data-view="sources"]>.app-shell .health-card,body[data-view="rules"]>.app-shell .stats,body[data-view="rules"]>.app-shell .health-card,body[data-view="groups"]>.app-shell .stats,body[data-view="groups"]>.app-shell .health-card{display:none}}
@media(max-width:700px){.management-table,.management-table tbody{display:block;min-width:0}.management-table tr:first-child{display:none}.management-table tr:not(:first-child){display:grid;grid-template-columns:repeat(2,minmax(0,1fr));background:var(--surface);border:1px solid var(--line);border-radius:14px;margin:10px 0;overflow:hidden;box-shadow:0 5px 16px rgba(27,34,52,.04)}.management-table td{display:block;min-width:0;padding:10px 11px;overflow-wrap:anywhere}.management-table td:before{content:attr(data-label);display:block;color:#8a94a5;font-size:.63rem;font-weight:750;margin-bottom:4px}.management-table td[data-label="来源名称"],.management-table td[data-label="订阅地址"],.management-table td[data-label="匹配值"],.management-table td[data-label="过滤条件"],.management-table td[data-label="成员 / 当前节点"],.management-table td[data-label="操作"]{grid-column:1/-1}.table-scroll:has(.management-table){overflow:visible;background:transparent;border:0;box-shadow:none}}
details.help-panel{margin:0 0 13px;padding:13px 15px;background:var(--surface);border:1px solid var(--line);border-radius:13px}details.help-panel .note{margin:10px 0 0}#tab-nodes>.node-summary{display:none}
@media(prefers-reduced-motion:reduce){*,*:before,*:after{scroll-behavior:auto!important;animation:none!important;transition:none!important}}
</style>
</head>
<body>
<a class="skip-link" href="#main-content">跳到主要内容</a>
<div class="app-shell">
<aside class="sidebar" aria-label="主导航">
  <div class="brand">
    <div class="brand-mark" aria-hidden="true">PA</div>
    <div class="brand-copy"><strong>Proxy Atlas</strong><span>Node Control</span></div>
  </div>
  <div class="nav-label">资源管理</div>
  <nav class="tabs" role="tablist" aria-label="管理页面">
    <a id="tab-link-nodes" href="#nodes" class="tab-link" data-tab="nodes" data-icon="◉" role="tab" aria-controls="tab-nodes">代理池</a>
    <a id="tab-link-candidates" href="#candidates" class="tab-link" data-tab="candidates" data-icon="◫" role="tab" aria-controls="tab-candidates">候选目录</a>
    <a id="tab-link-sources" href="#sources" class="tab-link" data-tab="sources" data-icon="⌁" role="tab" aria-controls="tab-sources">订阅来源</a>
    <a id="tab-link-rules" href="#rules" class="tab-link" data-tab="rules" data-icon="⑂" role="tab" aria-controls="tab-rules">分流规则</a>
    <a id="tab-link-groups" href="#groups" class="tab-link" data-tab="groups" data-icon="◇" role="tab" aria-controls="tab-groups">分组策略</a>
  </nav>
  <div class="sidebar-foot">
    <div class="system-online">控制台已连接</div>
    <a class="gh-link" href="https://github.com/kim1232aa/socks5-pool-pro" target="_blank" rel="noopener"><svg viewBox="0 0 16 16" aria-hidden="true"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z"/></svg>查看项目源码</a>
  </div>
</aside>

<main id="main-content" class="main-shell" tabindex="-1">
<div class="container">
  <header class="topbar">
    <div><span class="eyebrow">Live Operations</span><h1 id="page-title">节点运行中心</h1><p id="page-description" class="page-description">查看健康状态、真实出口与路由节点，所有列表均由服务端分页。</p></div>
    <div class="topbar-actions">
      <span id="refresh-status" class="small refresh-status" role="status" aria-live="polite"></span>
      <button id="refresh-btn" type="button" class="btn" onclick="doRefresh(this)" aria-describedby="refresh-status">立即刷新节点</button>
    </div>
  </header>

  <section class="stats" aria-label="代理池概览">
    <div class="stat-item">池内节点<span id="stat-total">{{.Total}}</span></div>
    <div class="stat-item">健康可用<span id="stat-available">加载中</span></div>
    <div class="stat-item">暂时不可用<span id="stat-unavailable">加载中</span></div>
    <div class="stat-item">当前筛选<span id="stat-matching">加载中</span></div>
    <div class="stat-item">ProxyIP 资源<span id="stat-proxyip">{{.ProxyIPTotal}}</span></div>
    <div class="stat-item">最近刷新<span id="stat-last">{{if .LastScrape}}{{.LastScrape}}{{else}}N/A{{end}}</span></div>
    <div class="stat-item">计划刷新<span id="stat-next">{{if .NextScrape}}{{.NextScrape}}{{else}}N/A{{end}}</span></div>
  </section>

  <section class="health-card" aria-label="检测设置与刷新计划">
    <div class="checkurl-bar">
      <label class="checkurl-label" for="check-url-input">可用性检测目标</label>
      <input id="check-url-input" type="url" value="{{.CheckURL}}" placeholder="http://www.google.com/generate_204">
      <button type="button" class="btn-sm" onclick="saveCheckURL()">保存并重检</button>
      <span class="checkurl-hint">节点能通过代理访问该地址并取得任意 HTTP 响应即视为可用。修改后会立即启动全池复检。</span>
      <span id="check-url-status" class="small" role="status" aria-live="polite"></span>
    </div>
    <div class="refresh-timeline" aria-label="刷新时间">
      <div class="timeline-item">上一次完成<strong id="timeline-last">{{if .LastScrape}}{{.LastScrape}}{{else}}尚未刷新{{end}}</strong></div>
      <div class="timeline-item">下一次计划<strong id="timeline-next">{{if .NextScrape}}{{.NextScrape}}{{else}}等待调度{{end}}</strong></div>
    </div>
  </section>

  <div class="content-stage">

<div id="tab-nodes" class="tab-panel" role="tabpanel" aria-labelledby="tab-link-nodes">
  <div class="panel-heading"><div><span class="panel-kicker">Routable Pool</span><h2>代理池</h2><p>管理已经过检测并进入本地转发池的节点。</p></div></div>
  <div class="scope-intro">
    <span class="scope-icon" aria-hidden="true">✅</span>
    <div><strong>可路由代理池 <span class="scope-tag">已检测 / 已收录</span></strong>这里显示已经进入本地转发池的节点，包括当前可用和暂时不可用但仍保留的节点。它不是来源里抓到的全部地址；要查看几十万条原始去重候选，请切换到“全部抓取候选”。</div>
  </div>
  <div id="current-node-banner" class="current-node" aria-live="polite">当前使用节点: <span class="cn-addr">加载中...</span></div>

  <div id="group-cards-container" class="group-cards"></div>

  <details class="help-panel"><summary>了解出口定位、评分与节点保留规则</summary><p class="note">"国家/城市"是<b>真实出口 IP</b> 的定位(穿过节点探测),不是节点自身 IP。"匿名"=高匿(elite,不暴露)/普通(anonymous,可被识别为代理)/透明(transparent,泄露你的真实IP)。"评分"综合成功率/延迟/速度。默认已开启剔除"假代理"(出口IP==本机出口的透明节点),用 -require-ip-change=false 关闭。点节点上的<b>"使用"</b>即把默认(ANY)出口<b>手动锁定</b>到该节点,后台自动轮换会暂停;点上方横幅的<b>"恢复自动轮换"</b>可解锁。<b>节点不会被自动删除</b>:每轮刷新只标记"可用/不可用",不可用的节点默认被"隐藏不可用"过滤掉但仍保留在池中,下次测活成功会自动恢复显示;要彻底删除不可用节点,请在列表底部展开维护区并手动确认。</p></details>

  <div class="node-summary" aria-label="节点列表统计">
    <div class="node-summary-item">池内总数<strong id="node-total">加载中</strong></div>
    <div class="node-summary-item">当前可用<strong id="node-available">加载中</strong></div>
    <div class="node-summary-item">不可用但仍保留<strong id="node-unavailable">加载中</strong></div>
    <div class="node-summary-item">当前筛选匹配<strong id="node-matching">加载中</strong></div>
  </div>
  <div id="scrape-flow" class="scrape-flow" aria-label="最近一轮节点处理链路" hidden>
    <span class="scrape-step">来源原始 <strong id="scrape-raw">0</strong></span><span class="scrape-arrow">→</span>
    <span class="scrape-step">去重候选 <strong id="scrape-candidates">0</strong></span><span class="scrape-arrow">→</span>
    <span class="scrape-step">本轮检测 <strong id="scrape-checked">0</strong></span><span class="scrape-arrow">→</span>
    <span class="scrape-step">本轮通过 <strong id="scrape-alive">0</strong></span>
    <span id="scrape-meta" class="scrape-meta"></span>
  </div>

  <div class="filter-bar">
    <label class="sr-only" for="f-text">搜索节点 IP 或地址</label>
    <input id="f-text" placeholder="搜索 IP/地址..." oninput="onFilterChange()" autocomplete="off">
    <input id="f-country" type="hidden" value="">
    <button id="f-country-button" type="button" class="country-picker-trigger" onclick="openNodeCountryPicker()" aria-haspopup="dialog" aria-expanded="false" aria-controls="candidate-country-modal">🗺️ 全部实测出口国家</button>
    <label class="sr-only" for="f-proto">按协议筛选</label>
    <select id="f-proto" onchange="onFilterChange()"><option value="">全部协议</option><option>socks5</option><option>http</option><option>https</option></select>
    <label class="sr-only" for="f-sort">节点排序方式</label>
    <select id="f-sort" onchange="onFilterChange()">
      <option value="score">按评分↓</option>
      <option value="latency">按延迟↑</option>
      <option value="speed">按速度↓</option>
      <option value="country">按国家</option>
    </select>
    <label class="sr-only" for="f-pagesize">每页显示数量</label>
    <select id="f-pagesize" onchange="onPageSizeChange()">
      <option value="10">每页10</option>
      <option value="20">每页20</option>
      <option value="50">每页50</option>
      <option value="100">每页100</option>
    </select>
    <label class="chk"><input type="checkbox" id="f-ipchanged" onchange="onFilterChange()"> 只看真正改IP的</label>
    <label class="chk"><input type="checkbox" id="f-hide-unavail" checked onchange="onFilterChange()"> 隐藏不可用</label>
    <button type="button" class="btn-sm" onclick="exportNodes('csv')" title="按延迟升序,UTF-8 BOM,Excel 可直接打开">导出CSV</button>
    <button type="button" class="btn-sm" onclick="exportNodes('tme')" title="Telegram SOCKS 链接(仅 socks5 节点)">导出t.me</button>
    <span id="node-count" class="small" role="status" aria-live="polite"></span>
  </div>
  <div id="node-notice" class="list-notice" role="status" aria-live="polite" hidden></div>

  <div class="pager node-pager-top" id="node-pager-top" aria-label="代理池分页（顶部）"></div>
  <div class="table-scroll node-table-scroll" tabindex="0" aria-label="节点表格，可横向滚动">
  <table class="node-table">
  <caption class="sr-only">代理节点状态和操作</caption>
  <thead><tr><th></th><th>协议</th><th>地址(节点IP)</th><th>出口IP</th><th>匿名</th><th>国家/城市</th><th>评分</th><th title="健康检查累计成功/失败次数">成功/失败</th><th>延迟</th><th>速度</th><th>来源</th><th>操作</th></tr></thead>
  <tbody id="node-tbody"><tr><td colspan="12" class="empty">加载中...</td></tr></tbody>
  </table>
  </div>
  <div class="pager" id="node-pager"></div>

  <details class="danger-zone">
    <summary>维护与危险操作</summary>
    <div><p>不可用节点默认只隐藏并会在恢复后重新出现。仅在确认不再保留历史节点时执行永久清理。</p><button type="button" class="btn-sm danger" onclick="clearUnavailable()">永久清理全部不可用节点</button></div>
  </details>

  <details class="proxyip-section">
    <summary>ProxyIP 节点(仅展示,不参与本地转发) - {{.ProxyIPTotal}} 个</summary>
    <p class="note">这些是供 Cloudflare Worker/VLESS/Trojan 隧道使用的外部反代跳板，不是 Cloudflare 边缘 IP，也不支持通用 SOCKS5/HTTP 代理协议，因此不会被本地 SOCKS5 服务转发使用。为避免把大量记录塞进首页 HTML，完整列表已统一放入服务端分页的“全部抓取候选”。</p>
    <button type="button" class="btn-sm" onclick="showCandidateProtocol('proxyip')">打开 ProxyIP 完整目录</button>
  </details>
</div>

<div id="tab-candidates" class="tab-panel" style="display:none" role="tabpanel" aria-labelledby="tab-link-candidates">
  <div class="panel-heading"><div><span class="panel-kicker">Candidate Inventory</span><h2>候选目录</h2><p>分页浏览全部来源的去重候选，不把几十万条数据一次塞进浏览器。</p></div></div>
  <div class="scope-intro">
    <span class="scope-icon" aria-hidden="true">📚</span>
    <div><strong>全部抓取候选 <span class="scope-tag">只读目录</span></strong>这里按页展示最近一次从所有已启用来源抓取、按“协议 + 地址”去重后的完整候选目录。候选不等于可用代理：受每轮检测上限影响，部分节点仍在排队；检测失败的节点也会保留在此目录，后续轮转继续验证。这里的国家来自来源声明；为空表示来源没有标注，代理池页才显示经过转发验证的真实出口地区。</div>
  </div>

  <div class="node-summary candidate-summary" aria-label="完整候选目录统计">
    <div class="node-summary-item">去重候选总数<strong id="candidate-total">加载中</strong></div>
    <div class="node-summary-item">当前筛选匹配<strong id="candidate-matching">加载中</strong></div>
    <div class="node-summary-item">已进入代理池<strong id="candidate-known">加载中</strong></div>
    <div class="node-summary-item">仍待检测<strong id="candidate-deferred">加载中</strong></div>
    <div class="node-summary-item">来源地区未知<strong id="candidate-country-unknown">加载中</strong></div>
  </div>

  <div id="candidate-protocol-cards" class="protocol-quick" aria-label="按协议快速筛选候选"></div>

  <div class="filter-bar" aria-label="候选目录筛选">
    <label class="sr-only" for="cf-text">搜索候选地址或来源</label>
    <input id="cf-text" placeholder="搜索 IP / 地址 / 来源..." oninput="onCandidateFilterChange()" autocomplete="off">
    <label class="sr-only" for="cf-source">按来源筛选候选</label>
    <select id="cf-source" onchange="onCandidateFilterChange()"><option value="">全部来源</option></select>
    <label class="sr-only" for="cf-proto">按协议筛选候选</label>
    <select id="cf-proto" onchange="onCandidateFilterChange()"><option value="">全部协议</option><option value="socks5">socks5</option><option value="http">http</option><option value="https">https</option><option value="proxyip">proxyip（不参与转发）</option></select>
    <input id="cf-country" type="hidden" value="">
    <button id="cf-country-button" type="button" class="country-picker-trigger" onclick="openCandidateCountryPicker()" aria-haspopup="dialog" aria-expanded="false" aria-controls="candidate-country-modal">🗺️ 全部国家/地区</button>
    <label class="sr-only" for="cf-status">按检测状态筛选候选</label>
    <select id="cf-status" onchange="onCandidateFilterChange()">
      <option value="">全部状态</option>
      <option value="known_available">池内可用</option>
      <option value="known_unavailable">池内不可用</option>
      <option value="checked_failed">最近检测失败</option>
      <option value="policy_filtered">策略排除</option>
      <option value="resource">ProxyIP 资源（不路由）</option>
      <option value="deferred">仍待检测</option>
    </select>
    <label class="sr-only" for="cf-pagesize">候选每页显示数量</label>
    <select id="cf-pagesize" onchange="onCandidatePageSizeChange()">
      <option value="10">每页10</option>
      <option value="20">每页20</option>
      <option value="50" selected>每页50</option>
      <option value="100">每页100</option>
    </select>
    <span id="candidate-count" class="small" role="status" aria-live="polite"></span>
    <span class="candidate-filter-note">筛选和分页都由服务端执行，浏览器不会一次下载几十万条记录。目录为只读，只有进入代理池的节点才能在“代理池”页执行使用、测速或验证。ProxyIP“专用验证”只会在你逐条点击时调用外部检测服务；结果仅供 Cloudflare Worker ProxyIP 参考，绝不改变资源状态或本地代理池。</span>
  </div>
  <div id="candidate-notice" class="list-notice" role="status" aria-live="polite" hidden></div>

  <div class="pager candidate-pager-top" id="candidate-pager-top" aria-label="候选分页（顶部）"></div>
  <div class="table-scroll candidate-table-scroll" tabindex="0" aria-label="全部抓取候选表格">
  <table class="candidate-table">
  <caption class="sr-only">所有来源抓取到的去重代理候选</caption>
  <thead><tr><th>状态</th><th>协议</th><th>候选地址</th><th>来源标注地区</th><th>来源</th><th>专用验证</th></tr></thead>
  <tbody id="candidate-tbody"><tr><td colspan="6" class="empty">切换到本页后加载完整候选目录...</td></tr></tbody>
  </table>
  </div>
  <div class="pager" id="candidate-pager"></div>
</div>

<div id="candidate-country-modal" class="country-picker-overlay" role="dialog" aria-modal="true" aria-labelledby="candidate-country-title" hidden onclick="candidateCountryBackdrop(event)">
  <div class="country-picker">
    <div class="country-picker-head">
      <h2 id="candidate-country-title">🗺️ 按国家/地区浏览全部候选</h2>
      <label class="sr-only" for="candidate-country-search">搜索国家代码</label>
      <input id="candidate-country-search" placeholder="搜索国家代码，如 JP / US" oninput="renderCandidateCountryPicker()" autocomplete="off">
      <button type="button" class="country-picker-close" onclick="closeCandidateCountryPicker()" aria-label="关闭国家筛选">×</button>
    </div>
    <div class="country-picker-body">
      <div class="country-map">
        <div id="country-picker-map-title" class="country-map-title">选择大洲，右侧查看该区域的实时数量</div>
        <div id="candidate-continent-map" class="country-map-grid"></div>
        <p id="country-picker-note" class="country-map-note">国家和数量来自当前服务端数据。</p>
      </div>
      <div class="country-list-pane">
        <div class="country-list-toolbar">
          <button id="country-picker-all" type="button" class="btn-sm" onclick="chooseCandidateCountry('')">全部地区</button>
          <button type="button" class="btn-sm" onclick="chooseCandidateCountry('__unknown__')">国家未知</button>
          <span id="candidate-country-result-count" class="small"></span>
        </div>
        <div id="candidate-country-list" class="country-list"></div>
      </div>
    </div>
  </div>
</div>

<div id="tab-sources" class="tab-panel" style="display:none" role="tabpanel" aria-labelledby="tab-link-sources">
  <div class="panel-heading"><div><span class="panel-kicker">Subscriptions</span><h2>订阅来源</h2><p>控制抓取入口、数据格式及启用状态；内置与自定义来源统一管理。</p></div></div>
  <div class="scope-intro"><span class="scope-icon" aria-hidden="true">⌁</span><div><strong>来源决定候选目录的边界</strong>停用来源不会立即删除池内节点；下一次刷新会按当前启用集合重建候选快照并重新检测。</div></div>
  <div class="table-scroll" tabindex="0" aria-label="来源表格，可横向滚动">
  <table class="management-table">
  <caption class="sr-only">代理来源管理</caption>
  <tr><th>名称</th><th>URL</th><th>格式</th><th>类型</th><th>启用</th><th>操作</th></tr>
  {{range .Sources}}
  <tr>
    <td data-label="来源名称">{{.Name}}{{if .AllowPrivate}}<span class="private-source-badge" title="该来源已显式允许访问局域网或保留地址">允许私网</span>{{end}}{{if .Note}}<div class="note-inline">{{.Note}}</div>{{end}}</td>
    <td data-label="订阅地址" class="mono small">{{.URL}}</td>
    <td data-label="数据格式" class="small">{{.Format}}{{if .Protocol}} ({{.Protocol}}){{end}}</td>
    <td data-label="来源类型" class="small">{{if .Builtin}}内置{{else}}自定义{{end}}</td>
    <td data-label="启用状态">
      <label class="switch">
        <input type="checkbox" aria-label="切换来源 {{.Name}}" {{if .Enabled}}checked{{end}} onchange="postJSON('/api/sources/toggle',{id:'{{.ID}}',enabled:this.checked},reloadOrAlert)">
        <span class="slider"></span>
      </label>
    </td>
    <td data-label="操作"><button type="button" class="btn-sm danger" onclick="if(confirm('删除来源 {{.Name}}?'))postJSON('/api/sources/delete',{id:'{{.ID}}'},reloadOrAlert)">删除</button></td>
  </tr>
  {{end}}
  </table>
  </div>

  <form class="inline" id="form-add-source">
    <div class="form-title">添加自定义来源<span>支持纯文本、JSON 与 ProxyIP 专用目录</span></div>
    <label class="sr-only" for="source-name">来源名称</label>
    <input id="source-name" name="name" placeholder="名称" required>
    <label class="sr-only" for="source-url">来源 URL</label>
    <input id="source-url" name="url" type="url" placeholder="URL" required style="min-width:280px">
    <label class="sr-only" for="source-format">来源格式</label>
    <select id="source-format" name="format">{{range .Formats}}<option value="{{.}}">{{.}}</option>{{end}}</select>
    <label class="sr-only" for="source-protocol">来源协议</label>
    <input id="source-protocol" name="protocol" placeholder="协议(仅纯文本/JSON数组格式需要,如 socks5)">
    <label class="security-optin" for="source-allow-private"><input id="source-allow-private" name="allow_private" type="checkbox"><span><strong>允许访问私网 / 保留地址（高风险）</strong>仅当订阅服务由你控制并部署在可信局域网时开启。公网来源必须保持关闭，以阻止来源 URL 访问本机、内网和云元数据地址。</span></label>
    <button class="btn" type="submit">添加来源</button>
  </form>
  <p class="note">格式说明: text-regex = 文本中扫描 "scheme://ip:port"; edt-json = EDT-Pages 风格 JSON 数组; proxyip-json = Cloudflare ProxyIP 专用格式; plain-list = 每行一个 "ip:port"(需填协议); json-array = JSON 字符串数组,每项 "ip:port"(需填协议)。</p>
</div>

<div id="tab-rules" class="tab-panel" style="display:none" role="tabpanel" aria-labelledby="tab-link-rules">
  <div class="panel-heading"><div><span class="panel-kicker">Traffic Routing</span><h2>分流规则</h2><p>规则自上而下匹配；越具体的域名规则应越靠前。</p></div></div>
  <div class="preset-bar">
    <b style="color:#e2e8f0">一键 GFW 分流</b>
    <span>国内域名/内网 直连(DIRECT),其余走代理(ANY);会覆盖当前规则。</span>
    <button type="button" class="btn" onclick="if(confirm('用 GFW 分流预设覆盖当前所有规则?'))postJSON('/api/rules/preset-gfw',{},reloadOrAlert)">启用 GFW 分流</button>
  </div>
  <div class="table-scroll" tabindex="0" aria-label="规则表格，可横向滚动">
  <table class="management-table">
  <caption class="sr-only">分流规则管理</caption>
  <tr><th>#</th><th>类型</th><th>值</th><th>目标分组</th><th>操作</th></tr>
  {{range $i, $r := .Rules}}
  <tr>
    <td data-label="优先级">{{$i}}</td>
    <td data-label="规则类型">{{$r.Type}}</td>
    <td data-label="匹配值" class="mono">{{if eq $r.Type "MATCH"}}*{{else}}{{$r.Value}}{{end}}</td>
    <td data-label="目标分组">{{$r.Group}}</td>
    <td data-label="操作">
      {{if ne $r.Type "MATCH"}}
      <button type="button" class="btn-sm" aria-label="上移规则" onclick="postJSON('/api/rules/move',{id:'{{$r.ID}}',delta:-1},reloadOrAlert)">↑</button>
      <button type="button" class="btn-sm" aria-label="下移规则" onclick="postJSON('/api/rules/move',{id:'{{$r.ID}}',delta:1},reloadOrAlert)">↓</button>
      <button type="button" class="btn-sm danger" onclick="if(confirm('删除规则?'))postJSON('/api/rules/delete',{id:'{{$r.ID}}'},reloadOrAlert)">删除</button>
      {{else}}
      <span class="note-inline">兜底规则,不可删除/移动,可在下方修改默认分组</span>
      {{end}}
    </td>
  </tr>
  {{end}}
  </table>
  </div>

  <form class="inline" id="form-add-rule">
    <div class="form-title">添加分流规则<span>命中后将连接交给指定分组或国家节点</span></div>
    <label class="sr-only" for="rule-type">规则类型</label>
    <select id="rule-type" name="type">{{range .RuleTypes}}<option value="{{.}}">{{.}}</option>{{end}}</select>
    <label class="sr-only" for="rule-value">规则值</label>
    <input id="rule-value" name="value" placeholder="值,如 netflix.com / 10.0.0.0/8 / cn / gfw">
    <label class="sr-only" for="rule-target-select">目标分组</label>
    <select name="group" id="rule-target-select">{{range .GroupOptions}}<option value="{{.}}">{{.}}</option>{{end}}</select>
    <button class="btn" type="submit">添加规则</button>
  </form>
  <p class="note">规则按从上到下的顺序匹配目标域名/IP,命中即用对应目标转发;DOMAIN/DOMAIN-SUFFIX/DOMAIN-KEYWORD 匹配域名,IP-CIDR 匹配字面 IP 目标,GEOSITE 值填 <b>cn</b>(内置中国常用域名)或 <b>gfw</b>(内置常见被墙域名)。<b>目标可直接选国家</b>(列表里的 <span class="mono">COUNTRY:US</span> / <span class="mono">COUNTRY:JP</span> 等,表示"该国任意节点,自动挑最快的一个"),无需先建分组。例如:<span class="mono">DOMAIN-SUFFIX com → COUNTRY:US</span>,再把 <span class="mono">DOMAIN 111.com → COUNTRY:JP</span> 拖到它上面(越靠上越优先),就能实现"*.com 走美国、111.com 走日本"。若某国当前无可用节点,会自动回退到 ANY。最下面的 MATCH 是兜底规则,始终存在。</p>

  <div class="default-group-editor">
    <label for="default-group-select">默认(兜底)分组:</label>
    <select id="default-group-select" data-default="{{.DefaultGroup}}">{{range .GroupOptions}}<option value="{{.}}" {{if eq . $.DefaultGroup}}selected{{end}}>{{.}}</option>{{end}}</select>
    <button type="button" class="btn-sm" onclick="postJSON('/api/rules/default',{group:document.getElementById('default-group-select').value},reloadOrAlert)">保存</button>
  </div>
</div>

<div id="tab-groups" class="tab-panel" style="display:none" role="tabpanel" aria-labelledby="tab-link-groups">
  <div class="panel-heading"><div><span class="panel-kicker">Routing Groups</span><h2>分组策略</h2><p>组合国家、协议、来源和固定节点，控制连接选择方式。</p></div></div>
  <div class="scope-intro"><span class="scope-icon" aria-hidden="true">◇</span><div><strong>分组连接规则与可用节点</strong>规则指向分组，分组再按 sticky、延迟、速度、轮询或随机策略选择当前节点。</div></div>
  <div class="table-scroll" tabindex="0" aria-label="分组表格，可横向滚动">
  <table class="management-table">
  <caption class="sr-only">代理分组管理</caption>
  <tr><th>名称</th><th>类型</th><th>策略</th><th>过滤条件</th><th>成员数/当前</th><th>操作</th></tr>
  {{range .Groups}}
  {{if .ID}}
  <tr>
    <td data-label="分组名称">{{.Name}}</td>
    <td data-label="类型" class="small">自定义</td>
    <td data-label="选择策略">
      <select aria-label="{{.Name}} 的策略" onchange="postJSON('/api/groups/strategy',{id:'{{.ID}}',strategy:this.value},reloadOrAlert)">
        {{$cur := .Strategy}}
        {{range $.Strategies}}<option value="{{.}}" {{if eq . $cur}}selected{{end}}>{{.}}</option>{{end}}
      </select>
    </td>
    <td data-label="过滤条件" class="small">{{if .Nodes}}指定节点: {{range .Nodes}}{{.}} {{end}}<br>{{end}}{{if .Countries}}国家: {{range .Countries}}{{.}} {{end}}<br>{{end}}{{if .Protocols}}协议: {{range .Protocols}}{{.}} {{end}}<br>{{end}}{{if .Sources}}来源: {{range .Sources}}{{.}} {{end}}{{end}}</td>
    <td data-label="成员 / 当前节点">{{.Count}} / {{if .Current}}{{.Current}}{{if .Dynamic}} (每连接轮换){{end}}{{else}}-{{end}}</td>
    <td data-label="操作"><button type="button" class="btn-sm danger" onclick="if(confirm('删除分组 {{.Name}}? 引用它的规则会自动回退到 ANY'))postJSON('/api/groups/delete',{id:'{{.ID}}'},reloadOrAlert)">删除</button></td>
  </tr>
  {{end}}
  {{end}}
  </table>
  </div>

  <form class="inline" id="form-add-group">
    <div class="form-title">创建路由分组<span>筛选条件可组合；留空代表该维度不限制</span></div>
    <label class="sr-only" for="group-name">分组名称</label>
    <input id="group-name" name="name" placeholder="分组名称" required>
    <label class="sr-only" for="group-strategy">分组策略</label>
    <select id="group-strategy" name="strategy">{{range .Strategies}}<option value="{{.}}">{{.}}</option>{{end}}</select>
    <label class="sr-only" for="group-nodes">指定节点</label>
    <input id="group-nodes" name="nodes" placeholder="精确钉死用 protocol://ip:port；ip:port 匹配全部协议，逗号分隔">
    <label class="sr-only" for="group-countries">国家筛选</label>
    <input id="group-countries" name="countries" placeholder="国家代码,逗号分隔,如 US,JP (留空=不限)">
    <label class="sr-only" for="group-protocols">协议筛选</label>
    <input id="group-protocols" name="protocols" placeholder="协议,逗号分隔,如 socks5,http (留空=不限)">
    <label class="sr-only" for="group-sources">来源筛选</label>
    <input id="group-sources" name="sources" placeholder="来源名称,逗号分隔 (留空=不限)">
    <button class="btn" type="submit">创建分组</button>
  </form>
  <p class="note">分组是从代理池里筛出的节点子集,配合分流规则使用。<b>要把某个域名精确固定走一个协议节点</b>:在"指定节点"里填 <code>protocol://ip:port</code>(例如 <code>socks5://1.2.3.4:1080</code>),建一个分组,再在分流规则里把该域名指向这个分组即可。裸 <code>ip:port</code> 是旧兼容写法,会匹配该地址的全部协议变体。筛选条件可组合(指定节点 / 国家 / 协议 / 来源)。策略: sticky=固定直到手动切换或失败, round-robin=每次新连接轮换, random=随机, latency=优先延迟最低, speed=优先测速结果最高(需先手动测速)。</p>
</div>

</div>
</div>
</main>
</div>
<div id="toast-region" class="toast-region" role="status" aria-live="polite" aria-atomic="true"></div>
<div id="result-overlay" class="result-overlay" role="dialog" aria-modal="true" aria-labelledby="result-dialog-title" hidden onclick="resultDialogBackdrop(event)">
  <div class="result-dialog">
    <div class="result-dialog-head"><h2 id="result-dialog-title">操作结果</h2><button type="button" class="result-dialog-close" onclick="closeResultDialog()" aria-label="关闭">×</button></div>
    <div id="result-dialog-body" class="result-dialog-body"></div>
    <div class="result-dialog-foot"><button type="button" class="btn" onclick="closeResultDialog()">知道了</button></div>
  </div>
</div>
<script>
function fetchJSON(url, options) {
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
function showResultDialog(title, message) {
  var overlay = document.getElementById('result-overlay');
  if (!overlay) { alert(message); return; }
  resultDialogFocus = document.activeElement;
  setText('result-dialog-title', title || '操作结果');
  setText('result-dialog-body', message || '');
  overlay.hidden = false;
  document.body.classList.add('modal-open');
  var close = overlay.querySelector('.result-dialog-close');
  if (close) close.focus();
}

function closeResultDialog() {
  var overlay = document.getElementById('result-overlay');
  if (!overlay || overlay.hidden) return;
  overlay.hidden = true;
  document.body.classList.remove('modal-open');
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
var nodeSnapshotID = '';
var anyPinned = false;
var nodesLoaded = false;
var currentTab = 'nodes';
var statusRequest = null;
var nodesRequest = null;
var nodesAbortController = null;
var pollTimer = null;
var refreshPollTimer = null;
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
    {value:'proxyip', label:'ProxyIP', note:'Worker 外部反代资源 · 不参与转发'}
  ];
  container.innerHTML = cards.map(function(card) {
    var count = candidateProtocolCount(card.value);
    return '<button type="button" class="protocol-card' + (selected === card.value ? ' active' : '') + '" onclick="chooseCandidateProtocol(\'' + card.value + '\')" aria-pressed="' + (selected === card.value ? 'true' : 'false') + '">' +
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
    return '<button type="button" class="continent-tile continent-' + item.cls + (candidateContinentFilter === item.code ? ' active' : '') + '" onclick="setCandidateContinentFilter(\'' + item.code + '\')">' +
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
      html += '<button type="button" class="country-option' + (selected === item.country ? ' active' : '') + '" onclick="chooseCandidateCountry(\'' + item.country + '\')">' +
        '<span aria-hidden="true">' + flagEmoji(item.country) + '</span><span class="country-option-code">' + item.country + ' ' + escapeHtml(countryNameZH(item.country)) + '</span><span class="country-option-count">' + pickerCountLabel(item) + '</span></button>';
    });
    html += '</div>';
  });
  var unknown = pickerUnknownCountryCounts();
  if ((!candidateContinentFilter || candidateContinentFilter === 'unknown') && (!query || 'UNKNOWN 国家未知 尚未定位'.indexOf(query) >= 0)) {
    shown++;
    html += '<div class="country-continent-group"><div class="country-continent-title"><span>🏳️ 国家未知</span><span>' + pickerCountLabel(unknown) + '</span></div>' +
      '<button type="button" class="country-option' + (selected === '__unknown__' ? ' active' : '') + '" onclick="chooseCandidateCountry(\'__unknown__\')"><span aria-hidden="true">🏳️</span><span class="country-option-code">尚未定位</span><span class="country-option-count">' + pickerCountLabel(unknown) + '</span></button></div>';
  }
  list.innerHTML = html || '<div class="country-option-empty">没有匹配的国家/地区</div>';
  setText('candidate-country-result-count', shown + ' 个地区');
}

function updateCandidateCountryButton() {
  var value = String((document.getElementById('cf-country') || {}).value || '');
  var button = document.getElementById('cf-country-button');
  if (!button) return;
  button.textContent = value === '__unknown__' ? '🏳️ 国家未知' : (value ? countryLabel(value) : '🗺️ 全部国家/地区');
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
    if (title) title.textContent = '🗺️ 按来源标注地区浏览全部候选';
    if (mapTitle) mapTitle.textContent = '每个数量都是完整候选快照中的库存条数';
    if (note) note.textContent = '候选地区来自来源元数据，并非全部经过真实出口验证；国家未知表示来源未标注。';
    if (allButton) allButton.textContent = '全部来源地区';
  }
  renderCandidateCountryPicker();
  modal.hidden = false;
  document.body.classList.add('modal-open');
  if (search) setTimeout(function(){ search.focus(); }, 0);
}

function closeCandidateCountryPicker() {
  var modal = document.getElementById('candidate-country-modal');
  if (!modal || modal.hidden) return;
  modal.hidden = true;
  document.body.classList.remove('modal-open');
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
  var button = '<button type="button" class="btn-sm" data-action="proxyip-verify" onclick="runProxyIPVerify(this)" aria-label="' + buttonLabel + ' ' + safeKey + '">' + buttonLabel + '</button>';
  if (!result) return '<div class="proxyip-verify"><div class="proxyip-verify-actions">' + button + '</div>' + note + '</div>';
  if (result.state === 'loading') {
    return '<div class="proxyip-verify"><div class="proxyip-verify-actions"><button type="button" class="btn-sm" data-action="proxyip-verify" disabled>验证中…</button>' +
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
    if (cell) cell.innerHTML = proxyIPVerifyCellHTML(key, 'proxyip');
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
  candidatePageSize = compactViewport() && !candidatePageSizeTouched ? Math.min(10, returnedPageSize) : returnedPageSize;
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

function applyCandidateView() {
  var tbody = document.getElementById('candidate-tbody');
  var pager = document.getElementById('candidate-pager');
  var topPager = document.getElementById('candidate-pager-top');
  if (!tbody) return;
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
  setText('tab-link-candidates', '候选目录 (' + formatCount(catalogTotal) + ')');
  setText('candidate-matching', formatCount(total));
  setText('stat-matching', formatCount(total));
  setText('candidate-known', formatCount(known));
  setText('candidate-deferred', formatCount(deferred));
  setText('candidate-country-unknown', formatCount(candidateUnknownCountryTotal()));
  var updated = formatCandidateUpdatedAt(data.updated_at);
  var phaseLabels = {checking:'检查中', complete:'已完成', partial:'部分来源失败（已保留旧目录）', loading:'生成中'};
  var phase = data.phase ? (' · 快照' + (phaseLabels[data.phase] || data.phase)) : '';
  setText('candidate-count', (total ? ('显示 ' + formatCount(start + 1) + '-' + formatCount(start + rows.length) + ' · 匹配 ' + formatCount(total)) : '匹配 0') + ' · 完整目录 ' + formatCount(catalogTotal) + ' · 最近失败 ' + formatCount(failed) + (policyFiltered ? ' · 策略排除 ' + formatCount(policyFiltered) : '') + phase + (updated ? ' · 更新于 ' + updated : ''));

  if (!catalogTotal) {
    tbody.innerHTML = '<tr><td colspan="6" class="empty">完整候选快照尚未生成，请点击页面上方“刷新代理池”后再查看。</td></tr>';
    renderCandidatePagers('');
    return;
  }
  if (!total) {
    tbody.innerHTML = '<tr><td colspan="6" class="empty">没有符合当前筛选条件的候选</td></tr>';
    renderCandidatePagers('');
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
      '<td data-label="候选地址" class="mono">' + escapeHtml(candidate.addr || '') + '<button type="button" class="copy-btn" data-copy-address="' + escapeHtml(candidate.addr || '') + '" onclick="copyAddrFrom(this)" aria-label="复制候选地址">复制</button><button type="button" class="mobile-detail-toggle" onclick="toggleCandidateDetails(this)" aria-expanded="' + (candidateExpanded ? 'true' : 'false') + '">' + (candidateExpanded ? '收起' : '详情') + '</button></td>' +
      '<td data-label="来源标注地区">' + escapeHtml(location) + '</td>' +
      '<td data-label="来源" class="small mobile-secondary">' + escapeHtml(sources) + '<span class="candidate-readonly"> · 只读候选</span></td>' +
      '<td data-label="专用验证" class="candidate-verify-cell mobile-secondary">' + proxyIPVerifyCellHTML(candidate.key, candidate.protocol) + '</td></tr>';
  }).join('');

  if (total <= pageSize) {
    renderCandidatePagers('');
  } else {
    renderCandidatePagers(
      '<button type="button" class="btn-sm" ' + (page <= 1 ? 'disabled' : '') + ' onclick="gotoCandidatePage(' + (page - 1) + ')">上一页</button>' +
      '<span class="small">第 ' + page + ' / ' + pageCount + ' 页</span>' +
      '<button type="button" class="btn-sm" ' + (page >= pageCount ? 'disabled' : '') + ' onclick="gotoCandidatePage(' + (page + 1) + ')">下一页</button>');
  }
}

function onNodePageFetched(pageData) {
  nodePageData = pageData && typeof pageData === 'object' ? pageData : {};
  if (!Array.isArray(nodePageData.nodes)) nodePageData.nodes = [];
  if (!Array.isArray(nodePageData.countries)) nodePageData.countries = [];
  nodePage = Number(nodePageData.page) > 0 ? Number(nodePageData.page) : 1;
  nodeSnapshotID = String(nodePageData.snapshot_id || '');
  nodePageSize = Number(nodePageData.page_size) > 0 ? Number(nodePageData.page_size) : nodePageSize;
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
  if (el.closest && el.closest('#node-pager') && el.getAttribute('data-action')) {
    return {pager:el.getAttribute('data-action')};
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
    el = document.querySelector('#node-pager [data-action="' + saved.pager + '"]');
  }
  if (el && !el.disabled) el.focus();
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
  setText('tab-link-nodes', '代理池 (' + formatCount(poolTotal) + ')');
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
      var actionsCell =
        '<div class="row-actions"><button type="button" class="btn-sm" data-action="switch" onclick="switchNode(this)" aria-label="使用节点 ' + escapeHtml(n.addr) + '">使用</button>' +
        (ops.speedtest
          ? '<button type="button" class="btn-sm" data-action="speedtest" disabled>测速中...</button>'
          : '<button type="button" class="btn-sm" data-action="speedtest" onclick="runSpeedtest(this)" aria-label="测速节点 ' + escapeHtml(n.addr) + '">测速</button>') +
        (ops.verify
          ? '<button type="button" class="btn-sm" data-action="verify" disabled>验证中...</button>'
          : '<button type="button" class="btn-sm" data-action="verify" onclick="runVerify(this)" title="立即重新拨号,查看真实出口IP/国家是否和标签一致" aria-label="验证节点 ' + escapeHtml(n.addr) + '">验证</button>') +
        '<button type="button" class="mobile-detail-toggle" data-action="details" onclick="toggleNodeDetails(this)" aria-expanded="' + (rowExpanded ? 'true' : 'false') + '">' + (rowExpanded ? '收起' : '详情') + '</button></div>';
      html += '<tr class="' + (n.active ? 'active ' : '') + (n.available === false ? 'unavail ' : '') + (rowExpanded ? 'mobile-expanded' : '') + '" data-key="' + escapeHtml(n.key) + '">' +
        '<td data-label="状态">' + (n.active ? '<span class="badge-inuse">使用中</span>' : (n.available === false ? '<span class="badge-unavail">不可用</span>' : '<span class="small">可用</span>')) + '</td>' +
        '<td data-label="协议">' + protoBadge(n.protocol) + '</td>' +
        '<td data-label="地址(节点IP)" class="mono">' + escapeHtml(n.addr) + '<button type="button" class="copy-btn" data-action="copy" data-copy-address="' + escapeHtml(n.addr) + '" onclick="copyAddrFrom(this)" aria-label="复制节点地址">复制</button></td>' +
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
          '<button type="button" class="btn-sm" data-action="previous" ' + (page <= 1 ? 'disabled' : '') + ' onclick="gotoPage(' + (page - 1) + ')">上一页</button>' +
          '<span class="small">第 ' + page + ' / ' + pageCount + ' 页</span>' +
          '<button type="button" class="btn-sm" data-action="next" ' + (page >= pageCount ? 'disabled' : '') + ' onclick="gotoPage(' + (page + 1) + ')">下一页</button>');
    }
  }

  if (banner) {
    var lockUI = anyPinned
      ? '<span class="lock-badge">🔒 手动锁定</span><button type="button" class="btn-sm" onclick="setAuto()">恢复自动轮换</button>'
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

function applyStatusSummary(d) {
  if (!d || typeof d !== 'object') return;
  var pageData = nodePageData || {};
  var total = typeof d.total === 'number' ? d.total : (nodesLoaded ? pageData.pool_total : null);
  var available = typeof d.available_total === 'number' ? d.available_total : (nodesLoaded ? pageData.available_total : null);
  var unavailable = typeof d.unavailable_total === 'number' ? d.unavailable_total : (nodesLoaded ? pageData.unavailable_total : null);
  updateTopCounts(total, available, unavailable);
  if (typeof d.proxyip_total === 'number') setText('stat-proxyip', d.proxyip_total);
  setText('stat-last', d.last_scrape || 'N/A');
  setText('stat-next', d.next_scrape || 'N/A');
  setText('timeline-last', d.last_scrape || '尚未刷新');
  setText('timeline-next', d.next_scrape || '等待调度');
  lastKnownScrape = d.last_scrape || '';
  lastKnownNextScrape = d.next_scrape || '';

  var scrapeEl = document.getElementById('scrape-flow');
  if (scrapeEl && d.scrape && typeof d.scrape === 'object') {
    setText('scrape-raw', formatCount(typeof d.scrape.raw === 'number' ? d.scrape.raw : 0));
    setText('scrape-candidates', formatCount(typeof d.scrape.candidates === 'number' ? d.scrape.candidates : 0));
    if (!candidatesLoaded && typeof d.scrape.candidates === 'number') setText('tab-link-candidates', '候选目录 (' + formatCount(d.scrape.candidates) + ')');
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
    .then(function(d){ applyStatusSummary(d); return d; })
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

function waitForRefresh(beforeLast, beforeNext, btn, orig, startedAt, jobID) {
  if (refreshPollTimer) clearTimeout(refreshPollTimer);
  refreshPollTimer = setTimeout(function checkRefreshStatus() {
    var jobRequest = jobID ? fetchJSON('/api/refresh/status').catch(function(){ return null; }) : Promise.resolve(null);
    Promise.all([requestStatus(), jobRequest]).then(function(results) {
      var d = results[0] || {};
      var operation = refreshJobFromState(results[1], jobID);
      var last = d.last_scrape || '';
      var next = d.next_scrape || '';
      var operationDone = operation && ['complete','partial','skipped','failed'].indexOf(operation.status) >= 0;
      var completed = operationDone || (!!last && last !== beforeLast) || (!!next && next !== beforeNext && !!last);
      if (completed) {
        btn.disabled = false;
        btn.textContent = orig;
        var statusEl = document.getElementById('refresh-status');
        var partial = operation && operation.status === 'partial';
        if (statusEl) statusEl.textContent = partial ? '刷新完成；部分来源失败，旧候选已保留。' : '刷新完成，节点状态已更新。';
        notify(partial ? '刷新完成，部分来源暂时失败' : '代理池刷新完成', partial ? '' : 'success');
        requestCurrentCatalog(true);
        setTimeout(function(){ if (statusEl) statusEl.textContent = ''; }, 8000);
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

function saveCheckURL() {
  var input = document.getElementById('check-url-input');
  var statusEl = document.getElementById('check-url-status');
  var url = (input.value || '').trim();
  if (!url) { notify('请输入一个 http:// 或 https:// 开头的网址', 'error'); return; }
  postJSON('/api/settings/check-url', {url: url}, function(err) {
    if (err) { notify(err, 'error', 7000); return; }
    if (statusEl) {
      statusEl.textContent = '已保存,正在按新标准重新检测全部节点...';
      setTimeout(function(){ statusEl.textContent = ''; }, 8000);
    }
  });
}

function runSpeedtest(btn) {
  var key = rowKey(btn);
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
    nodes: ['节点运行中心','查看健康状态、真实出口与路由节点，所有列表均由服务端分页。'],
    candidates: ['候选资源目录','浏览全部来源快照，并按协议、国家、来源和状态快速缩小范围。'],
    sources: ['订阅来源管理','控制抓取入口、格式和启用状态。'],
    rules: ['流量分流规则','按从上到下的顺序构建可读、可预测的路由决策。'],
    groups: ['路由分组策略','用节点、国家、协议和来源组合可复用的连接策略。']
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
  if (e.key === 'Escape') { closeCandidateCountryPicker(); closeResultDialog(); }
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
    allow_private: !!f.allow_private.checked
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
</script>
</body>
</html>`
