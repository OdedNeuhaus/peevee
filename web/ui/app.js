'use strict';

/* peevee UI — no framework, no build step. The whole app is one state object,
   a fetch, and a render. That is enough for a table with filters, and it keeps
   the container image to a single static binary. */

const state = {
  filters: { cluster: [], namespace: [], storageClass: [], status: [] },
  search: '',
  atRisk: false,
  sort: 'usage',
  desc: false,
  // Rows per page. 0 means every matching row, which is what the table did
  // before and is still the right answer for a small fleet.
  pageSize: 50,
  offset: 0,
  data: null,
  clusters: null,
  config: null,
  thresholds: { warning: 75, critical: 90 },
  tab: 'volumes',
  configView: 'structured',
};

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

/* ── Formatting ─────────────────────────────────────────────── */

const UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];

function bytes(n) {
  if (n === null || n === undefined || Number.isNaN(n)) return '—';
  if (n === 0) return '0 B';
  const neg = n < 0;
  let v = Math.abs(n), i = 0;
  while (v >= 1024 && i < UNITS.length - 1) { v /= 1024; i++; }
  const digits = v < 10 && i > 0 ? 1 : 0;
  return `${neg ? '-' : ''}${v.toFixed(digits)} ${UNITS[i]}`;
}

function pct(v) {
  if (v === null || v === undefined || Number.isNaN(v)) return '—';
  return `${v < 10 ? v.toFixed(1) : Math.round(v)}%`;
}

function relTime(iso) {
  if (!iso) return 'never';
  const secs = (Date.now() - new Date(iso).getTime()) / 1000;
  if (secs < 5) return 'just now';
  if (secs < 60) return `${Math.round(secs)}s ago`;
  if (secs < 3600) return `${Math.round(secs / 60)}m ago`;
  if (secs < 86400) return `${Math.round(secs / 3600)}h ago`;
  return `${Math.round(secs / 86400)}d ago`;
}

// A projection is only useful if it is legible: "3 days" beats "3.2 days", and
// anything past a year is noise dressed up as precision.
function daysLabel(d) {
  if (d === null || d === undefined) return null;
  if (d < 1) return `${Math.max(1, Math.round(d * 24))}h`;
  if (d < 10) return `${d.toFixed(1)}d`;
  if (d < 365) return `${Math.round(d)}d`;
  return '>1y';
}

const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

/* ── Data loading ───────────────────────────────────────────── */

function queryString() {
  const p = new URLSearchParams();
  for (const [key, vals] of Object.entries(state.filters)) {
    if (vals.length) p.set(key, vals.join(','));
  }
  if (state.search) p.set('search', state.search);
  if (state.atRisk) p.set('atRisk', 'true');
  if (state.sort) p.set('sort', state.sort);
  if (state.desc) p.set('desc', 'true');
  return p.toString();
}

// The CSV export deliberately uses queryString() without these: exporting one
// page of a filtered view is never what anyone means by "export".
function pagedQueryString() {
  const p = new URLSearchParams(queryString());
  if (state.pageSize > 0) {
    p.set('limit', String(state.pageSize));
    if (state.offset > 0) p.set('offset', String(state.offset));
  }
  return p.toString();
}

// Every filter, search or sort change makes the current page meaningless —
// page 7 of the old result set has nothing to do with page 7 of the new one.
function reload() {
  state.offset = 0;
  return loadVolumes();
}

async function loadVolumes() {
  try {
    const res = await fetch(`/api/v1/volumes?${pagedQueryString()}`);
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    state.data = await res.json();
    renderVolumes();
    setLive('connected', `updated ${relTime(state.data.generatedAt)}`);
  } catch (err) {
    setLive('error', 'update failed');
    console.error('loadVolumes', err);
  }
  $('#exportBtn').href = `/api/v1/export.csv?${queryString()}`;
}

async function loadClusters() {
  const res = await fetch('/api/v1/clusters');
  state.clusters = await res.json();
  renderClusters();
}

// Fetched lazily: the YAML view is a second render of config already on the
// page, so there is no reason to pay for it unless someone asks to see it.
async function loadConfigYaml() {
  const pre = $('#configYaml');
  try {
    const res = await fetch('/api/v1/config.yaml');
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const text = await res.text();
    pre.dataset.raw = text;
    pre.innerHTML = `<code>${highlightYaml(text)}</code>`;
  } catch (err) {
    pre.innerHTML = '<code>Could not load the configuration as YAML.</code>';
    console.error('loadConfigYaml', err);
  }
}

