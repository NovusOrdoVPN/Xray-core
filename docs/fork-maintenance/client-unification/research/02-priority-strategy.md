# Group 2 — PriorityStrategy Router (Client Fork)

## Executive Summary

The client fork adds a **new router balancer strategy named `"priority"`** that selects the first `Alive` outbound from a pre-sorted candidate list using Observatory probe results. It is the Go-side heart of the VPNTool failover mechanism (Reality primary → Cloudflare/Relay fallback). Upstream xray-core **v26.4.15 does not have this strategy**; only `random`, `roundrobin`, `leastping`, and `leastload` exist. The strategy also exposes a process-global `RouteChangeCallback` hook the gomobile layer uses to notify Swift when the active outbound changes. **Recommendation: PORT_AS_IS** into the unified fork.

## Feature Description

Given a candidate set of outbound tags (e.g. `direct-00`, `direct-01`, `relay-00`), PriorityStrategy picks in three phases:

1. **Phase 1 — First ALIVE:** iterates candidates in their incoming (alphabetical) order and returns the first tag whose Observatory status is `Alive=true`.
2. **Phase 2 — First UNKNOWN:** if no alive tag is found, returns the first tag that has never been probed yet (cold-start optimism, prevents a dead-looking state before any probe completes).
3. **Phase 3 — All dead:** returns `""`, which the `Balancer` wrapper interprets as "no candidate", triggering `fallbackTag` (passed through from `BalancingRule.FallbackTag`).

Because selection is by sort order, the naming convention (`direct-00` < `direct-01` < `relay-00`) encodes priority: direct Reality is always preferred over relay fallback when both are healthy. Memory of the previous pick is kept in `s.lastPicked`; when the pick changes, the optional `routeChangeCallback` fires with the new tag.

## Annotated Code

File: `app/router/strategy_priority.go` (131 lines, NEW).

```go
type PriorityStrategy struct {
    FallbackTag string
    ctx         context.Context
    observatory extension.Observatory   // acquired via core.RequireFeatures
    mu          sync.Mutex
    lastPicked  string                  // for change-detection notifications
}
```

Key methods:
- `InjectContext(ctx)` — called by the Balancer; uses `core.RequireFeatures` to inject the Observatory feature. If no Observatory is configured, `s.observatory` stays nil and `PickOutbound` just returns `candidates[0]` (sensible fallback, documented in a comment inside the file).
- `PickOutbound(candidates)` — the three-phase logic described above.
- `GetPrincipleTarget(candidates)` — wraps `PickOutbound` for the routing-rule principle-target interface; if the pick is empty, returns the full candidate list so the balancer can fall back.
- `notifyIfChanged(tag)` — diff against `lastPicked`; if different, call the registered `routeChangeCallback` under a second mutex. Callback set via package-global `SetRouteChangeCallback` / `ClearRouteChangeCallback`.

## Config Format (JSON)

Consumed in a standard Xray `routing.balancers[]` entry:

```json
{
  "routing": {
    "balancers": [{
      "tag": "main",
      "selector": ["direct-", "relay-"],
      "fallbackTag": "blocked",
      "strategy": { "type": "priority" }
    }]
  }
}
```

`"priority"` uses `strategyEmptyConfig` — no extra settings keys. It relies on an Observatory (or BurstObservatory) app being configured elsewhere in the config to produce probe statuses; without it, the strategy degrades to "pick first candidate".

## Integration Points

1. `infra/conf/router_strategy.go` — adds `strategyPriority = "priority"` constant and registers it in `strategyConfigLoader` with `strategyEmptyConfig`.
2. `infra/conf/router.go` — adds `strategyPriority` to the whitelist in the `switch r.Strategy.Type` validator (reject-unknown-type guard).
3. `app/router/config.go` — `BalancingRule.Build` gets a new `case "priority":` branch that instantiates `&Balancer{ strategy: &PriorityStrategy{FallbackTag: br.FallbackTag}, … }`.
4. `app/router/strategy_priority.go` — the strategy itself plus the exported callback API (`SetRouteChangeCallback`, `ClearRouteChangeCallback`) that the gomobile controller uses to push route-change events up to Swift / Flutter.

Dependencies: `app/observatory` (for `ObservationResult`/`OutboundStatus` shape) and `features/extension.Observatory` (for probe data). These already exist in upstream v26.4.15 unchanged.

## Upstream Comparison (v26.4.15)

- `/Users/arturdev/Developer/NovusOrdo/xray/Xray-core/app/router/` contains only `strategy_random.go`, `strategy_roundrobin` (in config.go), `strategy_leastping.go`, `strategy_leastload.go`. **No `strategy_priority.go`.**
- `infra/conf/router_strategy.go` upstream defines only the four original strategies; `priority` is absent.
- `BalancingRule.Build` upstream has no `case "priority":` branch.
- Grep for `PriorityStrategy`, `strategyPriority`, `"priority"` in upstream returned zero matches.

No naming collision, no overlapping feature. `leastping` is the closest cousin (also Observatory-backed) but it picks by minimum RTT and explicitly returns `""` if the observatory is nil — different semantics, not a substitute for the deterministic priority-order failover this client depends on. The memory log (`project_failover_go_migration.md`) notes this was the April 10 migration from Swift FailoverManager into Go, so it is actively in production on the client.

## Recommendation: PORT_AS_IS

Port directly into the unified fork with zero behavioral changes:

1. Add `app/router/strategy_priority.go` verbatim from `/Users/arturdev/Developer/NovusOrdo/xray/xray_mobile_library/xray-core/app/router/strategy_priority.go`.
2. Extend `infra/conf/router_strategy.go` with the `strategyPriority` constant and loader entry.
3. Extend `infra/conf/router.go` validator switch with `strategyPriority`.
4. Extend `app/router/config.go` `BalancingRule.Build` with the `case "priority":` branch.

Additional notes:
- Server fork does not use this strategy, so adding it to the unified fork has zero server-side impact (it is just dead code on the server, reachable only if a server-side routing config references `"priority"`).
- The gomobile controller's call to `router.SetRouteChangeCallback` must be preserved on the client-consumer side (lives in `xraymobile/controller.go`, outside this patch's scope — tracked by Group covering gomobile bridge).
- No protobuf changes needed: `strategyEmptyConfig.Build()` returns `nil`, and no proto message is serialized for priority.
- Verify after port that the `observatory` import path still resolves under v26.4.15 (`github.com/xtls/xray-core/app/observatory` — unchanged upstream).

## Relevant File Paths (Absolute)

- Patch: `/tmp/xray-client-analysis/patches/02-priority-strategy.patch`
- Client source: `/Users/arturdev/Developer/NovusOrdo/xray/xray_mobile_library/xray-core/app/router/strategy_priority.go`
- Client modified: `/Users/arturdev/Developer/NovusOrdo/xray/xray_mobile_library/xray-core/app/router/config.go`
- Client modified: `/Users/arturdev/Developer/NovusOrdo/xray/xray_mobile_library/xray-core/infra/conf/router.go`
- Client modified: `/Users/arturdev/Developer/NovusOrdo/xray/xray_mobile_library/xray-core/infra/conf/router_strategy.go`
- Upstream target: `/Users/arturdev/Developer/NovusOrdo/xray/Xray-core/app/router/` (branch `sync-upstream-v26.4.15`)
