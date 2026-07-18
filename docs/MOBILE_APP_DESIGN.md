# TitanAlgo Mobile App - Design Document v3

> **Status**: Production-Ready Design (After 3 Review Cycles)  
> **Last Updated**: January 2026  
> **Author**: AI System Architect

---

## Executive Summary

Build a minimal, lightweight Android app to remotely control the TitanAlgo trading engine. The app will communicate with the Go backend via a REST API layer.

**Key Decisions:**
- **Architecture**: WebView PWA (< 2MB APK)
- **Backend**: REST API + WebSocket
- **Security**: API key authentication + localhost restriction
- **Offline**: Local asset caching

---

## Architecture Options Considered

| Option | Pros | Cons | Verdict |
|--------|------|------|---------|
| **Native Kotlin** | Best performance | Only Android, complex, 10MB+ | ❌ Overkill |
| **React Native** | Cross-platform | 50MB+ APK, Node.js needed | ❌ Too heavy |
| **Flutter** | Cross-platform | Dart, 15MB+ APK | ❌ Not minimal |
| **WebView PWA** | <2MB, single codebase, easy updates | Not "fully native" | ✅ **SELECTED** |

---

## Final Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                     ANDROID DEVICE                          │
│  ┌───────────────────────────────────────────────────────┐  │
│  │              WebView PWA App (< 2MB)                  │  │
│  │  ┌─────────────┐  ┌─────────────┐  ┌──────────────┐   │  │
│  │  │  Dashboard  │  │   Config    │  │   Trades     │   │  │
│  │  │   Screen    │  │   Screen    │  │   History    │   │  │
│  │  └─────────────┘  └─────────────┘  └──────────────┘   │  │
│  └───────────────────────┼───────────────────────────────┘  │
└──────────────────────────┼──────────────────────────────────┘
                           │ HTTPS / Local Network
                           ▼
┌─────────────────────────────────────────────────────────────┐
│                    GO BACKEND (PC/Server)                   │
│  ┌───────────────────────────────────────────────────────┐  │
│  │              API Server (Port 8080)                   │  │
│  │    GET  /api/status     - Engine status               │  │
│  │    GET  /api/positions  - Open positions              │  │
│  │    GET  /api/trades     - Trade history               │  │
│  │    GET  /api/config     - Current config              │  │
│  │    POST /api/config     - Update config               │  │
│  │    POST /api/start      - Start trading               │  │
│  │    POST /api/stop       - Stop trading                │  │
│  │    WS   /ws/live        - Real-time updates           │  │
│  └───────────────────────────────────────────────────────┘  │
│                  Existing Trading Engine                     │
└─────────────────────────────────────────────────────────────┘
```

---

## Critical Self-Review Summary

### Security (Review Cycle 1)
- ✅ API key header authentication (`X-API-Key`)
- ✅ CORS restricted to mobile app origin
- ✅ Rate limiting (10 req/sec)
- ✅ Config changes require confirmation

### Reliability (Review Cycle 2)
- ✅ Heartbeat every 5 seconds
- ✅ Connection status indicator (🟢/🔴)
- ✅ WebSocket auto-reconnect with backoff
- ✅ "Last updated" timestamps

### Usability (Review Cycle 3)
- ✅ Single-page app with 3 tabs
- ✅ Form fields (not raw YAML)
- ✅ Read-only trades (start/stop only)
- ✅ Pause updates when backgrounded

---

## API Endpoints

### GET /api/status
```json
{
  "running": true,
  "mode": "paper",
  "strategy": "sniper",
  "balance": 1000.00,
  "unrealized_pnl": 45.50,
  "realized_pnl": 120.00,
  "positions_count": 2
}
```

### GET /api/positions
```json
{
  "positions": [{
    "symbol": "NIFTY27JAN2625000PE",
    "side": "BUY",
    "quantity": 65,
    "entry_price": 45.50,
    "current_price": 48.00,
    "pnl": 162.50
  }]
}
```

### POST /api/config
```json
{"session_balance": 2000.0, "stop_loss_percent": 3.0}
```

### POST /api/start
```json
{"mode": "paper", "strategy": "sniper"}
```

---

## Mobile App Screens

### Dashboard (Home)
- Balance display
- Unrealized/Realized P&L
- Open positions list
- Start/Stop button

### Configuration
- Strategy dropdown
- Session balance input
- Stop-loss toggle + percentage
- Discovery indices checkboxes
- Save button

### Trade History
- Chronological trade list
- Buy/Sell with prices
- P&L per trade

---

## File Structure

```
titan-algo/
├── mobile-app/
│   ├── android/
│   │   ├── app/src/main/
│   │   │   ├── java/.../MainActivity.kt
│   │   │   ├── res/layout/activity_main.xml
│   │   │   └── assets/www/
│   │   │       ├── index.html
│   │   │       ├── app.js
│   │   │       └── style.css
│   │   └── build.gradle
│   └── README.md
│
└── go-engine/
    └── internal/api/
        └── server.go
```

---

## Implementation Phases

| Phase | Component | Time |
|-------|-----------|------|
| 1 | Backend REST API | 2-3h |
| 2 | Web UI (HTML/JS) | 2-3h |
| 3 | Android Wrapper | 1h |
| 4 | Testing | 1h |
| **Total** | | **6-8h** |

---

## Production Checklist

- [x] API key authentication
- [x] HTTPS or localhost-only
- [x] Rate limiting
- [x] Heartbeat monitoring
- [x] Auto-reconnect
- [x] Single-page app
- [x] Form-based config
- [x] <2MB APK
- [x] WebSocket (not polling)
- [x] Offline asset caching

---

## Risk Assessment

| Risk | Probability | Impact | Mitigation |
|------|-------------|--------|------------|
| Network latency | Medium | Low | Loading states, timeout |
| Backend unreachable | Medium | High | Offline indicator, retry |
| Unauthorized access | Low | Critical | API key + localhost |
| Config errors | Medium | High | Validation + confirmation |

---

## Conclusion

This design creates a minimal, secure, and reliable mobile control panel for TitanAlgo. The WebView PWA approach ensures:
- **< 2MB APK** size
- **Easy updates** (just update web assets)
- **Cross-platform ready** (iOS later)
- **Production-grade security** with API keys

**Ready for implementation.** ✅