// A deliberately small highlighter: comments, keys, and the scalar after a key.
// Anything more would mean shipping a parser to colour twenty lines of YAML.
function highlightYaml(text) {
  return text.split('\n').map((line) => {
    if (/^\s*#/.test(line)) return `<span class="c">${esc(line)}</span>`;
    const m = line.match(/^(\s*-?\s*)([\w.\-]+)(:)(.*)$/);
    if (!m) return esc(line);
    const [, indent, key, colon, rest] = m;
    const value = rest.trim() === '' ? esc(rest) : `<span class="v">${esc(rest)}</span>`;
    return `${esc(indent)}<span class="k">${esc(key)}</span>${colon}${value}`;
  }).join('\n');
}

function setConfigView(view) {
  state.configView = view;
  $$('.seg-btn').forEach((b) => b.classList.toggle('active', b.dataset.view === view));
  const yaml = view === 'yaml';
  $('#configGrid').hidden = yaml;
  $('#configYaml').hidden = !yaml;
  $('#copyYaml').hidden = !yaml;
  if (yaml && !$('#configYaml').dataset.raw) loadConfigYaml();
}

async function loadConfig() {
  const res = await fetch('/api/v1/config');
  state.config = await res.json();
  if (state.config.config?.ui?.thresholds) state.thresholds = state.config.config.ui.thresholds;
  const title = state.config.config?.ui?.title;
  if (title) {
    $('#brandName').textContent = title;
    document.title = title;
  }
  renderConfig();
  renderAtRiskTip();
}

/* ── Live indicator ─────────────────────────────────────────── */

function setLive(cls, text) {
  const el = $('#liveIndicator');
  el.className = `live ${cls}`;
  $('#liveText').textContent = text;
}

function pulseLive() {
  const el = $('#liveIndicator');
  el.classList.add('pulse');
  setTimeout(() => el.classList.remove('pulse'), 1200);
}

// The server pushes an event when a collection finishes, so the table refreshes
// on real new data rather than on a timer that is usually wrong.
function connectStream() {
  const es = new EventSource('/api/v1/stream');
  es.addEventListener('snapshot', () => {
    pulseLive();
    loadVolumes();
    if (state.tab === 'clusters') loadClusters();
  });
  es.onerror = () => setLive('error', 'reconnecting');
  es.onopen = () => setLive('connected', 'live');
}

/* ── Summary tiles ──────────────────────────────────────────── */

function renderTiles(t) {
  const tiles = [
    { label: 'Volumes', value: t.volumes.toLocaleString(),
      sub: `${t.withStats.toLocaleString()} reporting usage` },
    { label: 'Critical', value: t.critical.toLocaleString(), cls: t.critical ? 'crit' : '',
      sub: `at or above ${state.thresholds.critical}%` },
    { label: 'Warning', value: t.warning.toLocaleString(), cls: t.warning ? 'warn' : '',
      sub: `at or above ${state.thresholds.warning}%` },
    { label: 'Unmounted', value: t.unmounted.toLocaleString(),
      sub: 'no pod, so usage is unknown' },
    // Unreported claims are in use; they sit beside unmounted rather than in
    // it, because the unmounted tile is the one people act on by deleting.
    { label: 'Unreported', value: (t.unreported || 0).toLocaleString(),
      sub: 'mounted, but the driver reports nothing' },
  ];

  $('#tiles').innerHTML = tiles.map((tile) => `
    <div class="tile ${tile.cls || ''}">
      <div class="tile-label">${esc(tile.label)}</div>
      <div class="tile-value">${esc(tile.value)}</div>
      <div class="tile-sub">${esc(tile.sub)}</div>
      ${tile.meter !== undefined
        ? `<div class="tile-meter"><span style="width:${tile.meter}%;background:${tile.meterColor}"></span></div>`
        : ''}
    </div>`).join('');
}

/* ── Volume table ───────────────────────────────────────────── */

function sparkline(points) {
  if (!points || points.length < 2) return '';
  const w = 74, h = 22, pad = 2;
  const min = Math.min(...points), max = Math.max(...points);
  // A dead-flat series would divide by zero; give it a tiny span so it renders
  // as a straight line through the middle instead of vanishing.
  const span = max - min < 0.5 ? 0.5 : max - min;
  const step = w / (points.length - 1);
  const y = (v) => pad + (h - pad * 2) * (1 - (v - min) / span);

  const pts = points.map((v, i) => `${(i * step).toFixed(1)},${y(v).toFixed(1)}`);
  const line = `M${pts.join('L')}`;
  const area = `${line}L${w},${h}L0,${h}Z`;
  const rising = points[points.length - 1] > points[0];
  const color = rising ? 'var(--warn)' : 'var(--ok)';

  return `<svg class="spark" viewBox="0 0 ${w} ${h}" preserveAspectRatio="none" aria-hidden="true">
    <path class="spark-area" d="${area}" fill="${color}"/>
    <path d="${line}" stroke="${color}"/>
  </svg>`;
}

function renderVolumes() {
  const data = state.data;
  if (!data) return;

  renderTiles(data.totals);
  renderFacets(data.facets);

  const body = $('#volBody');
  const empty = $('#volEmpty');

  // Belt and braces: an empty page must render as an empty table, never throw.
  data.volumes = data.volumes || [];

  if (!data.volumes.length) {
    body.innerHTML = '';
    empty.hidden = false;
    empty.innerHTML = data.totals.volumes === 0 && !hasActiveFilters()
      ? `<strong>Nothing collected yet</strong>
         Either no cluster has been reached, or none of them has a PersistentVolumeClaim.
         Check the Clusters tab.`
      : `<strong>No volumes match these filters</strong>
         Try widening the search or clearing a filter.`;
  } else {
    empty.hidden = true;
    body.innerHTML = data.volumes.map(rowHTML).join('');
  }

  renderPager(data);

  const errCount = Object.keys(data.errors || {}).length;
  $('#footMeta').innerHTML = `
    Collected in ${data.durationMs}ms · ${relTime(data.generatedAt)}
    ${errCount ? ` · <span style="color:var(--crit)">${errCount} cluster${errCount > 1 ? 's' : ''} unreachable</span>` : ''}`;

  $('#clearFilters').hidden = !hasActiveFilters();
  $$('.vol-table th.sortable').forEach((th) => {
    th.classList.toggle('sorted', th.dataset.sort === state.sort);
    th.classList.toggle('asc', th.dataset.sort === state.sort && state.desc);
  });
}

// The table used to render every matching claim, which on a real fleet is a
// page you can scroll for a very long time without arriving anywhere.
function renderPager(data) {
  const size = state.pageSize;
  const matched = data.matched;

  // A background refresh can shrink the result set under us — claims come and
  // go — leaving the offset past the end and the table blank. Step back to the
  // last page that exists. The offset strictly decreases, so this cannot loop.
  if (size > 0 && state.offset > 0 && state.offset >= matched) {
    state.offset = Math.max(0, (Math.ceil(matched / size) - 1) * size);
    loadVolumes();
    return;
  }

  const shown = data.volumes.length;
  const from = shown === 0 ? 0 : state.offset + 1;
  const to = state.offset + shown;
  $('#rowRange').textContent = size > 0 && matched > shown
    ? `${from.toLocaleString()}–${to.toLocaleString()} of ${matched.toLocaleString()}`
    : `${matched.toLocaleString()} volume${matched === 1 ? '' : 's'}`;

  const pages = size > 0 ? Math.ceil(matched / size) : 1;
  const page = size > 0 ? Math.floor(state.offset / size) + 1 : 1;
  $('#pager').hidden = pages <= 1;
  $('#pageLabel').textContent = `Page ${page.toLocaleString()} of ${pages.toLocaleString()}`;

  const atFirst = page <= 1;
  const atLast = page >= pages;
  $('#pageFirst').disabled = atFirst;
  $('#pagePrev').disabled = atFirst;
  $('#pageNext').disabled = atLast;
  $('#pageLast').disabled = atLast;
}

// Paging keeps the scroll position by default, which lands you in the middle of
// the new page with no idea it changed.
function goToOffset(offset) {
  state.offset = Math.max(0, offset);
  loadVolumes();
  $('.table-wrap').scrollIntoView({ block: 'start', behavior: 'smooth' });
}

function rowHTML(v) {
  const sev = v.severity || 'unknown';
  const known = v.hasStats;
  const days = daysLabel(v.daysUntilFull);

  const usage = known
    ? `<div class="usage-cell">
         <span class="bar ${sev}"><span style="width:${Math.min(100, v.usagePercent)}%"></span></span>
         <span class="pct ${sev}">${pct(v.usagePercent)}</span>
       </div>`
    : `<div class="usage-cell"><span class="badge ${v.status}">${esc(v.status)}</span></div>`;

  const size = known
    ? `${bytes(v.usedBytes)} <span class="sub">/ ${bytes(v.capacityBytes)}</span>`
    : `<span class="sub">${bytes(v.requestedBytes)} requested</span>`;

  return `<tr data-key="${esc(v.cluster)}|${esc(v.namespace)}|${esc(v.name)}">
    <td><span class="ellip">${esc(v.clusterDisplay || v.cluster)}</span></td>
    <td><span class="ellip mono">${esc(v.namespace)}</span></td>
    <td>
      <span class="ellip mono">${esc(v.name)}</span>
      ${v.workload ? `<span class="sub ellip">${esc(v.workload)}</span>` : ''}
    </td>
    <td class="num">${usage}</td>
    <td class="num">${size}</td>
    <td>
      <span class="ellip">${esc(v.storageClass)}</span>
      ${v.sharedFilesystem ? '<span class="badge shared" title="Backing filesystem is shared with the node">shared</span>' : ''}
    </td>
    <td>${sparkline(v.spark)}</td>
    <td class="num">${days ? esc(days) : '<span class="sub">—</span>'}</td>
  </tr>`;
}

/* ── Filter dropdowns ───────────────────────────────────────── */

function hasActiveFilters() {
  return state.search !== '' || state.atRisk ||
    Object.values(state.filters).some((v) => v.length > 0);
}

function renderFacets(facets) {
  $$('.dropdown').forEach((dd) => {
    const facetKey = dd.dataset.facet;
    const param = dd.dataset.param;
    const items = facets[facetKey] || [];
    const selected = state.filters[param] || [];
    const needle = ($('.dropdown-search input', dd)?.value || '').toLowerCase();

    const visible = items.filter((i) => !needle || i.value.toLowerCase().includes(needle));
    const list = $('.dropdown-list', dd);

    list.innerHTML = visible.length
      ? visible.map((i) => `
          <label class="dd-item">
            <input type="checkbox" value="${esc(i.value)}" ${selected.includes(i.value) ? 'checked' : ''}/>
            <span class="dd-value" title="${esc(i.value)}">${esc(i.value)}</span>
            <span class="dd-count">${i.count}</span>
          </label>`).join('')
      : '<div class="dd-empty">No matches</div>';

    dd.classList.toggle('has-selection', selected.length > 0);
    $('.chip-count', dd).textContent = selected.length;
  });
}

function wireDropdowns() {
  $$('.dropdown').forEach((dd) => {
    const param = dd.dataset.param;

    $('.filter-btn', dd).addEventListener('click', (e) => {
      e.stopPropagation();
      const wasOpen = dd.classList.contains('open');
      $$('.dropdown').forEach((d) => d.classList.remove('open'));
      if (!wasOpen) {
        dd.classList.add('open');
        $('.dropdown-search input', dd)?.focus();
      }
    });

    dd.addEventListener('click', (e) => e.stopPropagation());

    $('.dropdown-search input', dd)?.addEventListener('input', () => {
      if (state.data) renderFacets(state.data.facets);
    });

    $('.dropdown-list', dd).addEventListener('change', (e) => {
      if (e.target.type !== 'checkbox') return;
      const val = e.target.value;
      const cur = state.filters[param];
      state.filters[param] = e.target.checked
        ? [...cur, val]
        : cur.filter((v) => v !== val);
      reload();
    });
  });

  document.addEventListener('click', () => $$('.dropdown').forEach((d) => d.classList.remove('open')));
}

/* ── Cluster cards ──────────────────────────────────────────── */

// Nodes whose kubelet could not be scraped are the reason claims on them report
// an error instead of a usage number, so name them here rather than leaving
// "3 of 5 reporting" as the only clue.
function nodeErrors(errors) {
  const rows = Object.entries(errors || {});
  if (!rows.length) return '';
  return `<div class="cluster-error">${rows
    .map(([node, err]) => `${esc(node)}: ${esc(err)}`)
    .join('<br>')}</div>`;
}

function renderClusters() {
  const data = state.clusters;
  if (!data) return;

  const grid = $('#clusterGrid');
  if (!data.clusters.length) {
    grid.innerHTML = `<div class="empty">
      <strong>No clusters connected</strong>
      Mount kubeconfig files into the configured directory and they are picked up automatically.
    </div>`;
  } else {
    grid.innerHTML = data.clusters.map((c) => `
      <div class="cluster-card ${c.reachable ? 'up' : 'down'}">
        <div class="cluster-head">
          <span class="cluster-name">${esc(c.displayName || c.name)}</span>
          <span class="badge ${c.reachable ? 'ok' : 'critical'}">${c.reachable ? 'reachable' : 'unreachable'}</span>
        </div>
        <dl class="cluster-meta">
          <dt>Endpoint</dt><dd>${esc(c.endpoint || '—')}</dd>
          <dt>Version</dt><dd>${esc(c.version || '—')}</dd>
          <dt>Kubeconfig</dt><dd>${esc(c.source)} · ${esc(c.context)}</dd>
          <dt>Nodes</dt><dd>${c.nodesScraped} of ${c.nodesTotal} reporting</dd>
          <dt>Claims</dt><dd>${c.pvcsTotal}</dd>
          <dt>Scrape</dt><dd>${c.scrapeMillis}ms · ${esc(relTime(c.lastCheck))}</dd>
        </dl>
        ${c.error ? `<div class="cluster-error">${esc(c.error)}</div>` : ''}
        ${nodeErrors(c.nodeErrors)}
      </div>`).join('');
  }

  const errs = Object.entries(data.loadErrors || {});
  $('#loadErrors').innerHTML = errs.length
    ? `<div class="config-card">
         <h3>Kubeconfig files that could not be loaded</h3>
         <div class="kv">${errs.map(([f, e]) =>
           `<div>${esc(f)}</div><div style="color:var(--crit)">${esc(e)}</div>`).join('')}</div>
       </div>`
    : '';
}

/* ── Configuration view ─────────────────────────────────────── */

function fmtVal(v) {
  if (v === true) return '<span class="val-true">true</span>';
  if (v === false) return '<span class="val-false">false</span>';
  if (v === null || v === undefined || v === '') return '<span class="val-empty">not set</span>';
  if (Array.isArray(v)) return v.length ? esc(v.join(', ')) : '<span class="val-empty">none</span>';
  if (typeof v === 'object') {
    const entries = Object.entries(v);
    return entries.length
      ? entries.map(([k, val]) => `${esc(k)}=${esc(val)}`).join(', ')
      : '<span class="val-empty">none</span>';
  }
  return esc(v);
}

function card(title, rows) {
  return `<div class="config-card">
    <h3>${esc(title)}</h3>
    <div class="kv">${rows.map(([k, v]) => `<div>${esc(k)}</div><div>${fmtVal(v)}</div>`).join('')}</div>
  </div>`;
}

function renderConfig() {
  const d = state.config;
  if (!d) return;
  const c = d.config;
  const rw = c.remoteWrite || {};
  const status = d.remoteWrite || {};

  $('#configSource').textContent = d.source;

  const rwRows = [
    ['Enabled', rw.enabled],
    ['Endpoint', rw.url],
    ['Tenant (X-Scope-OrgID)', rw.tenantId],
    ['Timeout', rw.timeout],
    ['Max samples per send', rw.maxSamplesPerSend],
    ['Max shards', rw.maxShards],
    ['Max retries', rw.maxRetries],
    ['External labels', rw.externalLabels],
    ['Extra headers', rw.headers],
    ['Basic auth user', rw.basicAuth?.username],
    ['TLS skip verify', rw.tlsConfig?.insecureSkipVerify],
  ];
  if (rw.enabled) {
    rwRows.push(
      ['Last success', status.lastSuccess ? relTime(status.lastSuccess) : 'never'],
      ['Samples sent', (status.samplesSent || 0).toLocaleString()],
      ['Samples failed', (status.samplesFailed || 0).toLocaleString()],
      ['Last error', status.lastError || '']);
  }

  $('#configGrid').innerHTML = [
    card('Collection', [
      ['Interval', c.collector?.interval],
      ['Timeout', c.collector?.timeout],
      ['Cluster concurrency', c.collector?.clusterConcurrency],
      ['Node concurrency', c.collector?.nodeConcurrency],
      ['Include namespaces', c.collector?.includeNamespaces],
      ['Exclude namespaces', c.collector?.excludeNamespaces],
      ['Keep unmounted claims', c.collector?.includeUnmounted],
      ['Stale after', c.collector?.staleAfter],
    ]),
    card('Cluster discovery', [
      ['Kubeconfig directory', c.discovery?.dir],
      ['Use every context', c.discovery?.allContexts],
      ['Rescan interval', c.discovery?.reloadInterval],
      ['In-cluster fallback', c.discovery?.inClusterFallback],
      ['File extensions', c.discovery?.fileExtensions],
    ]),
    card('Remote write to Mimir', rwRows),
    card('Interface', [
      ['Title', c.ui?.title],
      ['Warning threshold', `${c.ui?.thresholds?.warning}%`],
      ['Critical threshold', `${c.ui?.thresholds?.critical}%`],
      ['Listen', `${c.server?.listenAddress}:${c.server?.port}`],
    ]),
    card('Runtime', [
      ['Version', d.version?.version],
      ['Commit', d.version?.commit],
      ['Built', d.version?.date],
      ['Uptime', `${Math.floor((d.uptimeSeconds || 0) / 60)}m`],
      ['Config source', d.source],
      ['Editable from UI', d.readOnly ? 'no, read-only by design' : 'yes'],
      ['Last collection error', d.lastError || ''],
    ]),
  ].join('');
}

// The tooltip quotes the live thresholds rather than hard-coded numbers,
// because they are configurable and a stale "75%" here would be a lie.
function renderAtRiskTip() {
  const { warning, critical } = state.thresholds;
  $('#atRiskTip').innerHTML = `
    <strong>At risk only</strong> hides everything that is comfortably fine, leaving
    just the volumes worth acting on: those at or above the warning threshold
    (<code>${warning}%</code>), including the critical ones (<code>${critical}%</code>).
    <br><br>
    A volume counts as at risk on <em>either</em> disk usage or inode usage, whichever
    is worse &mdash; running out of inodes fails writes exactly like a full disk.
    <br><br>
    Volumes with no usage data (<code>unmounted</code>, <code>unreported</code>,
    <code>pending</code>, <code>block</code>) are never shown here, because there is
    nothing to judge them on.`;
}

/* ── Detail drawer ──────────────────────────────────────────── */

function lineChart(points) {
  if (!points || points.length < 2) {
    return '<div class="note">Not enough history yet. A trend appears after a few collection cycles.</div>';
  }
  const w = 500, h = 130, padL = 38, padR = 8, padT = 10, padB = 20;
  const vals = points.map((p) => p.pct);
  let min = Math.min(...vals), max = Math.max(...vals);
  // Pad the range so a nearly flat line does not fill the whole chart height
  // and read as dramatic growth.
  const span = Math.max(max - min, 1);
  min = Math.max(0, min - span * 0.15);
  max = Math.min(100, max + span * 0.15);
  const range = max - min || 1;

  const t0 = new Date(points[0].t).getTime();
  const t1 = new Date(points[points.length - 1].t).getTime();
  const tspan = t1 - t0 || 1;
  const x = (p) => padL + (w - padL - padR) * ((new Date(p.t).getTime() - t0) / tspan);
  const y = (v) => padT + (h - padT - padB) * (1 - (v - min) / range);

  const d = points.map((p, i) => `${i ? 'L' : 'M'}${x(p).toFixed(1)},${y(p.pct).toFixed(1)}`).join('');
  const area = `${d}L${x(points[points.length - 1]).toFixed(1)},${h - padB}L${padL},${h - padB}Z`;

  const ticks = [min, (min + max) / 2, max].map((v) => `
    <line class="grid-line" x1="${padL}" y1="${y(v).toFixed(1)}" x2="${w - padR}" y2="${y(v).toFixed(1)}"/>
    <text class="axis-label" x="${padL - 6}" y="${(y(v) + 3).toFixed(1)}" text-anchor="end">${v.toFixed(0)}%</text>`).join('');

  return `<svg class="chart" viewBox="0 0 ${w} ${h}" preserveAspectRatio="none">
    ${ticks}
    <path class="series-area" d="${area}"/>
    <path class="series" d="${d}"/>
  </svg>
  <div class="sub" style="display:flex;justify-content:space-between;margin-top:2px">
    <span>${esc(relTime(points[0].t))}</span><span>now</span>
  </div>`;
}

async function openDrawer(key) {
  const [cluster, namespace, name] = key.split('|');
  const v = state.data.volumes.find(
    (x) => x.cluster === cluster && x.namespace === namespace && x.name === name);
  if (!v) return;

  $('#drawerTitle').textContent = v.name;
  $('#drawerSub').textContent = `${v.clusterDisplay || v.cluster} · ${v.namespace}`;
  $('#drawer').hidden = false;
  $('#drawerBackdrop').hidden = false;

  const sev = v.severity || 'unknown';
  const rows = [
    ['Status', v.status],
    ['Phase', v.phase],
    ['Storage class', v.storageClass],
    ['Provisioner', v.provisioner],
    ['Volume mode', v.volumeMode],
    ['Access modes', v.accessModes],
    ['Persistent volume', v.volumeName],
    ['Requested', bytes(v.requestedBytes)],
    ['Provisioned', bytes(v.provisionedBytes)],
    ['Node', v.node],
    ['Workload', v.workload],
    ['Pods', v.pods],
    ['Sampled', v.sampled ? relTime(v.sampled) : null],
  ];

  const inodeRow = v.hasStats && v.inodes > 0
    ? `<div class="drawer-section">
         <h4>Inodes</h4>
         <div class="big-usage">
           <span class="num" style="font-size:22px">${pct(v.inodesPercent)}</span>
           <span class="of">${v.inodesUsed.toLocaleString()} of ${v.inodes.toLocaleString()} used</span>
         </div>
         <span class="bar lg ${v.inodesPercent >= state.thresholds.critical ? 'critical'
            : v.inodesPercent >= state.thresholds.warning ? 'warning' : 'ok'}">
           <span style="width:${Math.min(100, v.inodesPercent)}%"></span>
         </span>
         ${v.inodesPercent >= state.thresholds.warning
           ? '<div class="note warn">Inode exhaustion fails writes exactly like a full disk, even with free space left.</div>'
           : ''}
       </div>`
    : '';

  const growth = v.growthBytesPerDay !== undefined && v.growthBytesPerDay !== null
    ? `<div class="drawer-section">
         <h4>Trend</h4>
         <div class="kv" style="border:1px solid var(--border);border-radius:var(--radius-sm);overflow:hidden">
           <div>Growth</div><div>${v.growthBytesPerDay > 0 ? '+' : ''}${bytes(v.growthBytesPerDay)} / day</div>
           <div>Projected full</div><div>${daysLabel(v.daysUntilFull) || 'not growing'}</div>
         </div>
         <div class="note">Straight-line projection from recent samples. It assumes the current rate holds, which is a planning hint, not a forecast.</div>
       </div>`
    : '';

  $('#drawerBody').innerHTML = `
    <div class="drawer-section">
      <h4>Filesystem usage</h4>
      ${v.hasStats ? `
        <div class="big-usage">
          <span class="num ${sev === 'ok' ? '' : ''}" style="color:var(--${sev === 'critical' ? 'crit' : sev === 'warning' ? 'warn' : 'ok'})">${pct(v.usagePercent)}</span>
          <span class="of">${bytes(v.usedBytes)} of ${bytes(v.capacityBytes)} · ${bytes(v.availableBytes)} free</span>
        </div>
        <span class="bar lg ${sev}"><span style="width:${Math.min(100, v.usagePercent)}%"></span></span>
        ${v.sharedFilesystem ? `<div class="note warn">
          <div>This claim sits on a filesystem shared with the node, so the percentage above is of the host disk,
          not of the ${bytes(v.requestedBytes)} requested. Against the request it is at
          <strong>${pct(v.requestedPercent)}</strong>.</div></div>` : ''}
      ` : `<div class="note">${esc(v.message || 'No filesystem statistics are available for this claim.')}</div>`}
    </div>
    ${inodeRow}
    ${growth}
    <div class="drawer-section">
      <h4>Usage history</h4>
      <div id="drawerChart"><div class="note">Loading…</div></div>
    </div>
    <div class="drawer-section">
      <h4>Details</h4>
      <div class="kv" style="border:1px solid var(--border);border-radius:var(--radius-sm);overflow:hidden">
        ${rows.filter(([, val]) => val !== null && val !== undefined && val !== '' && !(Array.isArray(val) && !val.length))
          .map(([k, val]) => `<div>${esc(k)}</div><div>${fmtVal(val)}</div>`).join('')}
      </div>
    </div>`;

  try {
    const p = new URLSearchParams({ cluster, namespace, name });
    const res = await fetch(`/api/v1/history?${p}`);
    const hist = await res.json();
    $('#drawerChart').innerHTML = lineChart(hist.points);
  } catch {
    $('#drawerChart').innerHTML = '<div class="note">History could not be loaded.</div>';
  }
}

function closeDrawer() {
  $('#drawer').hidden = true;
  $('#drawerBackdrop').hidden = true;
}

/* ── Toast ──────────────────────────────────────────────────── */

let toastTimer;
function toast(msg) {
  const el = $('#toast');
  el.textContent = msg;
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.hidden = true; }, 2600);
}

