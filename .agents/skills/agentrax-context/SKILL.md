---
name: agentrax-context
description: Project context and settled architecture decisions for the Agentrax Kubernetes operator (module agentrax.io/v1alpha1, repo agentrax). Always consult this before writing, reviewing, or reasoning about any code in this repository — CRD types, the reconciler, the rollout controller, the autoscaler, the quota webhook, or the MCP registry — so implementation stays consistent with the design doc instead of drifting or re-deriving decisions that are already settled. Trigger on any mention of AgentDeployment, TenantQuota, canary rollout, or this repo's controllers, even if the user doesn't name the skill directly.
---

# Agentrax Context Skill

> When uncertain about any architecture decision, defer to `docs/ARCHITECTURE.md` rather than improvising. Don't guess when the doc has the answer.

## Non-negotiable terminology

- API group/version: `agentrax.io/v1alpha1` — the group is `agentrax.io`, never `agentrax.agentrax.io`
- CRD Kinds: `AgentDeployment` (one per deployed model/agent), `TenantQuota` (one per tenant)
- Namespace convention: `tenant-<name>`
- `status.phase` values: `Pending`, `Running`, `RolloutInProgress`, `RolloutFailed`, `Degraded`
- Rollout terminology is always "stable" / "canary" — never "blue/green" or "primary/secondary"; matches CRD status fields (`stableVersion`, `canaryVersion`, `canaryWeight`) exactly
- Finalizer name: `agentrax.io/mcp-deregister` (constant `AgentDeploymentFinalizer` in `api/v1alpha1`)

## Architecture boundaries — already decided, don't relitigate

- **Autoscaling**: native `HorizontalPodAutoscaler` pointed at Prometheus Adapter custom metrics (`queueDepth` or `gpuUtilization`). No custom scaling loop. During active canary, the stable HPA is paused (deleted) and no canary HPA is created — autoscaling resumes only after promotion or rollback.
- **Traffic splitting**: Gateway API `HTTPRoute` weighted backends. Not Istio, not ingress annotations.
- **MCP registry**: embedded HTTP handler inside the operator process, backed by a `ConfigMap`. Not a separate Deployment, not a new database — HA storage is a v2 item.
- **Non-goals**: no model training/fine-tuning, no general-purpose workload management, no service mesh, no UI in v1. Flag any drift toward these rather than quietly implementing them.

## Package map

| Package                | Responsibility                                                                                                                   |
| ---------------------- | -------------------------------------------------------------------------------------------------------------------------------- |
| `api/v1alpha1/`        | CRD Go types, validation markers, defaulting. No business logic.                                                                 |
| `internal/controller/` | Reconcile loops. Only code that calls the Kubernetes API for core-owned resources.                                               |
| `internal/rollout/`    | Canary state machine and PromQL threshold evaluation.                                                                            |
| `internal/scaling/`    | HPA generation and quota-capped scaling logic.                                                                                   |
| `internal/registry/`   | MCP registrar, registry HTTP handler, TTL sweep.                                                                                 |
| `internal/quota/`      | Quota arithmetic and in-flight reservation. Shared by webhook and TenantQuota reconciler.                                        |
| `internal/webhook/`    | Validating and mutating admission webhooks. Lives here (not `api/`) to import `internal/quota` without creating an import cycle. |
| `internal/metrics/`    | Shared Prometheus client plumbing used by rollout and scaling.                                                                   |

## Where the hard logic lives

- **`internal/rollout/`** — never evaluate `rollback` thresholds against a sample smaller than `minRequestSample`. A 10%-weight canary at low traffic produces statistically meaningless error rates; gate on sample size first. Canary steps must include at least one terminal `setWeight: 100` step for full promotion. Range query windows must format to canonical Prometheus syntax (`5m`, `1h`, `30s`, no trailing `0s`).
- **`internal/quota/`** — two concurrent near-limit creates can individually pass a read-then-write quota check but combined exceed it. Use an in-flight reservation (short-lived in-memory map, keyed by tenant), not a naive status read.
- **`internal/registry/`** — registration requires a successful MCP-level `initialize` handshake, not just Kubernetes readiness. Entries carry a TTL/heartbeat; ungraceful termination (OOM-kill, node failure) skips the deletion event path entirely, so don't rely on it.
- **`internal/metrics/`** — all Prometheus HTTP responses must be read with `io.LimitReader` (1 MiB ceiling) to protect against memory exhaustion.
- **`internal/controller/`** — reconcilers consume MCP registry operations via the `AgentRegistrar` interface (`Register`, `Deregister`, `Heartbeat`) for test isolation without polluting production structs.
- Finalizer ordering: deregister from MCP _before_ the `Service` is garbage collected. Controller-runtime's foreground deletion via finalizer is the enforcement mechanism, not best-effort.
- Quota reduction: lowering `TenantQuota` below current usage sets an `OverQuota` condition and blocks new creates/scale-ups. Never forcibly delete existing resources.

## Testing conventions

- **Unit**: table-driven, no cluster — covers `internal/quota` arithmetic, `internal/rollout` sample-gating, PromQL template output.
- **Integration**: `envtest` (real API server + etcd, no kubelet) — reconciler child resource creation, webhook quota rejection, finalizer ordering.
- **E2E**: real `kind` cluster in CI. Full user-journey scenarios only; don't add top-level scenarios beyond the existing five feature areas.

## Key edge cases — never silently drop

| Area      | Edge case                                       | Correct behaviour                                                                |
| --------- | ----------------------------------------------- | -------------------------------------------------------------------------------- |
| Canary    | `minRequestSample` not met during pause         | Extend pause (capped at 15 min); never evaluate on insufficient sample           |
| Canary    | Prometheus unreachable during pause             | Fail-safe rollback after 60 s; never hang                                        |
| Canary    | Second rollout triggered mid-rollout            | Webhook rejects; never run two concurrent canaries on the same `AgentDeployment` |
| Canary    | HPA during active canary                        | Pause stable HPA; no canary HPA; resume only after promote/rollback              |
| Canary    | Out-of-band deletion of HTTPRoute/Deployment    | Self-healed on next reconcile cycle in `Step()` preserving active traffic split  |
| Canary    | Rollout failed or aborted                       | Operator sets `RolloutFailed`, preserves `status.canaryVersion`, no retry loop   |
| Quota     | Two concurrent near-limit creates               | In-flight reservation; one wins, one is rejected                                 |
| Quota     | Quota lowered below current usage               | Set `OverQuota` condition; never forcibly delete existing resources              |
| Quota     | `TenantQuota` deleted while agents exist        | Surface `TenantQuotaNotFound` on `QuotaLimited` condition; never crash           |
| MCP       | Ungraceful pod termination                      | TTL/heartbeat expires the entry within one TTL window (default 90 s)             |
| MCP       | Pod `Ready` but MCP handshake fails             | Do not register; surface `MCPHandshakeFailed` condition                          |
| Deletion  | `AgentDeployment` deleted                       | Finalizer ensures MCP deregistration before `Service` is GC'd                    |
| Namespace | Namespace deleted with active `AgentDeployment` | Finalizer blocks termination long enough to deregister cleanly                   |
