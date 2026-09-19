// Package dashboard — embedded single-file web panel.
package dashboard

// indexHTML is served verbatim for "/" and "/dashboard".
//
// It is intentionally a single self-contained file with no build step, no CDN
// and no external assets, so the panel works offline and inside restricted
// networks. Live updates arrive over SSE from /api/stream.
const indexHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>LLM Gateway · Token 用量</title>
<style>
  :root {
    --bg:#0f1115; --panel:#171a21; --panel2:#1e222b; --line:#272c37;
    --fg:#e6e9ef; --muted:#8b93a7; --accent:#4f8cff; --ok:#3fb950;
    --warn:#d29922; --err:#f85149; --prompt:#4f8cff; --completion:#3fb950;
  }
  * { box-sizing:border-box; }
  body {
    margin:0; background:var(--bg); color:var(--fg);
    font:14px/1.5 ui-sans-serif,-apple-system,"Segoe UI",Roboto,"Helvetica Neue",Arial,"PingFang SC","Microsoft YaHei",sans-serif;
  }
  header {
    display:flex; align-items:center; gap:12px; flex-wrap:wrap;
    padding:14px 20px; border-bottom:1px solid var(--line); background:var(--panel);
    position:sticky; top:0; z-index:10;
  }
  header h1 { font-size:15px; margin:0; font-weight:600; letter-spacing:.2px; }
  .spacer { flex:1; }
  .pill {
    display:inline-flex; align-items:center; gap:6px; padding:3px 10px;
    border:1px solid var(--line); border-radius:999px; color:var(--muted); font-size:12px;
  }
  .dot { width:7px; height:7px; border-radius:50%; background:var(--muted); }
  .dot.live { background:var(--ok); box-shadow:0 0 0 3px rgba(63,185,80,.15); }
  .dot.down { background:var(--err); }
  main { padding:20px; max-width:1400px; margin:0 auto; }
  .grid { display:grid; gap:14px; grid-template-columns:repeat(auto-fit,minmax(210px,1fr)); }
  .card {
    background:var(--panel); border:1px solid var(--line); border-radius:12px; padding:16px 18px;
  }
  .card .label { color:var(--muted); font-size:12px; text-transform:uppercase; letter-spacing:.6px; }
  .card .value { font-size:30px; font-weight:650; margin-top:8px; font-variant-numeric:tabular-nums; }
  .card .sub { color:var(--muted); font-size:12px; margin-top:6px; }
  .split { display:grid; gap:14px; grid-template-columns:1fr 1fr; margin-top:14px; }
  @media (max-width:900px){ .split { grid-template-columns:1fr; } }
  h2 { font-size:13px; margin:0; padding:14px 18px; border-bottom:1px solid var(--line);
       color:var(--muted); text-transform:uppercase; letter-spacing:.6px; font-weight:600; }
  .card.flush { padding:0; overflow:hidden; }
  table { width:100%; border-collapse:collapse; font-variant-numeric:tabular-nums; }
  th,td { text-align:right; padding:9px 14px; border-bottom:1px solid var(--line); white-space:nowrap; }
  th:first-child, td:first-child { text-align:left; }
  th { color:var(--muted); font-weight:500; font-size:12px; }
  tbody tr:last-child td { border-bottom:none; }
  tbody tr:hover { background:var(--panel2); }
  .scroll { max-height:420px; overflow:auto; }
  .empty { padding:26px 18px; color:var(--muted); text-align:center; }
  code, .mono { font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace; font-size:12px; }
  .tag { padding:2px 7px; border-radius:5px; font-size:11px; border:1px solid var(--line); color:var(--muted); }
  .tag.s { color:var(--accent); border-color:rgba(79,140,255,.4); }
  .tag.e { color:var(--err); border-color:rgba(248,81,73,.4); }
  .tag.n { color:var(--warn); border-color:rgba(210,153,34,.4); }
  .bar { height:5px; border-radius:3px; background:var(--line); overflow:hidden; display:flex; margin-top:7px; }
  .bar i { display:block; height:100%; }
  footer { color:var(--muted); font-size:12px; padding:18px 20px 34px; text-align:center; }
  a { color:var(--accent); }
</style>
</head>
<body>
<header>
  <h1>LLM Gateway · 实时 Token 用量</h1>
  <span class="pill" id="conn"><span class="dot" id="connDot"></span><span id="connText">连接中…</span></span>
  <span class="spacer"></span>
  <span class="pill" id="upstream">—</span>
  <span class="pill">运行 <span id="uptime" class="mono">0s</span></span>
  <span class="pill">并发 <span id="inflight" class="mono">0</span></span>
</header>

