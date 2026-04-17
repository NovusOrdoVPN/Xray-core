# TUN Proxy — Client vs Upstream v26.4.15

## Executive Summary

**Verdict: MIGRATION_SAFE** — with a zero-effort caveat. The iOS fd-injection path is 100% identical in both forks (same env-var key, same `tun_darwin.go` dual-mode branching, same protobuf field numbers for the 3 fields VPNTool uses). The client's TUN is essentially an earlier snapshot of upstream's TUN with fewer bells & whistles. Upstream's version is a strict superset — new config fields default to zero/empty and are cleanly ignored by the existing Dart config. No Swift-side changes required. No Go patches to port. Dart changes are optional (only if we want auto-system-routing-table).

## Side-by-Side File Comparison

| File | Client | Upstream v26.4.15 | Delta |
|---|---|---|---|
| `config.proto` | 3 fields (name, MTU, user_level) | 7 fields (+gateway, DNS, auto_system_routing_table, auto_outbounds_interface) | Backward compatible — field numbers 1, 2, user_level renumbered 3→5 |
| `config.go` | Empty (`package tun`) | 79 lines — `InterfaceUpdater` for auto-outbounds-interface | Pure addition |
| `config.pb.go` | 4285 bytes | 5618 bytes | Regenerated from new proto |
| `handler.go` | 169 lines | 195 lines | Adds `AutoOutboundsInterface` block + `Close()` method + stores `tun` ref |
| `tun.go` | `Tun{Start,Close}`, `TunOptions{Name,MTU}` | `Tun{Start,Close,Name,Index,newEndpoint}` | Interface gained `Name()`, `Index()`, and `newEndpoint` moved to interface; `TunOptions` gone — `*Config` passed directly |
| `tun_darwin.go` | 355 lines, iOS fd mode present | 389 lines, iOS fd mode present (IDENTICAL logic) | +`UTUN_OPT_IFNAME`, +`Name()`/`Index()` impls, +`setinterface()` helper, signature changed to `NewTun(*Config)` |
| `tun_default.go` | `!linux && !windows && !android && !darwin` | `!linux && !windows && !android && !darwin && !freebsd` | FreeBSD now supported |
| `tun_freebsd.go` | MISSING | 3631 bytes | New file — irrelevant to iOS |
| `tun_android.go` | 1197 bytes | 1699 bytes | Probably gained Name/Index; irrelevant to iOS |
| `tun_linux.go` | 2557 bytes | 2790 bytes | Minor growth; irrelevant to iOS |
| `tun_windows.go` | 3850 bytes | 8073 bytes | Major Windows improvements; irrelevant to iOS |
| `stack.go` | Identical | Identical | — |
| `stack_gvisor.go` | Uses `GVisorTun` interface | Uses `Tun` directly | Refactor; UDP handler signature changed (no bool return) |
| `stack_gvisor_endpoint.go` | Identical byte-for-byte | Identical byte-for-byte | Zero delta |
| `udp_fullcone.go` | Simple queue, `Mutex`, `chan []byte` size 16 | Richer: `RWMutex`, `packet` struct with dest, `chan *packet` size 1024, `io.EOF` handling, debug-log-on-drop | Performance improvement — better UDP under load |

## iOS File Descriptor Mechanism — CRITICAL VERIFIED IDENTICAL

Both forks use the SAME injection mechanism:

1. **Platform key** `/Users/arturdev/Developer/NovusOrdo/xray/xray_mobile_library/xray-core/common/platform/platform.go:28` → `TunFdKey = "xray.tun.fd"`
   Upstream equivalent at `/Users/arturdev/Developer/NovusOrdo/xray/Xray-core/common/platform/platform.go:26` → IDENTICAL string literal.

2. **Swift path** (`/Users/arturdev/Developer/NovusOrdo/VPNTool/ios/PacketTunnel/PacketTunnelProvider.swift:338`): scans fds 0..1024, `fcntl(fd, F_GETPATH, buf)`, matches `utun*`, hands off via `VpntoolcoreSetTunFd(Int(tunFd))`.

3. **gomobile wrapper** (`/Users/arturdev/Developer/NovusOrdo/xray/xray_mobile_library/xraymobile/controller.go:93`): sets `os.Setenv("XRAY_TUN_FD", strconv.Itoa(fd))` before calling xray. This is the gomobile binding `SetTunFd` — NOT a patch to xray-core itself. The xray-core Go code merely READS the env var.

4. **Client tun_darwin.go:52** and **upstream tun_darwin.go:52** — character-for-character identical iOS branch:
   ```go
   fdStr := platform.NewEnvFlag(platform.TunFdKey).GetValue(func() string { return "" })
   if fdStr != "" {
       // iOS: use provided fd from NetworkExtension
       fd, err := strconv.Atoi(fdStr)
       if err != nil { return nil, err }
       if err = unix.SetNonblock(fd, true); err != nil { return nil, err }
       return &DarwinTun{tunFile: os.NewFile(uintptr(fd), "utun"), ..., ownsFd: false}, nil
   }
   // macOS: create our own utun interface
   ```
   `ownsFd: false` path preserves the fd on Close (does NOT call `tunFile.Close()`) — NetworkExtension owns the lifetime. Identical behavior in both.

