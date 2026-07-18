import streamlit as st
import pandas as pd
import plotly.graph_objects as go
from plotly.subplots import make_subplots
from pathlib import Path
import time

# Page configuration
st.set_page_config(
    page_title="TitanAlgo Dashboard",
    page_icon="📈",
    layout="wide",
    initial_sidebar_state="expanded"
)

# Custom CSS
st.markdown("""
<style>
    .main-header {
        font-size: 2.5rem;
        font-weight: bold;
        color: #1f77b4;
        text-align: center;
        margin-bottom: 1rem;
    }
    .metric-card {
        background-color: #f0f2f6;
        padding: 1rem;
        border-radius: 0.5rem;
        text-align: center;
    }
    .profit {
        color: #00c853;
        font-weight: bold;
    }
    .loss {
        color: #d32f2f;
        font-weight: bold;
    }
    .alert-warning {
        background-color: #fff3cd;
        border-left: 4px solid #ffc107;
        padding: 1rem;
        margin: 1rem 0;
    }
    .alert-danger {
        background-color: #f8d7da;
        border-left: 4px solid #dc3545;
        padding: 1rem;
        margin: 1rem 0;
    }
    .alert-success {
        background-color: #d4edda;
        border-left: 4px solid #28a745;
        padding: 1rem;
        margin: 1rem 0;
    }
</style>
""", unsafe_allow_html=True)

# Title
st.markdown('<div class="main-header">🚀 TitanAlgo Trading Dashboard</div>', unsafe_allow_html=True)

# CSV file path
# Check both paper and live mode paths, use whichever exists/has more recent data
SCRIPT_DIR = Path(__file__).parent
CSV_PATH_PAPER = (SCRIPT_DIR / "../../go-engine/logs/trades.csv").resolve()
CSV_PATH_LIVE = (SCRIPT_DIR / "../../go-engine/logs/live/trades.csv").resolve()

# Use live path if it exists, otherwise paper path
if CSV_PATH_LIVE.exists():
    CSV_PATH = CSV_PATH_LIVE
    print(f"DEBUG: Using LIVE mode CSV: {CSV_PATH}")
elif CSV_PATH_PAPER.exists():
    CSV_PATH = CSV_PATH_PAPER
    print(f"DEBUG: Using PAPER mode CSV: {CSV_PATH}")
else:
    CSV_PATH = CSV_PATH_PAPER  # Default, will show "no data" message
    print(f"DEBUG: No CSV found, defaulting to: {CSV_PATH}")

# Sidebar controls
st.sidebar.title("⚙️ Controls")
auto_refresh = st.sidebar.checkbox("Auto-refresh (10s)", value=True)
if auto_refresh:
    st.sidebar.info("Dashboard refreshing every 10 seconds")

if st.sidebar.button("🔄 Refresh Now"):
    st.rerun()

# Load data
@st.cache_data(ttl=10)
def load_data():
    try:
        if not CSV_PATH.exists():
            return None
        
        df = pd.read_csv(CSV_PATH)
        
        # Convert Timestamp to datetime
        df['Timestamp'] = pd.to_datetime(df['Timestamp'])
        
        return df
    except Exception as e:
        st.error(f"Error loading data: {e}")
        return None

df = load_data()

if df is None or len(df) == 0:
    st.warning("⚠️ No trading data available yet. Start the Go engine with `-paper` flag to generate trades.")
    st.code("cd go-engine && go run cmd/main.go -paper -balance 1000", language="bash")
    st.stop()

# Calculate metrics
initial_risk_balance = 1000.0  # Default, should match -balance flag
current_risk_balance = df['RiskBalance'].iloc[-1]
current_broker_balance = df['BrokerBalance'].iloc[-1]
total_pnl = df['NetPnL'].iloc[-1]
pnl_percentage = (total_pnl / initial_risk_balance) * 100
total_trades = len(df)