/* ── Easter-egg themes and their particle trails ────────────── */

// Each hidden theme is unlocked by typing its name outside an input and
// brings its own particle skin; the physics underneath are shared. Glyph
// particles render larger than square ones or the balls read as noise.
const eggThemes = {
  girls: {
    toast: 'Theme: girls \u2728',
    colors: ['#ff6ad5', '#ffd166', '#5ef7c4', '#c79ad4', '#ffffff'],
  },
  boys: {
    toast: 'Theme: boys \u26bd',
    glyphs: ['\u26bd', '\ud83c\udfc0', '\ud83c\udfc8'],  // soccer, basketball, football
  },
};

const confetti = {
  ctx: null, parts: [], raf: 0, active: false, skin: null,
};

// Colour-emoji rasterization is expensive enough to drop frames when done
// 120 times per frame, and mutating ctx.font per particle forces a font
// re-resolve on top of it. Each glyph is rendered once into a small offscreen
// canvas here; the per-frame cost is then a plain scaled drawImage.
const glyphSprites = {};
function glyphSprite(glyph) {
  let sp = glyphSprites[glyph];
  if (!sp) {
    sp = document.createElement('canvas');
    sp.width = sp.height = 48;
    const sc = sp.getContext('2d');
    sc.font = '40px serif';
    sc.textAlign = 'center';
    sc.textBaseline = 'middle';
    sc.fillText(glyph, 24, 26);
    glyphSprites[glyph] = sp;
  }
  return sp;
}
// Tracked so the activation burst can land where the mouse actually is,
// even though the trigger is a keystroke.
let mouseX = innerWidth / 2, mouseY = innerHeight / 2;
let spawnArmed = true;

