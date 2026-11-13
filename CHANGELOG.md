# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Multiple listeners per server. `-addr` and `-http` accept a comma separated list, and each entry
  may be `public:PORT` or `lan:PORT` to bind the internet-facing or the local network address.
  `-listen dual` is the one-flag preset for "public + LAN".
- Public and LAN listeners share one stream registry: they are front doors to the same server,
  not separate instances.
- `-public-ip` (explicit, `auto`, or `off`) and `-lan-ip` override address discovery; env
  fallbacks `FASTRTMP_LISTEN`, `FASTRTMP_PUBLIC_IP`, `FASTRTMP_LAN_IP`.
- Startup banner now reports public / LAN / loopback URLs plus firewall and port-forwarding hints.
- Public IP detection races several echo services with a 4 s cap and never blocks startup.
- LAN address selection prefers the interface carrying the default route and ranks virtual
  adapters (VirtualBox, VMware, Hyper-V, WSL, Docker) last.
- Dashboard password: `-admin-password` (env `FASTRTMP_ADMIN_PASSWORD`) puts a login page in
  front of the panel with a session cookie, five-attempt lockout per IP, and a sign-out button.
- Key rotation from the dashboard: `/api/config` reads the current settings and rewrites the
  publish/play key live, no restart required. The panel's example URLs pick up `?key=`
  automatically, and a "generate" button produces a random one.
- `/api/login`, `/api/logout`, `/api/config` endpoints. `/api/stats` and the panel itself now
  require a session when a password is configured.

### Fixed
- A public address that is not on a local NIC (NAT) no longer fails startup: `public:` degrades to
  a wildcard bind, logs a WARN and points at port forwarding. On a VPS the address is bound exactly.
- Redundant bindings are dropped when a wildcard listener already covers the port, avoiding
  `address already in use`.
- `-play-key` did not guard HTTP-FLV: `/flv/...` served the stream to anyone. It now checks the
  key like RTMP play does.
- The startup banner no longer prints the secrets themselves, only whether auth is enabled.
- Without `-admin-password` the dashboard cannot read or rotate stream keys, so an open panel is
  not a back door to the ingest secret.

### Planned
- Optional RTMPS listener
- HLS output
- Recording (FLV/MP4 segmenter)
- Prometheus metrics endpoint
- Admin API (kick publisher/viewer)
- Multiple dashboard accounts instead of one shared password
- Complex (digest) handshake support

## [1.0.0] - 2026-11-08

First public release.

### Added
- RTMP server written from scratch: simple handshake, chunk stream (fmt 0–3, extended
  timestamps), AMF0, and the `connect` / `createStream` / `publish` / `play` command flow.
- RTMP publish and play, plus HTTP-FLV output for browsers (mpegts.js) and `ffplay`.
- Per-stream GOP cache (metadata + codec config + last GOP) for instant playback start.
- Per-viewer send queue with drop-to-keyframe protection for slow clients.
- Embedded bilingual (EN/中文) dashboard with live preview.
- `/api/stats`, `/healthz`, optional pprof, `-publish-key` / `-play-key` auth.
- `rtmptest` publish / play / load-test client with real one-way latency measurement.
- Cross-platform builds: Linux amd64/arm64, Windows amd64, macOS amd64/arm64.
- systemd unit, Dockerfile, Makefile, release script, GitHub Actions CI and release workflows.

### Fixed during development (covered by tests)
- AMF0 object property names were encoded with a type marker, corrupting command objects.
- Continuation chunk headers used `fmt=1` instead of `fmt=3`, desynchronising multi-chunk messages.
- `removePublisher` released the stream lock twice.
- Streams were not namespaced by RTMP app, so `live/test` and `test` collided and HTTP-FLV
  could not find a stream published over RTMP.
- HTTP-FLV clients that disconnected while the stream was idle leaked a connection and a viewer slot.

[Unreleased]: https://github.com/Hydroxide-OH/FastRTMP/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/Hydroxide-OH/FastRTMP/releases/tag/v1.0.0
