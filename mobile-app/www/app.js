// TitanAlgo Mobile App - JavaScript

// Configuration
let CONFIG = {
    serverUrl: localStorage.getItem('serverUrl') || 'http://localhost:8080',
    apiKey: localStorage.getItem('apiKey') || ''
};

let ws = null;
let reconnectAttempts = 0;
const MAX_RECONNECT_ATTEMPTS = 5;

// ─────────────────────────────────────────────────────────────────
// INITIALIZATION
// ─────────────────────────────────────────────────────────────────

document.addEventListener('DOMContentLoaded', () => {
    // Check if settings configured
    if (!CONFIG.apiKey) {
        showSettings();
    } else {
        connect();
    }

    // Load initial data
    refreshAll();

    // Periodic refresh when visible
    setInterval(() => {
        if (document.visibilityState === 'visible') {
            loadStatus();
        }
    }, 10000);
});

// ─────────────────────────────────────────────────────────────────
// API CALLS
// ─────────────────────────────────────────────────────────────────

async function api(endpoint, method = 'GET', body = null) {
    const options = {
        method,
        headers: {
            'Content-Type': 'application/json',
            'X-API-Key': CONFIG.apiKey
        }
    };

    if (body) {
        options.body = JSON.stringify(body);
    }

    try {
        const response = await fetch(`${CONFIG.serverUrl}${endpoint}`, options);

        if (!response.ok) {
            throw new Error(`HTTP ${response.status}`);
        }

        return await response.json();
    } catch (error) {
        console.error(`API Error [${endpoint}]:`, error);
        setConnectionStatus(false);
        throw error;
    }
}

async function loadStatus() {
    try {
        const data = await api('/api/status');
        updateDashboard(data);
        setConnectionStatus(true);
    } catch (e) {
        console.error('Failed to load status:', e);
    }
}

async function loadPositions() {
    try {
        const data = await api('/api/positions');
        renderPositions(data.positions || []);
    } catch (e) {
        console.error('Failed to load positions:', e);
    }
}

async function loadTrades() {
    try {
        const data = await api('/api/trades');
        renderTrades(data.trades || []);
    } catch (e) {
        console.error('Failed to load trades:', e);
    }
}

async function loadConfig() {
    try {
        const data = await api('/api/config');
        populateConfigForm(data);
    } catch (e) {
        console.error('Failed to load config:', e);
    }
}

async function refreshAll() {
    await Promise.all([
        loadStatus(),
        loadPositions(),
        loadConfig()
    ]);
}

// ─────────────────────────────────────────────────────────────────
// TRADING CONTROLS
// ─────────────────────────────────────────────────────────────────

async function startTrading() {
    const strategy = document.getElementById('cfg-strategy').value;

    try {
        await api('/api/start', 'POST', { mode: 'paper', strategy });
        showToast('Trading started!', 'success');
        loadStatus();
    } catch (e) {
        showToast('Failed to start trading', 'error');
    }
}

async function stopTrading() {
    if (!confirm('Stop trading? All open positions will remain.')) {
        return;
    }

    try {
        await api('/api/stop', 'POST');
        showToast('Trading stopped', 'success');
        loadStatus();
    } catch (e) {
        showToast('Failed to stop trading', 'error');
    }
}

async function saveConfig(event) {
    event.preventDefault();

    const config = {
        strategy: document.getElementById('cfg-strategy').value,
        session_balance: parseFloat(document.getElementById('cfg-balance').value),
        stop_loss_enabled: document.getElementById('cfg-stoploss').checked,
        stop_loss_percent: parseFloat(document.getElementById('cfg-stoploss-pct').value)
    };

    try {
        await api('/api/config', 'POST', config);
        showToast('Configuration saved!', 'success');
    } catch (e) {
        showToast('Failed to save config', 'error');
    }
}

// ─────────────────────────────────────────────────────────────────
// UI UPDATES
// ─────────────────────────────────────────────────────────────────

