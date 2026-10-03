# Changelog

## 1.0.0

First production release.

- Supervises 1–500 Tor instances (default 200), each with its own SOCKS5 port on loopback.
- Startup is seeded from a shared directory cache and runs with bounded parallelism. An instance counts as ready only when its SOCKS port is open and bootstrap reaches 100 %.
- Auto-restart with exponential backoff, periodic health checks, and a Job Object so no tor.exe is orphaned.
- Embedded dark dashboard with live SSE: endpoint table and heatmap, per-instance logs, NEWNYM, verification through check.torproject.org, proxy export, diagnostics export, and the settings editor.
- Tray icon whose colour reflects status, with a context menu and notifications.
- Hardened local API: Host allow-list, CSRF header, Origin check, CSP, no CORS.
- CLI: `--headless`, `--list-proxies`, `--check-config`, and per-run overrides.
- Build script that embeds icon and version resources, creates a zip and SHA256 sums.
- `download-tor.ps1` fetches and verifies the official Tor Expert Bundle.
