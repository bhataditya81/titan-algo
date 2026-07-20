// app.js — state + DOM wiring. Talks to the backend only through Api (api.js)
// and never assumes a call succeeded: every handler checks res.ok before
// touching the DOM, so a backend-less page load never throws.

(() => {
  const $ = (id) => document.getElementById(id);

  const state = {
    running: false,
    mode: 'PAPER',
    ws: null,
    lastMessageAt: null,
    tradesCache: [],
    tradesSort: { key: null, dir: 1 },
    candlesCache: [],
    chartReady: false,
    viewingTable: false,
  };

  // ---- icon injection ----------------------------------------------------
  function icon(elId, name) { const el = $(elId); if (el) el.innerHTML = ICONS[name]; }
  function paintStaticIcons() {
    icon('settingsIcon', 'gear');
    icon('settingsCloseIcon', 'close');
    icon('killCloseIcon', 'close');
    icon('killWarningIcon', 'warning');
    icon('killConfirmIcon', 'stop');
    icon('reconnectIcon', 'refresh');
    icon('loadCandlesIcon', 'chart');
    icon('viewToggleIcon', 'table');
    icon('refreshTradesIcon', 'refresh');
    icon('startIcon', 'play');
    icon('pauseIcon', 'pause');
    icon('killIcon', 'stop');
  }

  // ---- theme --------------------------------------------------------------
  const THEME_KEY = 'titan_theme';
  function applyTheme(theme) {
    document.documentElement.dataset.theme = theme;
    $('themeDark').checked = theme === 'dark';
    $('themeLight').checked = theme === 'light';
    Charts.applyTheme();
  }
  function initTheme() {
    const saved = localStorage.getItem(THEME_KEY) || 'dark';
    applyTheme(saved);
  }
  function onThemeChange(e) {
    const theme = e.target.value;
    localStorage.setItem(THEME_KEY, theme);
    applyTheme(theme);
  }

  // ---- connection status ---------------------------------------------------
  function setConnState(s, text) {
    $('connDot').dataset.state = s;
    $('connText').textContent = text;
  }
  function setMode(mode) {
    if (!mode) return;
    state.mode = mode;
    $('modeBadge').textContent = mode;
    $('modeBadge').dataset.mode = mode;
  }

  // ---- button busy helper ---------------------------------------------------
  async function withBusy(btn, iconId, iconName, fn) {
    const original = btn.innerHTML;
    btn.disabled = true;
    btn.innerHTML = '<span class="spinner" aria-hidden="true"></span> Working…';
    try {
      await fn();
    } finally {
      btn.innerHTML = original;
      icon(iconId, iconName);
      refreshButtonStates();
    }
  }

  function refreshButtonStates() {
    $('startBtn').disabled = state.running;
    $('pauseBtn').disabled = !state.running;
    $('pauseBtn').lastChild.textContent = state.running ? ' Pause' : ' Resume';
    $('killBtn').disabled = !state.running;
  }

  // ---- control panel actions ------------------------------------------------
  function collectConfigPayload() {
    const strategy = $('strategySelect').value;
    const balance = parseFloat($('balanceInput').value);
    const manualMode = $('symbolModeManual').checked;
    const payload = {};
    if (strategy) payload.strategy = strategy;
    if (!Number.isNaN(balance) && balance > 0) payload.session_balance = balance;
    payload.discovery_enabled = !manualMode;
    if (manualMode) payload.symbol = $('manualSymbolInput').value.trim();
    return payload;
  }

  async function doStart() {
    await withBusy($('startBtn'), 'startIcon', 'play', async () => {
      const cfgRes = await Api.post('/api/config', collectConfigPayload());
      if (!cfgRes.ok) { alertInline('Config update failed: ' + cfgRes.error); return; }
      const res = await Api.post('/api/start');
      if (!res.ok) { alertInline('Start failed: ' + res.error); return; }
      state.running = true;
    });
  }

  async function doPauseResume() {
    const wasRunning = state.running;
    await withBusy($('pauseBtn'), 'pauseIcon', 'pause', async () => {
      const res = wasRunning ? await Api.post('/api/stop') : await Api.post('/api/start');
      if (!res.ok) { alertInline((wasRunning ? 'Stop' : 'Resume') + ' failed: ' + res.error); return; }
      state.running = !wasRunning;
    });
  }

  async function doKillConfirmed() {
    await withBusy($('killConfirmBtn'), 'killConfirmIcon', 'stop', async () => {
      const res = await Api.post('/api/kill');
      if (!res.ok) { alertInline('Kill switch failed: ' + res.error); return; }
      state.running = false;
    });
    $('killModal').close();
    $('killConfirmInput').value = '';
    $('killConfirmBtn').disabled = true;
  }

  function alertInline(msg) {
    // Minimal, dependency-free feedback channel — a solo trader's console-log
    // habit, surfaced where they're already looking.
    console.warn('[TitanAlgo]', msg);
    $('lastUpdated').textContent = msg;
  }

  // ---- strategies ------------------------------------------------------------
  async function loadStrategies() {
    const sel = $('strategySelect');
    const res = await Api.get('/api/strategies');
    if (!res.ok || !res.data || !Array.isArray(res.data.strategies)) {
      sel.innerHTML = '<option value="">Unavailable — check connection</option>';
      return;
    }
    sel.innerHTML = res.data.strategies.map((s) => `<option value="${s}">${s}</option>`).join('')
      || '<option value="">No strategies configured</option>';
  }

  // ---- config (generic key/value, but pre-fill known fields if present) -----
  async function loadConfig() {
    const res = await Api.get('/api/config');
    if (!res.ok || !res.data) return;
    const cfg = res.data;
    if (typeof cfg.session_balance === 'number') $('balanceInput').value = cfg.session_balance;
    if (typeof cfg.strategy === 'string' && cfg.strategy) $('strategySelect').value = cfg.strategy;
    if (typeof cfg.discovery_enabled === 'boolean') {
      $('symbolModeAuto').checked = cfg.discovery_enabled;
      $('symbolModeManual').checked = !cfg.discovery_enabled;
      $('manualSymbolInput').disabled = cfg.discovery_enabled;
    }
  }

  // ---- WebSocket / KPIs --------------------------------------------------------
  function fmtMoney(n) {
    if (typeof n !== 'number' || Number.isNaN(n)) return '—';
    return n.toLocaleString('en-IN', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  }

  function setPnlTile(elId, value) {
    const el = $(elId);
    if (typeof value !== 'number' || Number.isNaN(value)) { el.textContent = '—'; el.className = 'kpi-value mono'; return; }
    const positive = value >= 0;
    el.className = 'kpi-value mono ' + (positive ? 'positive' : 'negative');
    el.innerHTML = (positive ? ICONS.arrowUp : ICONS.arrowDown) +
      `<span>${positive ? '+' : '-'}${fmtMoney(Math.abs(value))}</span>`;
  }

  function handleWsMessage(msg) {
    state.lastMessageAt = Date.now();
    setConnState('connected', 'Connected');

    // Defensive field pulls — real field names may be unrealized_pnl or
    // unrealizedPnL, positions or positions_count, etc; try both.
    if ('running' in msg) { state.running = !!msg.running; refreshButtonStates(); }
    if ('mode' in msg) setMode(msg.mode);

    if (typeof msg.balance === 'number') $('kpiBalance').textContent = fmtMoney(msg.balance);

    const unreal = firstNumber(msg.unrealized_pnl, msg.unrealizedPnL, msg.unrealized);
    if (unreal !== undefined) setPnlTile('kpiUnrealized', unreal);

    const real = firstNumber(msg.realized_pnl, msg.realizedPnL, msg.realized);
    if (real !== undefined) setPnlTile('kpiRealized', real);

    const positions = firstNumber(msg.positions_count, msg.positionsCount, msg.positions);
    if (positions !== undefined) $('kpiPositions').textContent = String(positions);
  }
  function firstNumber(...vals) {
    for (const v of vals) if (typeof v === 'number' && !Number.isNaN(v)) return v;
    return undefined;
  }

  function connectWebSocket() {
    if (state.ws) { try { state.ws.close(); } catch (_) {} }
    if (!Api.getToken()) { setConnState('disconnected', 'Disconnected (no token set)'); return; }
    setConnState('disconnected', 'Connecting…');
    state.ws = Api.connectWs({
      onOpen: () => setConnState('connected', 'Connected'),
      onMessage: handleWsMessage,
      onClose: () => setConnState('error', 'Disconnected — click Reconnect'),
      onError: () => setConnState('error', 'Connection error'),
    });
  }

  function tickLastUpdated() {
    if (!state.lastMessageAt) { $('lastUpdated').textContent = 'Last updated: never'; return; }
    const secs = Math.floor((Date.now() - state.lastMessageAt) / 1000);
    $('lastUpdated').textContent = `Last updated ${secs}s ago`;
  }

  // ---- chart panel ---------------------------------------------------------
  function initChartPanel() {
    const container = $('chartContainer');
    try {
      if (!Charts.available()) throw new Error('library not loaded');
      Charts.init(container);
      state.chartReady = true;
    } catch (err) {
      // Chart library missing or incompatible (e.g. CDN served an
      // unexpected API version) — degrade to the accessible table view
      // rather than taking the rest of the app down with it.
      console.warn('[TitanAlgo] chart unavailable, falling back to table:', err.message);
      container.classList.add('hidden');
      $('chartUnavailableNote').classList.remove('hidden');
      $('candleTableWrap').classList.remove('hidden');
      $('viewToggleBtn').disabled = true;
    }
  }

  async function loadCandles() {
    const symbol = $('symbolInput').value.trim() || 'NIFTY';
    const limit = $('candleLimitSelect').value;
    const res = await Api.get(`/api/candles?symbol=${encodeURIComponent(symbol)}&limit=${encodeURIComponent(limit)}`);
    if (!res.ok || !res.data || !Array.isArray(res.data.candles)) {
      state.candlesCache = [];
      Charts.renderTable($('candleTableBody'), []);
      const note = $('chartUnavailableNote');
      note.textContent = (res.data && res.data.error) ? res.data.error : ('Could not load candles: ' + (res.error || 'unknown error'));
      note.classList.remove('hidden');
      return;
    }
    $('chartUnavailableNote').classList.add('hidden');
    state.candlesCache = res.data.candles;
    if (state.chartReady) Charts.render(state.candlesCache);
    Charts.renderTable($('candleTableBody'), state.candlesCache);
  }

  function toggleChartView() {
    state.viewingTable = !state.viewingTable;
    $('chartContainer').classList.toggle('hidden', state.viewingTable);
    $('candleTableWrap').classList.toggle('hidden', !state.viewingTable);
    $('viewToggleBtn').setAttribute('aria-pressed', String(state.viewingTable));
    $('viewToggleBtn').lastChild.textContent = state.viewingTable ? ' View as chart' : ' View as table';
  }

  // ---- trades table (generic, schema-agnostic) ------------------------------
  function isTimeKey(key) { return /time|date/i.test(key); }
  function formatCell(key, value) {
    if (value === null || value === undefined) return '—';
    if (isTimeKey(key)) {
      const d = new Date(value);
      if (!Number.isNaN(d.getTime())) return d.toLocaleString();
    }
    if (typeof value === 'number') return value.toLocaleString('en-IN', { maximumFractionDigits: 2 });
    if (typeof value === 'object') return JSON.stringify(value);
    return String(value);
  }
  function titleCase(key) {
    return key.replace(/_/g, ' ').replace(/([a-z])([A-Z])/g, '$1 $2')
      .replace(/\b\w/g, (c) => c.toUpperCase());
  }

  function renderTradesTable() {
    const thead = $('tradesTableHead');
    const tbody = $('tradesTableBody');
    const trades = state.tradesCache;

    if (!trades.length) {
      thead.innerHTML = '';
      tbody.innerHTML = '<tr><td class="empty-state">No trades yet.</td></tr>';
      return;
    }

    const keys = Object.keys(trades[0]);
    const { key: sortKey, dir } = state.tradesSort;
    let rows = trades;
    if (sortKey) {
      rows = [...trades].sort((a, b) => {
        const av = a[sortKey], bv = b[sortKey];
        if (av === bv) return 0;
        return (av > bv ? 1 : -1) * dir;
      });
    }

    thead.innerHTML = '<tr>' + keys.map((k) => {
      const active = k === sortKey;
      const arrow = active ? (dir === 1 ? ICONS.chevronUp : ICONS.chevronDown) : '';
      return `<th data-key="${k}" tabindex="0" role="button" aria-sort="${active ? (dir === 1 ? 'ascending' : 'descending') : 'none'}">
                <span class="th-inner">${titleCase(k)} ${arrow}</span></th>`;
    }).join('') + '</tr>';

    tbody.innerHTML = rows.map((row) =>
      '<tr>' + keys.map((k) => `<td class="${typeof row[k] === 'number' ? 'num' : ''}">${formatCell(k, row[k])}</td>`).join('') + '</tr>'
    ).join('');

    thead.querySelectorAll('th').forEach((th) => {
      const activate = () => {
        const k = th.dataset.key;
        state.tradesSort = { key: k, dir: state.tradesSort.key === k ? -state.tradesSort.dir : 1 };
        renderTradesTable();
      };
      th.addEventListener('click', activate);
      th.addEventListener('keydown', (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); activate(); } });
    });
  }

  async function loadTrades() {
    const limit = $('tradesLimitSelect').value;
    const res = await Api.get(`/api/trades?limit=${encodeURIComponent(limit)}`);
    if (!res.ok || !res.data || !Array.isArray(res.data.trades)) {
      state.tradesCache = [];
      $('tradesTableHead').innerHTML = '';
      $('tradesTableBody').innerHTML = `<tr><td class="empty-state">Could not load trades${res.error ? ': ' + res.error : ''}.</td></tr>`;
      return;
    }
    state.tradesCache = res.data.trades;
    renderTradesTable();
  }

  // ---- settings modal --------------------------------------------------------
  function openSettings() {
    $('baseUrlInput').value = Api.getBaseUrl();
    $('tokenInput').value = Api.getToken();
    $('settingsModal').showModal();
  }
  function saveSettings() {
    Api.setBaseUrl($('baseUrlInput').value.trim() || Api.DEFAULT_BASE_URL);
    Api.setToken($('tokenInput').value.trim());
    $('settingsModal').close();
    bootstrapData();
  }

  // ---- kill modal --------------------------------------------------------
  function openKillModal() {
    $('killConfirmInput').value = '';
    $('killConfirmBtn').disabled = true;
    $('killModal').showModal();
  }

  // ---- wiring ------------------------------------------------------------
  function wireEvents() {
    $('settingsBtn').addEventListener('click', openSettings);
    $('settingsCloseBtn').addEventListener('click', () => $('settingsModal').close());
    $('settingsCancelBtn').addEventListener('click', () => $('settingsModal').close());
    $('settingsForm').addEventListener('submit', (e) => { e.preventDefault(); saveSettings(); });
    $('themeDark').addEventListener('change', onThemeChange);
    $('themeLight').addEventListener('change', onThemeChange);

    $('symbolModeAuto').addEventListener('change', () => { $('manualSymbolInput').disabled = true; });
    $('symbolModeManual').addEventListener('change', () => { $('manualSymbolInput').disabled = false; $('manualSymbolInput').focus(); });

    $('startBtn').addEventListener('click', doStart);
    $('pauseBtn').addEventListener('click', doPauseResume);
    $('killBtn').addEventListener('click', openKillModal);
    $('killCloseBtn').addEventListener('click', () => $('killModal').close());
    $('killCancelBtn').addEventListener('click', () => $('killModal').close());
    $('killConfirmInput').addEventListener('input', (e) => {
      $('killConfirmBtn').disabled = e.target.value.trim() !== 'STOP';
    });
    $('killConfirmBtn').addEventListener('click', doKillConfirmed);

    // Native <dialog> is supposed to close on Escape on its own, but that
    // default action doesn't fire reliably in every engine — add an explicit
    // fallback so keyboard users can always dismiss (harmless double-close
    // when the native behavior does also fire; dialog.close() on an
    // already-closed dialog is a no-op).
    [$('settingsModal'), $('killModal')].forEach((dlg) => {
      dlg.addEventListener('keydown', (e) => {
        if (e.key === 'Escape') { e.preventDefault(); dlg.close(); }
      });
    });

    $('reconnectBtn').addEventListener('click', connectWebSocket);

    $('loadCandlesBtn').addEventListener('click', loadCandles);
    $('viewToggleBtn').addEventListener('click', toggleChartView);

    $('refreshTradesBtn').addEventListener('click', loadTrades);
    $('tradesLimitSelect').addEventListener('change', loadTrades);
  }

  async function bootstrapData() {
    await loadStrategies();
    await loadConfig();
    await loadTrades();
    await loadCandles();
    connectWebSocket();
  }

  function init() {
    paintStaticIcons();
    initTheme();
    initChartPanel();
    wireEvents();
    refreshButtonStates();
    setInterval(tickLastUpdated, 1000);
    bootstrapData(); // safe with no backend: every call fails soft and shows empty/disconnected states
  }

  document.addEventListener('DOMContentLoaded', init);
})();