<main>
  <div class="grid">
    <div class="card">
      <div class="label">总 Token</div>
      <div class="value" id="totalTokens">0</div>
      <div class="bar">
        <i id="barPrompt" style="background:var(--prompt);width:0%"></i>
        <i id="barCompletion" style="background:var(--completion);width:0%"></i>
      </div>
      <div class="sub"><span id="promptTokens">0</span> 输入 · <span id="completionTokens">0</span> 输出</div>
    </div>
    <div class="card">
      <div class="label">请求数</div>
      <div class="value" id="requests">0</div>
      <div class="sub">错误 <span id="errors">0</span> · 速率 <span id="rpm">0</span>/min</div>
    </div>
    <div class="card">
      <div class="label">Token 速率</div>
      <div class="value" id="tpm">0</div>
      <div class="sub">tokens / min（进程生命周期均值）</div>
    </div>
    <div class="card">
      <div class="label">缓存命中</div>
      <div class="value" id="cachedTokens">0</div>
      <div class="sub">命中率 <span id="cacheRate">—</span> · 推理 <span id="reasoningTokens">0</span></div>
    </div>
  </div>

  <div class="split">
    <div class="card flush">
      <h2>按模型统计</h2>
      <div class="scroll">
        <table>
          <thead><tr>
            <th>模型</th><th>请求</th><th>输入</th><th>输出</th><th>合计</th><th>平均耗时</th>
          </tr></thead>
          <tbody id="modelRows"><tr><td colspan="6" class="empty">暂无数据</td></tr></tbody>
        </table>
      </div>
    </div>

    <div class="card flush">
      <h2>按使用者统计</h2>
      <div class="scroll">
        <table>
          <thead><tr>
            <th>使用者</th><th>请求</th><th>输入</th><th>输出</th><th>合计</th><th>错误</th>
          </tr></thead>
          <tbody id="clientRows"><tr><td colspan="6" class="empty">暂无数据</td></tr></tbody>
        </table>
      </div>
    </div>
  </div>

  <div class="split">
    <div class="card flush">
      <h2>按上游统计</h2>
      <div class="scroll">
        <table>
          <thead><tr>
            <th>上游</th><th>请求</th><th>输入</th><th>输出</th><th>合计</th><th>错误</th>
          </tr></thead>
          <tbody id="upstreamRows"><tr><td colspan="6" class="empty">暂无数据</td></tr></tbody>
        </table>
      </div>
    </div>

    <div class="card flush">
      <h2>实时请求流</h2>
      <div class="scroll">
        <table>
          <thead><tr>
            <th>时间</th><th>使用者</th><th>模型</th><th>状态</th><th>输入</th><th>输出</th><th>合计</th><th>耗时</th>
          </tr></thead>
          <tbody id="recentRows"><tr><td colspan="8" class="empty">暂无数据</td></tr></tbody>
        </table>
      </div>
    </div>
  </div>
</main>

<footer>
  SSE 实时推送 · <code>/api/stats</code> JSON · <code>/metrics</code> Prometheus · 上游 <span id="upFooter" class="mono">—</span>
</footer>