function confettiSpawn(x, y, n) {
  if (!confetti.active) return;
  // A hard cap keeps a frantic mouse from growing the array without bound.
  const room = 120 - confetti.parts.length;
  for (let i = 0; i < Math.min(n, room); i++) {
    const { colors, glyphs } = confetti.skin;
    const glyph = glyphs && glyphs[(Math.random() * glyphs.length) | 0];
    confetti.parts.push({
      x, y,
      vx: (Math.random() - 0.5) * 2.4,
      vy: -Math.random() * 1.8 - 0.4,
      size: glyphs ? Math.random() * 8 + 12 : Math.random() * 3 + 1.5,
      life: 1,
      color: colors && colors[(Math.random() * colors.length) | 0],
      glyph,
      sprite: glyph && glyphSprite(glyph),
    });
  }
  if (!confetti.raf) {
    lastTick = 0;
    confetti.raf = requestAnimationFrame(confettiTick);
  }
}

let lastTick = 0;
function confettiTick(now) {
  // Physics scale by elapsed time, not frames: frame-counted life dies in a
  // third of the intended 800ms on a 144Hz monitor and lingers for seconds
  // when the browser throttles. dt is clamped so a tab resuming from the
  // background does not teleport every particle off-screen in one step.
  const dt = Math.min(now - (lastTick || now), 64) / 16.67;
  lastTick = now;
  const c = $('#confetti');
  const { ctx, parts } = confetti;
  ctx.clearRect(0, 0, c.width, c.height);
  for (let i = parts.length - 1; i >= 0; i--) {
    const p = parts[i];
    p.x += p.vx * dt; p.y += p.vy * dt;
    p.vy += 0.055 * dt;   // gravity
    p.life -= dt / 48;    // 48 x 16.67ms = 800ms whatever the frame rate
    if (p.life <= 0) { parts.splice(i, 1); continue; }
    ctx.globalAlpha = p.life;
    if (p.sprite) {
      ctx.drawImage(p.sprite, p.x, p.y, p.size, p.size);
    } else {
      ctx.fillStyle = p.color;
      ctx.fillRect(p.x, p.y, p.size, p.size);
    }
  }
  ctx.globalAlpha = 1;
  // The loop stops itself when the last particle dies, so an idle tab
  // spends nothing.
  confetti.raf = parts.length ? requestAnimationFrame(confettiTick) : 0;
}

