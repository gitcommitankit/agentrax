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
| 1     | Core reconciliation | **Up Next** | `AgentDeployment` → Deployment/Service/ServiceMonitor; status/conditions; finalizer stub |
| 2     | Multi-tenancy       |   Pending   | `TenantQuota`, validating + mutating webhook, quota enforcement                          |
| 3     | Autoscaling         |   Pending   | Prometheus Adapter integration, managed HPA                                              |
| 4     | Canary rollout      |   Pending   | Rollout state machine, Gateway API traffic shifting, PromQL threshold evaluation         |
| 5     | MCP registry        |   Pending   | Registrar, registry HTTP handler, discovery API                                          |
| 6     | Hardening & demo    |   Pending   | E2e tests in CI, Helm chart, README, recorded demo                                       |

Phases 2, 3, and 5 are independent of each other and can run in parallel once Phase 1 is complete. Phase 4 depends on both 2 and 3.