function updateDashboard(data) {
    // Engine status badge
    const badge = document.getElementById('engine-status');
    badge.textContent = data.running ? 'RUNNING' : 'STOPPED';
    badge.className = 'badge ' + (data.running ? 'running' : 'stopped');

    // Stats
    document.getElementById('balance').textContent = formatCurrency(data.balance);

    const unrealized = document.getElementById('unrealized');
    unrealized.textContent = formatCurrency(data.unrealized_pnl, true);
    unrealized.parentElement.className = 'stat-card ' + (data.unrealized_pnl >= 0 ? 'positive' : 'negative');

    const realized = document.getElementById('realized');
    realized.textContent = formatCurrency(data.realized_pnl, true);
    realized.parentElement.className = 'stat-card ' + (data.realized_pnl >= 0 ? 'positive' : 'negative');

    document.getElementById('positions-count').textContent = data.positions_count;

    // Button states
    document.getElementById('btn-start').disabled = data.running;
    document.getElementById('btn-stop').disabled = !data.running;

    // Update last update time
    document.getElementById('last-update').textContent = 'Updated: just now';
}

function renderPositions(positions) {
    const container = document.getElementById('positions-list');

    if (positions.length === 0) {
        container.innerHTML = '<p class="empty-state">No open positions</p>';
        return;
    }

    container.innerHTML = positions.map(p => `
        <div class="position-item">
            <div class="position-symbol">${p.symbol}</div>
            <div class="position-details">
                <span>${p.side} ${p.quantity} @ ₹${p.entry_price.toFixed(2)}</span>
                <span class="position-pnl ${p.pnl >= 0 ? 'positive' : 'negative'}">
                    ${p.pnl >= 0 ? '+' : ''}₹${p.pnl.toFixed(2)}
                </span>
            </div>
        </div>
    `).join('');
}

function renderTrades(trades) {
    const container = document.getElementById('trades-list');

    if (trades.length === 0) {
        container.innerHTML = '<p class="empty-state">No trades yet</p>';
        return;
    }

    // Reverse to show newest first
    container.innerHTML = trades.reverse().slice(0, 50).map(t => `
        <div class="trade-item">
            <div class="trade-header">
                <span class="trade-time">${formatTime(t.timestamp)}</span>
                <span class="trade-action ${t.action.toLowerCase()}">${t.action}</span>
            </div>
            <div class="trade-symbol">${t.symbol}</div>
            <div class="trade-details">
                ${t.quantity} @ ₹${t.fill_price.toFixed(2)}
                ${t.net_pnl ? ` | P&L: ₹${t.net_pnl.toFixed(2)}` : ''}
            </div>
        </div>
    `).join('');
}

function populateConfigForm(config) {
    document.getElementById('cfg-strategy').value = config.strategy || 'sniper';
    document.getElementById('cfg-balance').value = config.session_balance || 1000;
    document.getElementById('cfg-stoploss').checked = config.stop_loss_enabled !== false;
    document.getElementById('cfg-stoploss-pct').value = config.stop_loss_percent || 5;
}

// ─────────────────────────────────────────────────────────────────
// WEBSOCKET
// ─────────────────────────────────────────────────────────────────

function connect() {
    const wsUrl = CONFIG.serverUrl.replace('http', 'ws') + '/ws/live';

    try {
        ws = new WebSocket(wsUrl);

        ws.onopen = () => {
            console.log('WebSocket connected');
            setConnectionStatus(true);
            reconnectAttempts = 0;
        };

        ws.onmessage = (event) => {
            const data = JSON.parse(event.data);
            handleWebSocketMessage(data);
        };

        ws.onclose = () => {
            console.log('WebSocket closed');
            setConnectionStatus(false);
            attemptReconnect();
        };

        ws.onerror = (error) => {
            console.error('WebSocket error:', error);
        };
    } catch (e) {
        console.error('WebSocket connection failed:', e);
    }
}