// One always-on listener: position tracking is a two-field write when the
// theme is off, and spawning is throttled to one batch per animation frame
// however fast the mouse moves.
document.addEventListener('mousemove', (e) => {
  mouseX = e.clientX; mouseY = e.clientY;
  if (!confetti.active || !spawnArmed) return;
  spawnArmed = false;
  requestAnimationFrame(() => { spawnArmed = true; });
  confettiSpawn(mouseX, mouseY, 3);
});

function resizeConfetti() {
  const c = $('#confetti');
  c.width = innerWidth; c.height = innerHeight;
}
window.addEventListener('resize', resizeConfetti);

// Called whenever the theme changes; owns everything conditional on an
// easter-egg theme being active.
function syncConfetti() {
  const skin = eggThemes[document.documentElement.dataset.theme];
  const on = !!skin && !matchMedia('(prefers-reduced-motion: reduce)').matches;
  const c = $('#confetti');
  confetti.active = on;
  confetti.skin = on ? skin : null;
  c.hidden = !on;
  if (on) {
    confetti.ctx = c.getContext('2d');
    resizeConfetti();
  } else {
    confetti.parts.length = 0;
  }
}

/* ── Wiring ─────────────────────────────────────────────────── */

let searchTimer;
let keyBuffer = '';
function wire() {
  $('#search').addEventListener('input', (e) => {
    state.search = e.target.value.trim();
    clearTimeout(searchTimer);
    searchTimer = setTimeout(reload, 180);
  });

  $('#atRisk').addEventListener('change', (e) => {
    state.atRisk = e.target.checked;
    reload();
  });

  $('#clearFilters').addEventListener('click', () => {
    state.filters = { cluster: [], namespace: [], storageClass: [], status: [] };
    state.search = '';
    state.atRisk = false;
    $('#search').value = '';
    $('#atRisk').checked = false;
    reload();
  });

  $('#pageSize').addEventListener('change', (e) => {
    state.pageSize = Number(e.target.value) || 0;
    localStorage.setItem('peevee-page-size', String(state.pageSize));
    reload();
  });

  $('#pageFirst').addEventListener('click', () => goToOffset(0));
  $('#pagePrev').addEventListener('click', () => goToOffset(state.offset - state.pageSize));
  $('#pageNext').addEventListener('click', () => goToOffset(state.offset + state.pageSize));
  $('#pageLast').addEventListener('click', () => {
    const matched = state.data?.matched || 0;
    goToOffset((Math.ceil(matched / state.pageSize) - 1) * state.pageSize);
  });

  $$('.vol-table th.sortable').forEach((th) => {
    th.addEventListener('click', () => {
      const key = th.dataset.sort;
      if (state.sort === key) state.desc = !state.desc;
      else { state.sort = key; state.desc = false; }
      reload();
    });
  });

  $('#volBody').addEventListener('click', (e) => {
    const tr = e.target.closest('tr');
    if (tr?.dataset.key) openDrawer(tr.dataset.key);
  });

  $('#drawerClose').addEventListener('click', closeDrawer);
  $('#drawerBackdrop').addEventListener('click', closeDrawer);

  $('#refreshBtn').addEventListener('click', async (e) => {
    const btn = e.currentTarget;
    btn.classList.add('spinning');
    try {
      await fetch('/api/v1/refresh', { method: 'POST' });
      toast('Collection triggered');
    } catch {
      toast('Could not reach the server');
    }
    setTimeout(() => btn.classList.remove('spinning'), 900);
  });

  $$('.seg-btn').forEach((btn) => {
    btn.addEventListener('click', () => setConfigView(btn.dataset.view));
  });

  $('#copyYaml').addEventListener('click', async () => {
    const text = $('#configYaml').dataset.raw || '';
    try {
      await navigator.clipboard.writeText(text);
      toast('Configuration copied');
    } catch {
      // Clipboard access is blocked outside a secure context, which includes
      // plain http:// on anything but localhost. Select it instead so Ctrl+C works.
      const range = document.createRange();
      range.selectNodeContents($('#configYaml'));
      const sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(range);
      toast('Selected \u2014 press Ctrl+C to copy');
    }
  });

  const helpBtn = $('#atRiskHelp');
  const helpTip = $('#atRiskTip');
  const showTip = (on) => {
    helpTip.hidden = !on;
    helpBtn.setAttribute('aria-expanded', String(on));
  };
  helpBtn.addEventListener('mouseenter', () => showTip(true));
  helpBtn.addEventListener('mouseleave', () => showTip(false));
  // Click and focus keep it reachable by keyboard and on touch, where there
  // is no hover to speak of.
  helpBtn.addEventListener('focus', () => showTip(true));
  helpBtn.addEventListener('blur', () => showTip(false));
  helpBtn.addEventListener('click', (e) => {
    e.stopPropagation();
    showTip(helpTip.hidden);
  });
  document.addEventListener('click', () => showTip(false));

  $$('.tab').forEach((tab) => {
    tab.addEventListener('click', () => {
      state.tab = tab.dataset.tab;
      $$('.tab').forEach((t) => t.classList.toggle('active', t === tab));
      $$('.panel').forEach((p) => p.classList.toggle('active', p.id === `panel-${state.tab}`));
      if (state.tab === 'clusters') loadClusters();
      if (state.tab === 'config') loadConfig();
    });
  });

  $('#themeBtn').addEventListener('click', () => {
    const root = document.documentElement;
    const order = ['auto', 'light', 'dark'];
    // indexOf is -1 for the easter-egg themes (girls, boys), so the button
    // always exits them back to "auto" rather than keeping hidden states in
    // the cycle.
    const next = order[(order.indexOf(root.dataset.theme) + 1) % order.length];
    root.dataset.theme = next;
    localStorage.setItem('peevee-theme', next);
    syncConfetti();
    toast(`Theme: ${next}`);
  });

  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      closeDrawer();
      $$('.dropdown').forEach((d) => d.classList.remove('open'));
      $('#atRiskTip').hidden = true;
    }
    // "/" focuses search, the shortcut every developer already has in muscle memory.
    if (e.key === '/' && document.activeElement.tagName !== 'INPUT') {
      e.preventDefault();
      $('#search').focus();
    }
    // Easter eggs: typing a hidden theme's name outside an input flips to
    // it. Only bare printable keys count, so shortcuts like Ctrl+R never
    // feed the buffer, and searching for a namespace containing the name
    // never triggers it.
    const tag = document.activeElement.tagName;
    if (e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey
        && tag !== 'INPUT' && tag !== 'TEXTAREA') {
      keyBuffer = (keyBuffer + e.key.toLowerCase()).slice(-8);
      const name = Object.keys(eggThemes).find((n) => keyBuffer.endsWith(n));
      if (name) {
        keyBuffer = '';
        document.documentElement.dataset.theme = name;
        localStorage.setItem('peevee-theme', name);
        syncConfetti();
        confettiSpawn(mouseX, mouseY, 60);
        toast(eggThemes[name].toast);
      }
    }
  });

  wireDropdowns();
}

function init() {
  const saved = localStorage.getItem('peevee-theme');
  if (saved) document.documentElement.dataset.theme = saved;
  syncConfetti();

  // Read before the first fetch, so a saved choice does not cost an extra round
  // trip at the default size. Only values the dropdown actually offers are
  // accepted, or the select and the state would disagree.
  const sizes = $$('#pageSize option').map((o) => o.value);
  const savedSize = localStorage.getItem('peevee-page-size');
  if (sizes.includes(savedSize)) state.pageSize = Number(savedSize);
  $('#pageSize').value = String(state.pageSize);

  wire();
  loadConfig();
  loadVolumes();
  connectStream();

  // A slow refresh keeps the "updated Ns ago" label honest between pushes.
  setInterval(() => {
    if (state.data) setLive('connected', `updated ${relTime(state.data.generatedAt)}`);
  }, 10000);
}

init();
