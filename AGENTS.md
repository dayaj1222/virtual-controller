# AGENTS.md — Xbox Controller Proxy

Compact brief for future agents. Read this instead of re-reading every file.

## What it is
Go server + phone web client that turns a phone into a **virtual Xbox 360 controller**.
Phone connects over LAN via WebSocket; the server injects touch input as real gamepad events through `/dev/uinput`.

## Architecture

| File | Role |
|---|---|
| `main.go` (774 lines) | uinput device lifecycle, raw `ioctl`/`syscall` (pure Go, no cgo), ordered event writer, WebSocket `/ws`, HTTP `GET/PUT /api/config` + `/api/layout`, D-Bus idle inhibitor, QR-code startup printout |
| `client/index.html` (559 lines) | Touch gamepad UI. Zone-based sticks (press-point-is-center), analog triggers, face buttons, d-pad, drag/resize edit mode, settings panel. Catppuccin Mocha palette, Lucide-style but plain text glyphs |
| `layout.json` | Saved control layout: `{id: {x,y,w,h}` in %`}` + `_grid` snap value |
| `.env` | Runtime config (STICK_DEADZONE, FLUSH_INTERVAL_MS, TRIGGER_THRESHOLD, LISTEN) |

## Key implementation details

- **Pure-Go uinput**: hand-computed `ioctl` request numbers (`ioc()`, `iow()`, `ioctlIO()`) + mirrored `<linux/uinput.h>` structs. No cgo.
- **Ordered writer**: single goroutine + buffered channel. Buttons flush immediately (low latency); analog axes batch to ~8ms ticker; `EV_SYN` (`type:"sync"`) marks flush boundary. Guarantees total event ordering.
- **Device spoofing**: reports vendor `0x045e` (Microsoft), product `0x028e` (Xbox 360 wired), name "Xbox 360 Controller" so Wine/games auto-recognize.
- **Hot-reload config**: `PUT /api/config` mutates in-memory `appCfg` AND rewrites `.env`. Note `listen` addr change needs restart.
- **D-Bus idle inhibitor**: pokes `ScreenSaver.SimulateUserActivity` when input is recent (35s window) to prevent screen blanking.
- **QR code** printed on startup → scan to connect phone.

## Event codes (WebSocket protocol)

- `{"type":"key","code":"A|B|X|Y|LB|RB|LT|RT|BACK|START|GUIDE|THUMBL|THUMBR|DPAD_UP|DPAD_DOWN|DPAD_LEFT|DPAD_RIGHT","value":0|1}`
- `{"type":"abs","code":"LX|LY|RX|RY|LT|RT|HATX|HATY","value":float}` — sticks/triggers use -1..1 (or 0..1), scaled to kernel ranges in `axisByCode`
- `{"type":"sync"}` — flush marker

## Conventions

- Go 1.26, deps: godbus/v5, gorilla/websocket, skip2/go-qrcode, golang.org/x/sys.
- Config file `.env` is read at startup, rewritten on PUT (upsert key=value lines).
- Layout JSON keys map to `.ctrl` elements' `data-id` in index.html.

## Gotchas / notes

- `git status` last showed uncommitted changes in `client/index.html` and `main.go`.
- Tags exist: `v1.0.0`, `v1.0.1`. GitHub Actions builds + releases on tag (see `.github/workflows/release.yml`).