<script>
(function () {
  var $ = function (id) { return document.getElementById(id); };

  function fmt(n) {
    n = Number(n) || 0;
    return n.toLocaleString('en-US');
  }

  function fmtDuration(sec) {
    sec = Math.floor(Number(sec) || 0);
    var d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600),
        m = Math.floor(sec % 3600 / 60), s = sec % 60;
    if (d) return d + 'd ' + h + 'h';
    if (h) return h + 'h ' + m + 'm';
    if (m) return m + 'm ' + s + 's';
    return s + 's';
  }

  function timeOf(iso) {
    var d = new Date(iso);
    if (isNaN(d.getTime())) return '—';
    return d.toLocaleTimeString('zh-CN', { hour12: false });
  }

  function rate(part, whole) {
    whole = Number(whole) || 0;
    if (whole <= 0) return '—';
    return (Number(part) / whole * 100).toFixed(1) + '%';
  }

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  function render(s) {
    var prompt = Number(s.prompt_tokens) || 0;
    var completion = Number(s.completion_tokens) || 0;
    var total = Number(s.total_tokens) || 0;
    var denom = prompt + completion;

    $('totalTokens').textContent = fmt(total);
    $('promptTokens').textContent = fmt(prompt);
    $('completionTokens').textContent = fmt(completion);
    $('barPrompt').style.width = (denom ? prompt / denom * 100 : 0) + '%';
    $('barCompletion').style.width = (denom ? completion / denom * 100 : 0) + '%';

    $('requests').textContent = fmt(s.requests);
    $('errors').textContent = fmt(s.errors);
    $('rpm').textContent = (Number(s.requests_per_minute) || 0).toFixed(2);
    $('tpm').textContent = fmt(Math.round(Number(s.tokens_per_minute) || 0));
    $('cachedTokens').textContent = fmt(s.cached_tokens);
    $('cacheRate').textContent = rate(s.cached_tokens, prompt);
    $('reasoningTokens').textContent = fmt(s.reasoning_tokens);
    $('uptime').textContent = fmtDuration(s.uptime_seconds);
    $('inflight').textContent = fmt(s.in_flight);

    var models = s.models || [];
    if (!models.length) {
      $('modelRows').innerHTML = '<tr><td colspan="6" class="empty">暂无数据</td></tr>';
    } else {
      $('modelRows').innerHTML = models.map(function (m) {
        return '<tr>' +
          '<td>' + esc(m.model) + '</td>' +
          '<td>' + fmt(m.requests) + '</td>' +
          '<td>' + fmt(m.prompt_tokens) + '</td>' +
          '<td>' + fmt(m.completion_tokens) + '</td>' +
          '<td><strong>' + fmt(m.total_tokens) + '</strong></td>' +
          '<td>' + fmt(m.avg_duration_ms) + ' ms</td>' +
        '</tr>';
      }).join('');
    }

    var recent = s.recent || [];
    if (!recent.length) {
      $('recentRows').innerHTML = '<tr><td colspan="8" class="empty">暂无数据</td></tr>';
    } else {
      $('recentRows').innerHTML = recent.map(function (r) {
        var cls = r.status_code >= 400 ? 'tag e' : 'tag';
        var stamp = r.usage_reported ? '' : ' <span class="tag n">无用量</span>';
        var stream = r.stream ? ' <span class="tag s">SSE</span>' : '';
        return '<tr>' +
          '<td class="mono">' + timeOf(r.time) + '</td>' +
          '<td>' + esc(r.client || 'anonymous') + '</td>' +
          '<td>' + esc(r.model || 'unknown') + '</td>' +
          '<td><span class="' + cls + '">' + r.status_code + '</span>' + stream + stamp + '</td>' +
          '<td>' + fmt(r.prompt_tokens) + '</td>' +
          '<td>' + fmt(r.completion_tokens) + '</td>' +
          '<td><strong>' + fmt(r.total_tokens) + '</strong></td>' +
          '<td>' + fmt(r.duration_ms) + ' ms</td>' +
        '</tr>';
      }).join('');
    }

    renderClients(s.clients || []);
    renderUpstreams(s.upstreams || []);
  }

  function renderClients(list) {
    if (!list.length) {
      $('clientRows').innerHTML = '<tr><td colspan="6" class="empty">暂无数据</td></tr>';
      return;
    }
    $('clientRows').innerHTML = list.map(function (c) {
      return '<tr>' +
        '<td>' + esc(c.client) + '</td>' +
        '<td>' + fmt(c.requests) + '</td>' +
        '<td>' + fmt(c.prompt_tokens) + '</td>' +
        '<td>' + fmt(c.completion_tokens) + '</td>' +
        '<td><strong>' + fmt(c.total_tokens) + '</strong></td>' +
        '<td>' + fmt(c.errors) + '</td>' +
      '</tr>';
    }).join('');
  }

  function renderUpstreams(list) {
    if (!list.length) {
      $('upstreamRows').innerHTML = '<tr><td colspan="6" class="empty">暂无数据</td></tr>';
      return;
    }
    $('upstreamRows').innerHTML = list.map(function (u) {
      return '<tr>' +
        '<td>' + esc(u.upstream) + '</td>' +
        '<td>' + fmt(u.requests) + '</td>' +
        '<td>' + fmt(u.prompt_tokens) + '</td>' +
        '<td>' + fmt(u.completion_tokens) + '</td>' +
        '<td><strong>' + fmt(u.total_tokens) + '</strong></td>' +
        '<td>' + fmt(u.errors) + '</td>' +
      '</tr>';
    }).join('');
  }

  // The dashboard key, when configured, must be carried on every dashboard
  // request. Read it once from the page URL (?key=...) and remember it so the
  // SSE connection, which cannot carry a header, still authenticates.
  var urlKey = new URLSearchParams(location.search).get('key') || '';
  function apiUrl(path) {
    return urlKey ? path + '?key=' + encodeURIComponent(urlKey) : path;
  }

  function setConn(state, text) {
    var dot = $('connDot');
    dot.className = 'dot ' + state;
    $('connText').textContent = text;
  }

  // Load static metadata once; it does not change while the process runs.
  fetch(apiUrl('/api/stats')).then(function (r) { return r.json(); }).then(function (d) {
    var ups = d.upstreams || [];
    var label = ups.length ? ups.join(', ') : '—';
    $('upstream').textContent = '上游 ' + label;
    $('upFooter').textContent = label;
    if (d.stats) render(d.stats);
  }).catch(function () {});

  // Live updates. EventSource reconnects automatically, so a dropped
  // connection recovers without any client-side retry logic.
  var es = new EventSource(apiUrl('/api/stream'));
  es.addEventListener('open', function () { setConn('live', '实时连接'); });
  es.addEventListener('stats', function (e) {
    try { render(JSON.parse(e.data)); } catch (err) {}
  });
  es.addEventListener('error', function () { setConn('down', '连接断开，重连中…'); });
})();
</script>
</body>
</html>
`