# Count wins and losses (only on SELL orders)
sell_trades = df[df['Action'] == 'SELL']
wins = len(sell_trades[sell_trades['NetPnL'] > 0])
losses = len(sell_trades[sell_trades['NetPnL'] < 0])
win_rate = (wins / len(sell_trades) * 100) if len(sell_trades) > 0 else 0

# Calculate session usage
session_usage = ((initial_risk_balance - current_risk_balance) / initial_risk_balance) * 100
available_balance = current_risk_balance

# Risk Alerts
alerts = []
if available_balance < initial_risk_balance * 0.2:
    alerts.append(("danger", f"⚠️ LOW BALANCE: Only ₹{available_balance:.2f} remaining (20% of initial)"))
elif available_balance < initial_risk_balance * 0.5:
    alerts.append(("warning", f"⚡ BALANCE WARNING: ₹{available_balance:.2f} remaining (50% of initial)"))

if total_pnl < 0 and abs(pnl_percentage) > 5:
    alerts.append(("danger", f"📉 MAX DRAWDOWN ALERT: {pnl_percentage:.2f}% loss detected!"))
elif total_pnl < 0 and abs(pnl_percentage) > 2:
    alerts.append(("warning", f"📊 Drawdown Warning: {pnl_percentage:.2f}% loss"))

if total_pnl > 0:
    alerts.append(("success", f"💰 PROFIT: ₹{total_pnl:.2f} ({pnl_percentage:.2f}%)"))

# Display Alerts
if alerts:
    st.markdown("### 🚨 Risk Alerts")
    for alert_type, message in alerts:
        st.markdown(f'<div class="alert-{alert_type}">{message}</div>', unsafe_allow_html=True)

# KPI Row
st.markdown("### 📊 Key Performance Indicators")
col1, col2, col3, col4, col5 = st.columns(5)

with col1:
    st.metric(
        label="Risk Balance",
        value=f"₹{current_risk_balance:,.2f}",
        delta=f"{total_pnl:.2f}"
    )

with col2:
    st.metric(
        label="Broker Balance",
        value=f"₹{current_broker_balance:,.2f}",
        delta=f"{current_broker_balance - 1000000:.2f}"
    )

with col3:
    st.metric(
        label="Total P&L",
        value=f"₹{total_pnl:,.2f}",
        delta=f"{pnl_percentage:.2f}%"
    )

with col4:
    st.metric(
        label="Win Rate",
        value=f"{win_rate:.1f}%",
        delta=f"{wins}W / {losses}L"
    )

with col5:
    st.metric(
        label="Total Trades",
        value=total_trades
    )

# Dual Balance Chart
st.markdown("### 📈 Balance Tracking")

fig = make_subplots(
    rows=2, cols=1,
    subplot_titles=('Risk Balance (Session Limit)', 'Broker Balance (Virtual)'),
    vertical_spacing=0.15,
    row_heights=[0.5, 0.5]
)

# Risk Balance Chart
fig.add_trace(
    go.Scatter(
        x=df['Timestamp'],
        y=df['RiskBalance'],
        mode='lines+markers',
        name='Risk Balance',
        line=dict(color='#ff6b6b', width=3),
        marker=dict(size=8),
        fill='tozeroy',
        fillcolor='rgba(255, 107, 107, 0.1)'
    ),
    row=1, col=1
)

# Add initial risk balance line
fig.add_hline(
    y=initial_risk_balance,
    line_dash="dash",
    line_color="gray",
    annotation_text=f"Initial: ₹{initial_risk_balance}",
    annotation_position="right",
    row=1, col=1
)

# Broker Balance Chart
fig.add_trace(
    go.Scatter(
        x=df['Timestamp'],
        y=df['BrokerBalance'],
        mode='lines+markers',
        name='Broker Balance',
        line=dict(color='#4ecdc4', width=3),
        marker=dict(size=8),
        fill='tozeroy',
        fillcolor='rgba(78, 205, 196, 0.1)'
    ),
    row=2, col=1
)

# Add initial broker balance line
fig.add_hline(
    y=1000000,
    line_dash="dash",
    line_color="gray",
    annotation_text="Initial: ₹10,00,000",
    annotation_position="right",
    row=2, col=1
)

