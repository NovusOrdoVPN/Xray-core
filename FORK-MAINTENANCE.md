# Fork Maintenance Guide

This fork of [XTLS/Xray-core](https://github.com/XTLS/Xray-core) carries custom
features for the NovusOrdo VPN service. This doc explains how the fork is
organized so you can sync with upstream cleanly and add new custom features
without making future syncs harder.

---

## 1. Principles

1. **Add, don't modify.** Prefer creating new files over editing upstream files.
   Files that don't exist in upstream can never conflict during a sync.
2. **Use optional interfaces, not interface extensions.** If you need a new
   capability on an upstream type, define an optional interface in a custom file
   and detect it via type assertion at the call site. See
   [proxy/vless/validator_meta.go](proxy/vless/validator_meta.go) for the
   canonical example.
3. **Mark every modification with `CUSTOM:`.** Every line (or block) of
   fork-specific logic inside an upstream file must be clearly labeled so it's
   visible during merge conflicts and when auditing drift.
4. **Keep semantic changes minimal inside upstream files.** Move helper
   functions into separate custom files and leave only small, marked call
   sites in the upstream files.

---

## 2. File inventory

### Custom-only files (no conflict surface with upstream)

| File | Feature |
|------|---------|
| `proxy/vless/validator_meta.go` | Optional `MetaValidator` interface for metadata-aware validation |
| `proxy/vless/inbound/validator_remote.go` | Remote tower HTTP validator with caching + dedup |
| `proxy/vless/inbound/validator_relay.go` | Passthrough validator for relay servers |
| `proxy/vless/inbound/error_response.go` | Server-side XERR error response sender |
| `proxy/vless/encoding/error_response.go` | Client-side XERR response parser |
| `app/dispatcher/custom_online.go` | Per-inbound online tracking helpers (`userOnlineIdentity`, `trackInboundOnline`) |
| `app/metrics/online_endpoints.go` | `/online` and `/online-users` HTTP endpoints |

### Modified upstream files (all custom changes marked with `CUSTOM:`)

| File | Change summary |
|------|----------------|
| `proxy/vless/validator.go` | None — byte-identical to upstream (kept pristine via `MetaValidator` optional interface) |
| `proxy/vless/encoding/encoding.go` | Decode-order reorder (addons before validator), XERR magic check in response, `MetaValidator` type assertion |
| `proxy/vless/encoding/addons.go` | Encoder triggers protobuf when `AuthVerified` or `ClientVersion` set (not just `Flow == XRV`) |
| `proxy/vless/encoding/addons.proto` | Added `AuthVerified` (field 3) + `ClientVersion` (field 4) |
| `proxy/vless/inbound/inbound.go` | Validator selection switch (init), VlessRoute bytes 8:10 override, RelayUUID/RelayClientVersion capture, `AuthVerified: true` in response, wildcard flow match, XERR hook on validation failure |
| `proxy/vless/inbound/config.proto` | Added `validator` (field 8) + `validator_endpoint` (field 9) |
| `proxy/vless/outbound/outbound.go` | `relay bool` field, relay-mode UUID/ClientVersion override |
| `proxy/vless/outbound/config.proto` | Added `relay` (field 2) |
| `common/session/session.go` | Added `RelayUUID []byte`, `RelayClientVersion string`; VlessRoute comment updated |
| `features/routing/context.go` | VlessRoute comment updated (bytes 8:10) |
| `infra/conf/vless.go` | Added `Validator`/`ValidatorEndpoint` (inbound), `Relay` (outbound) JSON config fields + propagation |
| `app/dispatcher/default.go` | Per-inbound online tracking call sites in `getLink`/`WrapLink` + outer guard relaxed for email-less users |
| `app/metrics/metrics.go` | Single call to `registerOnlineEndpoints(c)` |

---

## 3. The `CUSTOM:` marker convention

Every fork-specific modification in an upstream file uses one of:

- `// CUSTOM: <one-line reason>` — for single-line or small changes
- `// CUSTOM-BEGIN: <feature>` ... `// CUSTOM-END` — for multi-line blocks
- `// CUSTOM-BEGIN: <feature>` ... `// CUSTOM-END: <feature>` — when helpful

Find every customization with:

```bash
grep -rn "CUSTOM" --include="*.go" --include="*.proto"
```

---

## 4. Key design decisions (and why)

### MetaValidator as optional interface (not interface extension)

Upstream's `Validator` interface has `Get(id uuid.UUID) *protocol.MemoryUser`.
We need `GetWithMeta(id, clientVersion)` for the remote tower validator.
Instead of adding a method to `Validator` (which would force `MemoryValidator`
to also implement it and would conflict every time upstream changes `Validator`),
we defined `MetaValidator` in our own file. Validators that need metadata
implement it; upstream's `MemoryValidator` doesn't — the call site uses a type
assertion to detect and fall back to `Get(id)`.

Result: `proxy/vless/validator.go` is byte-identical to upstream.

### VlessRoute bytes 8:10 (not upstream's 6:8)

Upstream uses bytes 6-7 of the UUID for VlessRoute, and `ProcessUUID()` zeros
those same bytes to normalize the UUID for auth storage. In our relay
architecture, we need the **original untouched** bytes to flow from relay to
exit server, so we moved VlessRoute to bytes 8-9 — bytes untouched by
`ProcessUUID`. This is a deliberate override documented at
[common/session/session.go](common/session/session.go) and
[proxy/vless/inbound/inbound.go](proxy/vless/inbound/inbound.go).

### Online tracking uses UUID identity, not email

Remote-validator synthetic users have emails (`<uuid>@remote`). Relay-validator
synthetic users have no email. To count concurrent users on a relay server,
`trackInboundOnline` uses `userOnlineIdentity(user)` which returns the VLESS
UUID string (falls back to email for other protocols). The outer guard in
`app/dispatcher/default.go` was deliberately relaxed from
`if user != nil && len(user.Email) > 0` to `if user != nil` so email-less
users still reach the per-inbound tracking path.

### Protobuf encoder triggers on more than `Flow == XRV`

Upstream's `EncodeHeaderAddons` only emitted the protobuf payload when `Flow`
matched XRV. Without our fix, the server's `responseAddons{AuthVerified: true}`
would be dropped on the wire (zero-length written instead). We extended the
trigger to include `AuthVerified` and `ClientVersion != ""`. Old clients
decode the new payload gracefully — unknown proto3 fields are ignored.

---

## 5. Upstream sync procedure

### Prerequisites

- Protobuf tools (once):
  ```bash
  brew install protobuf          # provides protoc
  go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
  ```

### Routine sync

```bash
# 1. Save a backup branch before syncing (safety net)
git branch backup-pre-sync-$(date +%Y-%m-%d)
git push origin backup-pre-sync-$(date +%Y-%m-%d)

# 2. Fetch upstream
git fetch upstream

# 3. Merge or rebase
git checkout main
git merge upstream/main          # or: git rebase upstream/main

# 4. Resolve conflicts (only in the 10 modified upstream files)
#    The CUSTOM: markers make customizations easy to preserve.
#    Re-run the verification commands below after resolving.

# 5. If any .proto file was touched by upstream or you, regenerate:
protoc --go_out=. --go_opt=paths=source_relative \
  proxy/vless/encoding/addons.proto \
  proxy/vless/inbound/config.proto \
  proxy/vless/outbound/config.proto

# 6. Build
go build ./...

# 7. Run affected tests
go test ./proxy/vless/... \
        ./app/dispatcher/... \
        ./app/metrics/... \
        ./app/stats/... \
        ./common/session/... \
        ./features/routing/... \
        ./infra/conf/...
```

### Verification checklist after sync

Run these and confirm each returns non-zero:

```bash
# Each custom feature has evidence in the code
grep -c "case \"remote\":\|case \"relay\":" proxy/vless/inbound/inbound.go
grep -c "userSentID\[8:10\]" proxy/vless/inbound/inbound.go
grep -c "AuthVerified: true" proxy/vless/inbound/inbound.go
grep -c "account.Flow == \"\" || account.Flow == requestAddons.Flow" proxy/vless/inbound/inbound.go
grep -c "SendErrorResponse(connection" proxy/vless/inbound/inbound.go
grep -c "TryParseServerError" proxy/vless/encoding/encoding.go
grep -c "needsProtobuf := addons.Flow == vless.XRV" proxy/vless/encoding/addons.go
grep -c "validator.(vless.MetaValidator)" proxy/vless/encoding/encoding.go
grep -c "trackInboundOnline(ctx" app/dispatcher/default.go
grep -c "registerOnlineEndpoints(c)" app/metrics/metrics.go

# All 4 custom-only file groups present
test -f proxy/vless/validator_meta.go && echo ok
test -f proxy/vless/inbound/validator_remote.go && echo ok
test -f proxy/vless/inbound/validator_relay.go && echo ok
test -f proxy/vless/inbound/error_response.go && echo ok
test -f proxy/vless/encoding/error_response.go && echo ok
test -f app/dispatcher/custom_online.go && echo ok
test -f app/metrics/online_endpoints.go && echo ok

# Compile-time interface assertions still satisfied
grep -c "_ vless.MetaValidator = (\*remoteValidator)(nil)" proxy/vless/inbound/validator_remote.go
grep -c "_ vless.MetaValidator = (\*relayValidator)(nil)" proxy/vless/inbound/validator_relay.go
```

### What to do if a sync conflicts on an upstream file

1. Open the file — `CUSTOM:` markers show what's ours.
2. Resolve the conflict by keeping both upstream's new code **and** our
   custom markers/logic wherever they can coexist.
3. If upstream changed something our custom logic depends on (e.g. renamed
   a method or changed a signature), update our custom code to match.
4. Re-run the verification checklist.
5. If a proto file conflicts, regenerate the `.pb.go` file after resolving
   the `.proto` — never manually edit the generated file.

### What to do if upstream reorganizes a file heavily

If upstream heavily refactors `inbound.go` or `dispatcher/default.go` such that
the CUSTOM blocks can't be applied mechanically:

1. Check out the previous version: `git show HEAD~1:proxy/vless/inbound/inbound.go > /tmp/old.go`
2. Check out the backup branch to see what the original custom delta was:
   `git diff upstream/<old-tag>..backup-branch -- <file>`
3. Re-apply each CUSTOM block by hand to the new upstream structure. The
   `CUSTOM-BEGIN/END` markers make the boundaries explicit.
4. Run verification checklist.

---

## 6. Adding a new custom feature

Follow this checklist to keep the fork maintainable:

1. **Can it live entirely in a new file?** Prefer that.
2. **If it needs to hook into an upstream type, can an optional interface work?**
   (Like `MetaValidator`.) If yes, define the interface in a custom file and
   use type assertion at the call site.
3. **If you must edit an upstream file**, keep the diff minimal and wrap every
   custom line/block with `// CUSTOM:` / `// CUSTOM-BEGIN:` / `// CUSTOM-END`.
4. **If adding protobuf fields**, use high field numbers that upstream is
   unlikely to reuse (> 100 is safest). Document the assignment in the
   `.proto` file.
5. **Add compile-time assertions** for any interface contract your new code
   depends on:
   ```go
   var _ someUpstreamInterface = (*myType)(nil)
   ```
6. **Update this file** (FORK-MAINTENANCE.md) with the new feature in the
   tables above.

---

## 7. Backup & recovery

- Every sync should be preceded by a dated backup branch pushed to `origin`.
- The branch naming convention is `backup-pre-sync-YYYY-MM-DD`.
- If a sync goes wrong: `git reset --hard backup-pre-sync-YYYY-MM-DD`.

---

## 8. Research & planning docs

Design rationale and audit history for the current custom features lives in:

- [docs/fork-maintenance/](docs/fork-maintenance/) — index + subdirs
- [docs/fork-maintenance/research/](docs/fork-maintenance/research/) — per-feature design docs
- [docs/fork-maintenance/verification/](docs/fork-maintenance/verification/) — rebase audit reports
- [docs/fork-maintenance/rebase-2026-04-17/](docs/fork-maintenance/rebase-2026-04-17/) — snapshot of the delta at rebase time

Outside this repo, the parent NovusOrdo working directory also has
`docs/superpowers/plans/2026-04-17-xhttp-reality-reference.md` with the XHTTP
technical reference relevant to transport decisions.