5. **No client-specific iOS patches exist.** The client's TUN is literally upstream-minus-features — every iOS-critical line is already upstream.

## Config Schema Diff & Migration Path

Client proto (3 fields):
```protobuf
string name = 1;
uint32 MTU = 2;
uint32 user_level = 3;
```

Upstream proto (7 fields):
```protobuf
string name = 1;
uint32 MTU = 2;
repeated string gateway = 3;
repeated string DNS = 4;
uint32 user_level = 5;              // <-- CHANGED FROM 3 TO 5
repeated string auto_system_routing_table = 6;
string auto_outbounds_interface = 7;
```

**Gotcha:** `user_level` tag moved from `3` to `5`. But VPNTool's Dart config passes JSON, not protobuf wire bytes — see `/Users/arturdev/Developer/NovusOrdo/VPNTool/lib/data/models/xray_outbound_config.dart:270` which emits `{name, MTU, userLevel}`. Xray's JSON-to-proto mapper uses field names, not numbers, so the renumbering is a non-issue. Verified: handler.go:60 passes `t.config` straight to `NewTun(*Config)`, and only `config.Name`, `config.MTU`, `config.UserLevel` are read on the iOS fd path. The new fields (`AutoOutboundsInterface` etc.) default to empty and skip the whole block in handler.go:65.

## iOS-Specific Behavior Preservation

| Concern | Client | Upstream | Safe? |
|---|---|---|---|
| Does `NewTun` accept pre-opened fd? | YES (env var branch) | YES (env var branch) | ✓ |
| Is `ownsFd: false` semantics preserved? | Close is a no-op | Close is a no-op | ✓ |
| Non-blocking set on fd? | `unix.SetNonblock(fd, true)` | `unix.SetNonblock(fd, true)` | ✓ |
| 4-byte Darwin header handled? | utunHeaderSize = 4 | utunHeaderSize = 4 | ✓ |
| gVisor link endpoint? | `LinkEndpoint` in stack_gvisor_endpoint.go | Same file, byte-identical | ✓ |
| UDP full-cone NAT? | Present, lighter | Present, richer | ✓ (upgrade) |

## Porting Recommendation

**Take upstream's TUN as-is. Do not port the client's version.**

Reasons:
1. iOS fd path is verbatim upstream — zero regression risk.
2. Upstream's UDP full-cone is a real improvement (1024-slot dest-aware queue vs 16-slot).
3. Upstream gains `auto_outbounds_interface` which would let us track the physical uplink (Wi-Fi ↔ cellular) without Swift plumbing — future-useful.
4. Upstream gains `Close()` on Handler — cleaner shutdown, the client currently leaks a bit on repeat connects.
5. Upstream's `Name()`/`Index()` on the Tun interface are a no-harm addition (iOS path doesn't call them unless `AutoOutboundsInterface` is set, which VPNTool doesn't set).

**What VPNTool (Dart) needs:** Nothing mandatory. Current `{name, MTU, userLevel}` continues to work. Optionally add `"autoOutboundsInterface": "auto"` later for uplink tracking.

**What NetworkExtension (Swift) needs:** Nothing. `VpntoolcoreSetTunFd`, fd scan, env-var export path — all unchanged.

**What the gomobile wrapper needs:** Nothing. `os.Setenv("XRAY_TUN_FD", …)` still lands in `platform.NewEnvFlag(platform.TunFdKey)` the same way.

## Risk Assessment

| Risk | Probability | Mitigation |
|---|---|---|
| Upstream's richer UDP queue changes timing and breaks a game/voice app | LOW | Queue size 1024 > 16 is strictly better; dest-aware routing fixes an existing bug where replies could cross conns |
| `user_level` proto renumbering breaks something | NEAR-ZERO | Dart passes JSON by name, not wire-format |
| `AutoOutboundsInterface` block triggers when empty and calls `tunInterface.Index()` on iOS | ZERO | handler.go:65 gates the entire block on `if t.config.AutoOutboundsInterface != ""` |
| `Tun` interface now requires `Name()` and `Index()` | NONE FOR iOS | `DarwinTun` implements both; iOS path never invokes them unless AutoOutboundsInterface is set |
| FreeBSD file adds unwanted code | NONE | `//go:build freebsd` — excluded from iOS builds |
| Behavior under rapid reconnect changes due to new `Handler.Close()` | LOW | Net positive — current code leaks the `stack` on repeated starts |

## Bottom Line

The user's recollection is correct: the client's TUN was copy-pasted from an intermediate upstream commit (pre the v26.1.23 feature additions) precisely to get iOS fd injection. The iOS fd mechanism that matters — env-var `xray.tun.fd` → `os.NewFile(fd)` → `ownsFd: false` — is part of upstream mainline and unchanged. Migrating to the unified fork's native `proxy/tun/` satisfies the user's constraint ("VPNTool's expectations met + iOS tun works"). Verified: every Swift call site and the gomobile `SetTunFd` wrapper work against upstream without modification.
