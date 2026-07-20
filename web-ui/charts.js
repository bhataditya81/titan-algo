// charts.js — candlestick rendering via TradingView Lightweight Charts
// (loaded from CDN in index.html) plus a plain <table> fallback that reads
// the same candle array, for accessibility and for when the CDN script
// fails to load (no build step means no bundler to vendor it through, so we
// degrade instead of hard-failing — see docs/reports/UI-FRONTEND-REPORT.md).

const Charts = (() => {
  let chart = null;
  let series = null;
  let volumeSeries = null;

  function available() {
    return typeof LightweightCharts !== 'undefined';
  }

  function themeColors() {
    const dark = document.documentElement.dataset.theme !== 'light';
    return {
      bg: dark ? '#0B0F17' : '#F8FAFC',
      text: dark ? '#E2E8F0' : '#1E293B',
      grid: dark ? '#1A2233' : '#E2E8F0',
    };
  }

  function init(container) {
    if (!available()) return null;
    const c = themeColors();
    chart = LightweightCharts.createChart(container, {
      width: container.clientWidth,
      height: 360,
      layout: { background: { color: c.bg }, textColor: c.text, fontFamily: 'Fira Code, monospace' },
      grid: { vertLines: { color: c.grid }, horzLines: { color: c.grid } },
      timeScale: { timeVisible: true, secondsVisible: false },
      rightPriceScale: { borderColor: c.grid },
      handleScroll: true,
      handleScale: true,
    });
    series = chart.addCandlestickSeries({
      upColor: '#26A69A', downColor: '#EF5350',
      borderUpColor: '#26A69A', borderDownColor: '#EF5350',
      wickUpColor: '#26A69A', wickDownColor: '#EF5350',
    });
    volumeSeries = chart.addHistogramSeries({
      priceFormat: { type: 'volume' },
      priceScaleId: '',
      color: '#3B82F6',
    });
    volumeSeries.priceScale().applyOptions({ scaleMargins: { top: 0.8, bottom: 0 } });

    const ro = new ResizeObserver(() => {
      if (chart) chart.applyOptions({ width: container.clientWidth });
    });
    ro.observe(container);
    return chart;
  }

  function toUnixTime(t) {
    // Accept ISO-8601 strings or already-numeric epoch seconds.
    if (typeof t === 'number') return t;
    const ms = Date.parse(t);
    return Number.isNaN(ms) ? 0 : Math.floor(ms / 1000);
  }

  function render(candles) {
    if (!chart || !series) return;
    const bars = candles.map((c) => ({
      time: toUnixTime(c.time),
      open: Number(c.open) || 0,
      high: Number(c.high) || 0,
      low: Number(c.low) || 0,
      close: Number(c.close) || 0,
    })).sort((a, b) => a.time - b.time);
    series.setData(bars);

    const vols = candles.map((c) => ({
      time: toUnixTime(c.time),
      value: Number(c.volume) || 0,
      color: (Number(c.close) >= Number(c.open)) ? 'rgba(38,166,154,0.4)' : 'rgba(239,83,80,0.4)',
    })).sort((a, b) => a.time - b.time);
    volumeSeries.setData(vols);
  }

  function renderTable(tbody, candles) {
    tbody.innerHTML = '';
    if (!candles.length) {
      tbody.innerHTML = '<tr><td colspan="6" class="empty-state">No candle data</td></tr>';
      return;
    }
    for (const c of candles) {
      const tr = document.createElement('tr');
      const cols = [c.time, c.open, c.high, c.low, c.close, c.volume];
      tr.innerHTML = cols.map((v) => `<td class="num">${v === undefined ? '—' : v}</td>`).join('');
      tbody.appendChild(tr);
    }
  }

  function applyTheme() {
    if (!chart) return;
    const c = themeColors();
    chart.applyOptions({
      layout: { background: { color: c.bg }, textColor: c.text },
      grid: { vertLines: { color: c.grid }, horzLines: { color: c.grid } },
    });
  }

  return { available, init, render, renderTable, applyTheme };
})();