fig.update_xaxes(title_text="Time", row=2, col=1)
fig.update_yaxes(title_text="Risk Balance (₹)", row=1, col=1)
fig.update_yaxes(title_text="Broker Balance (₹)", row=2, col=1)

fig.update_layout(
    height=600,
    hovermode='x unified',
    showlegend=False
)

st.plotly_chart(fig, use_container_width=True)

# Risk Metrics Row
st.markdown("### 🎯 Risk Metrics")
col1, col2, col3, col4 = st.columns(4)

with col1:
    st.metric(
        label="Available Balance",
        value=f"₹{available_balance:,.2f}",
        delta=f"{(available_balance/initial_risk_balance)*100:.1f}% of initial"
    )

with col2:
    drawdown = abs(min(0, total_pnl))
    drawdown_pct = (drawdown / initial_risk_balance) * 100
    st.metric(
        label="Max Drawdown",
        value=f"₹{drawdown:,.2f}",
        delta=f"-{drawdown_pct:.2f}%",
        delta_color="inverse"
    )

with col3:
    # Count open positions (BUY without corresponding SELL)
    open_positions = 0
    for symbol in df['Symbol'].unique():
        symbol_trades = df[df['Symbol'] == symbol]
        buys = len(symbol_trades[symbol_trades['Action'] == 'BUY'])
        sells = len(symbol_trades[symbol_trades['Action'] == 'SELL'])
        if buys > sells:
            open_positions += 1
    
    st.metric(
        label="Open Positions",
        value=open_positions
    )

with col4:
    avg_trade_size = df['Quantity'].mean()
    st.metric(
        label="Avg Trade Size",
        value=f"{avg_trade_size:.1f} shares"
    )

# Recent Activity
st.markdown("### 📋 Recent Trades (Last 10)")

recent_df = df.tail(10).copy()
recent_df = recent_df.sort_values('Timestamp', ascending=False)

# Format for display
display_df = recent_df[['Timestamp', 'Symbol', 'Action', 'Quantity', 'FillPrice', 'Slippage', 'TransactionFee', 'RiskBalance', 'NetPnL']].copy()
display_df['Timestamp'] = display_df['Timestamp'].dt.strftime('%Y-%m-%d %H:%M:%S')
display_df['FillPrice'] = display_df['FillPrice'].apply(lambda x: f"₹{x:.2f}")
display_df['Slippage'] = display_df['Slippage'].apply(lambda x: f"₹{x:.2f}")
display_df['TransactionFee'] = display_df['TransactionFee'].apply(lambda x: f"₹{x:.2f}")
display_df['RiskBalance'] = display_df['RiskBalance'].apply(lambda x: f"₹{x:,.2f}")
display_df['NetPnL'] = display_df['NetPnL'].apply(lambda x: f"₹{x:.2f}")

# Color code P&L
def color_pnl(val):
    if 'NetPnL' in val.name:
        num = float(val.replace('₹', '').replace(',', ''))
        color = 'green' if num > 0 else 'red' if num < 0 else 'gray'
        return [f'color: {color}' for _ in val]
    return ['' for _ in val]

st.dataframe(display_df, use_container_width=True, hide_index=True)

# P&L Distribution
st.markdown("### 💹 P&L Distribution")
col1, col2 = st.columns(2)

with col1:
    # Cumulative P&L Chart
    fig_pnl = go.Figure()
    
    # Calculate cumulative P&L
    df_sorted = df.sort_values('Timestamp')
    cumulative_pnl = df_sorted['NetPnL'].cumsum()
    
    fig_pnl.add_trace(go.Scatter(
        x=df_sorted['Timestamp'],
        y=cumulative_pnl,
        mode='lines+markers',
        name='Cumulative P&L',
        line=dict(color='#6c5ce7', width=2),
        marker=dict(size=6),
        fill='tozeroy',
        fillcolor='rgba(108, 92, 231, 0.1)'
    ))
    
    fig_pnl.update_layout(
        title="Cumulative P&L Over Time",
        xaxis_title="Time",
        yaxis_title="Cumulative P&L (₹)",
        height=300,
        hovermode='x unified'
    )
    
    st.plotly_chart(fig_pnl, use_container_width=True)