function attemptReconnect() {
    if (reconnectAttempts < MAX_RECONNECT_ATTEMPTS) {
        reconnectAttempts++;
        const delay = Math.min(1000 * Math.pow(2, reconnectAttempts), 30000);
        console.log(`Reconnecting in ${delay}ms (attempt ${reconnectAttempts})...`);
        setTimeout(connect, delay);
    }
}

function handleWebSocketMessage(data) {
    switch (data.type) {
        case 'heartbeat':
            setConnectionStatus(true);
            break;
        case 'update':
            updateDashboard(data);
            break;
        case 'status':
            loadStatus();
            break;
        case 'position':
            loadPositions();
            break;
        case 'trade':
            loadTrades();
            break;
    }
}

// ─────────────────────────────────────────────────────────────────
// NAVIGATION & SETTINGS
// ─────────────────────────────────────────────────────────────────

function showTab(tabId) {
    // Hide all tabs
    document.querySelectorAll('.tab-content').forEach(tab => {
        tab.classList.remove('active');
    });

    // Show selected tab
    document.getElementById(tabId).classList.add('active');

    // Update nav buttons
    document.querySelectorAll('.nav-btn').forEach(btn => {
        btn.classList.remove('active');
    });
    event.currentTarget.classList.add('active');

    // Load data for tab
    if (tabId === 'history') {
        loadTrades();
    }
}

function showSettings() {
    document.getElementById('server-url').value = CONFIG.serverUrl;
    document.getElementById('api-key').value = CONFIG.apiKey;
    document.getElementById('settings-modal').classList.remove('hidden');
}

function closeSettings() {
    document.getElementById('settings-modal').classList.add('hidden');
}

function saveSettings() {
    CONFIG.serverUrl = document.getElementById('server-url').value;
    CONFIG.apiKey = document.getElementById('api-key').value;

    localStorage.setItem('serverUrl', CONFIG.serverUrl);
    localStorage.setItem('apiKey', CONFIG.apiKey);

    closeSettings();
    connect();
    refreshAll();
    showToast('Connected!', 'success');
}

// ─────────────────────────────────────────────────────────────────
// HELPERS
// ─────────────────────────────────────────────────────────────────

function setConnectionStatus(connected) {
    const bar = document.getElementById('status-bar');
    const status = document.getElementById('connection-status');

    if (connected) {
        bar.classList.remove('disconnected');
        bar.classList.add('connected');
        status.textContent = '🟢 Connected';
    } else {
        bar.classList.remove('connected');
        bar.classList.add('disconnected');
        status.textContent = '🔴 Disconnected';
    }
}

function formatCurrency(value, showSign = false) {
    const absValue = Math.abs(value || 0);
    const sign = showSign && value >= 0 ? '+' : (value < 0 ? '-' : '');
    return sign + '₹' + absValue.toFixed(2);
}

function formatTime(timestamp) {
    if (!timestamp) return '--';
    const date = new Date(timestamp);
    return date.toLocaleTimeString('en-IN', { hour: '2-digit', minute: '2-digit' });
}

function showToast(message, type = 'info') {
    // Simple toast - create element
    const toast = document.createElement('div');
    toast.style.cssText = `
        position: fixed;
        bottom: 80px;
        left: 50%;
        transform: translateX(-50%);
        background: ${type === 'error' ? '#ff6b6b' : '#00d26a'};
        color: ${type === 'error' ? '#fff' : '#000'};
        padding: 12px 24px;
        border-radius: 8px;
        font-weight: 600;
        z-index: 300;
        animation: slideUp 0.3s ease;
    `;
    toast.textContent = message;
    document.body.appendChild(toast);

    setTimeout(() => {
        toast.remove();
    }, 3000);
}

// Add toast animation
const style = document.createElement('style');
style.textContent = `
    @keyframes slideUp {
        from { opacity: 0; transform: translateX(-50%) translateY(20px); }
        to { opacity: 1; transform: translateX(-50%) translateY(0); }
    }
`;
document.head.appendChild(style);
