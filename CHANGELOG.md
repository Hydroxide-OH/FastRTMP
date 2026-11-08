# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Planned
- Optional RTMPS listener
- HLS output
- Recording (FLV/MP4 segmenter)
- Prometheus metrics endpoint
- Admin API (kick publisher/viewer)
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