with col2:
    # Trade P&L Distribution
    sell_trades_pnl = df[df['Action'] == 'SELL']['NetPnL']
    
    fig_dist = go.Figure()
    fig_dist.add_trace(go.Histogram(
        x=sell_trades_pnl,
        nbinsx=20,
        marker_color='#a29bfe',
        name='Trade P&L'
    ))
    
    fig_dist.update_layout(
        title="Trade P&L Distribution",
        xaxis_title="P&L (₹)",
        yaxis_title="Frequency",
        height=300
    )
    
    st.plotly_chart(fig_dist, use_container_width=True)

# Trade Statistics
st.markdown("### 📊 Trade Statistics")
col1, col2, col3 = st.columns(3)

with col1:
    # By Symbol
    symbol_counts = df['Symbol'].value_counts()
    fig_symbol = go.Figure(data=[go.Pie(
        labels=symbol_counts.index,
        values=symbol_counts.values,
        hole=0.3
    )])
    fig_symbol.update_layout(title="Trades by Symbol", height=300)
    st.plotly_chart(fig_symbol, use_container_width=True)

with col2:
    # By Action
    action_counts = df['Action'].value_counts()
    fig_action = go.Figure(data=[go.Bar(
        x=action_counts.index,
        y=action_counts.values,
        marker_color=['#00c853', '#d32f2f']
    )])
    fig_action.update_layout(title="Buy vs Sell", height=300, showlegend=False)
    st.plotly_chart(fig_action, use_container_width=True)

with col3:
    # Average Slippage
    avg_slippage = df['Slippage'].mean()
    max_slippage = df['Slippage'].max()
    
    st.markdown("#### Slippage Analysis")
    st.metric("Average Slippage", f"₹{avg_slippage:.2f}")
    st.metric("Max Slippage", f"₹{max_slippage:.2f}")
    st.metric("Total Slippage Cost", f"₹{df['Slippage'].sum():.2f}")

# Footer
st.markdown("---")
col1, col2, col3 = st.columns(3)
with col1:
    st.markdown(f"**Session Start:** {df['Timestamp'].iloc[0].strftime('%Y-%m-%d %H:%M:%S')}")
with col2:
    st.markdown(f"**Last Update:** {df['Timestamp'].iloc[-1].strftime('%Y-%m-%d %H:%M:%S')}")
with col3:
    st.markdown(f"**Duration:** {(df['Timestamp'].iloc[-1] - df['Timestamp'].iloc[0]).total_seconds():.0f}s")

st.markdown(
    '<div style="text-align: center; color: gray; margin-top: 1rem;">🧪 Paper Trading Mode | TitanAlgo v2.0 | Risk-Managed Trading</div>',
    unsafe_allow_html=True
)

# Auto-refresh logic using proper Streamlit pattern
# Note: time.sleep() blocks the entire UI, so we use fragment-based approach
if auto_refresh:
    # Create a placeholder for countdown
    refresh_placeholder = st.sidebar.empty()
    
    # Use session state to track refresh
    if 'last_refresh' not in st.session_state:
        st.session_state.last_refresh = time.time()
    
    time_since_refresh = time.time() - st.session_state.last_refresh
    time_until_refresh = max(0, 10 - time_since_refresh)
    
    if time_since_refresh >= 10:
        st.session_state.last_refresh = time.time()
        st.rerun()
    else:
        # Show countdown (non-blocking)
        refresh_placeholder.caption(f"⏱️ Refreshing in {int(time_until_refresh)}s...")
        # Use streamlit's native auto-rerun with query params trick
        import streamlit.components.v1 as components
        components.html(
            f"""<script>
                setTimeout(function() {{
                    window.parent.location.reload();
                }}, {int(time_until_refresh * 1000)});
            </script>""",
            height=0
        )
