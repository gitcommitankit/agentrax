# Agentrax — Architecture Reference

> Permanent architecture reference. Update this file in the same commit as any architecture boundary or CRD change.

---

## Alternatives Considered (§4.3)

| Decision                        | Alternative                           | Why not chosen (v1)                                                                                                                                                                                                         |
| ------------------------------- | ------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Custom CRD + controller         | KEDA (`ScaledObject`) for autoscaling | KEDA is excellent for scaling in isolation, but Agentrax's scope is the full lifecycle (rollout + tenancy + discovery). KEDA is a credible v2 backend alternative.                                                          |
| Custom rollout controller       | Argo Rollouts / Flagger               | Both are mature progressive-delivery tools and the closest prior art. Not adopted because the statistical-significance handling for low-traffic agent canaries needed to be owned explicitly, not inherited as a black box. |
| Custom model-serving management | KServe / Seldon Core                  | Both solve model serving but don't address multi-tenant quota admission or MCP-native discovery — Agentrax's actual differentiators.                                                                                        |
| Traffic splitting               | Istio VirtualService                  | Istio's weighting works but requires full mesh sidecar deployment; Gateway API achieves the same with a lighter footprint.                                                                                                  |
| Language                        | Python (Kopf)                         | Smaller testing ecosystem (no `envtest` equivalent) and weaker typing for CRD schemas at this complexity.                                                                                                                   |

---

## E2E Validation Scenarios (§9.4)

The canonical list of top-level end-to-end test scenarios. Do not add new top-level e2e scenarios without first checking this list.

| Feature             | Key scenario validated end-to-end                                                                     |
| ------------------- | ----------------------------------------------------------------------------------------------------- |
| Core reconciliation | Manual deletion of child `Deployment` is self-healed within one reconcile interval                    |
| Autoscaling         | Synthetic queue-depth load produces bounded, non-flapping scale-out and scale-in                      |
| Canary rollout      | Injected latency regression triggers rollback before reaching 100% weight; stable traffic never drops |
| Multi-tenancy       | Concurrent near-limit creates never both succeed when only one fits in quota                          |
| MCP registry        | Registry entry expires after ungraceful pod termination without waiting on an explicit delete event   |

---

## Development Roadmap (§8)

| Phase | Focus               |   Status    | Milestone                                                                                |
| ----- | ------------------- | :---------: | ---------------------------------------------------------------------------------------- |
| 0     | Scaffolding         |  **Done**   | `make install` applies CRDs; `make run` starts both controllers                          |
| 1     | Core reconciliation |  **Done**   | `AgentDeployment` → Deployment/Service/ServiceMonitor; status/conditions; finalizer stub |
| 2     | Multi-tenancy       |  **Done**   | `TenantQuota`, validating + mutating webhook, quota enforcement                          |
| 3     | Autoscaling         |  **Done**   | Prometheus Adapter integration, managed HPA                                              |
| 4     | Canary rollout      |  **Done**   | Rollout state machine, Gateway API traffic shifting, PromQL threshold evaluation         |
| 5     | MCP registry        |  **Done**   | Registrar, registry HTTP handler, discovery API, TTL/heartbeat, ConfigMap persistence    |
| 6     | Hardening & demo    |   Pending   | E2e tests in CI, Helm chart, README, recorded demo                                       |

Phases 2, 3, and 5 are independent of each other and can run in parallel once Phase 1 is complete. Phase 4 depends on both 2 and 3.

### Phase 4 — Canary Rollout (§6)

The canary controller (`internal/rollout.Controller`) is a pure helper driven by the `AgentDeploymentReconciler` each reconcile cycle. When `spec.rollout.strategy: Canary` and `spec.image` differs from `status.stableVersion`, the reconciler transitions to `RolloutInProgress` and calls `Step()` on every subsequent reconcile.

**State machine**: `setWeight` steps create the canary Deployment and upsert a Gateway API `HTTPRoute` with the target weight split. `pause` steps query Prometheus for request count (sample gate), error rate, and p99 latency via `internal/metrics.Client`. If Prometheus is unreachable for longer than a fixed 60-second timeout, a fail-safe rollback fires. Sample-size gating extends the pause window up to the lesser of `3×pause_duration` or an absolute 15-minute maximum before forcing evaluation. Promotion updates the stable Deployment image and restores the HPA; rollback reverts everything and sets `phase=RolloutFailed`.

**New operator flags**: `--prometheus-url` (required for Canary), `--gateway-name`, `--gateway-namespace`.

**New status fields**: `canaryStepIndex`, `pauseStartedAt`, `promUnreachableSince` — all persisted so the state machine is re-entrant across operator restarts.

### Phase 5 — MCP Registration & Discovery (§7)

The MCP registry is an embedded HTTP service inside the operator manager process, served on `--registry-bind-address` (default `:9090`) and exposed to the cluster as the `agentrax-registry` ClusterIP Service in `agentrax-system`.

**Registration flow**: When `spec.mcp.expose: true` and the agent's underlying Deployment reports `rolloutComplete` (`phase=Running`), the reconciler calls `registry.Registrar.Register()`, which performs an MCP-level `initialize` JSON-RPC 2.0 handshake by posting to the `/initialize` sub-path at `http://{name}.{namespace}.svc:{port}/initialize`. Discovered tools are merged with `spec.mcp.tools` and persisted to the in-memory registry map and `agentrax-registry` ConfigMap. On failure, `MCPHandshakeFailed` condition is set and `status.registered` remains `false`.

**TTL & Heartbeat**: Every registry entry carries a `TTL` (default 90s) and `HeartbeatAt` timestamp. When an agent is `status.registered: true` and healthy in `Running` phase, the reconciler triggers `Registrar.Heartbeat()`. A background sweeper running every 30s removes expired entries if heartbeats lapse (e.g. following an ungraceful pod termination). After 3 consecutive heartbeat probe failures, the agent is automatically deregistered.

**Deregistration & Finalizers**: Clean deregistration occurs when: (1) an `AgentDeployment` is deleted (via the `agentrax.io/mcp-deregister` finalizer before child services are garbage-collected), (2) `spec.mcp.expose` is toggled to `false`, or (3) 3 consecutive heartbeat probes fail.

**State Recovery**: On operator startup/restart, existing registrations are reloaded from the `agentrax-registry` ConfigMap.

**REST API Endpoints**:
- `GET /agents`: List all active, non-expired registered agents.
- `POST /agents`: Register / update an agent. This endpoint requires a successful MCP initialize handshake and currently requires no authentication. It bypasses the handshake verification when called directly.
- `GET /agents/{namespace}/{name}`: Get metadata and tools for a specific agent.
- `DELETE /agents/{namespace}/{name}`: Deregister an agent. This endpoint currently requires no authentication.
- *(Legacy aliases `POST /register` and `DELETE /deregister` are supported for backward compatibility)*.

**New operator flags**: `--registry-bind-address` (default `:9090`).


