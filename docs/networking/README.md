# Agentrax — Tenant Network Isolation

This document explains the two-tier network policy model shipped with Agentrax
and how platform operators apply it to tenant namespaces.

## Overview

Agentrax uses Kubernetes `NetworkPolicy` to enforce a **zero-trust perimeter**
around all agent pods. This prevents a compromised or misbehaving agent in one
tenant from reaching another tenant's services, the operator control plane, or
arbitrary internet destinations.

Two policies are maintained:

| Policy File                   | Namespace                  | Purpose                                                                    |
| ----------------------------- | -------------------------- | -------------------------------------------------------------------------- |
| `allow-metrics-traffic.yaml`  | `agentrax-system`          | Allows Prometheus to scrape the operator `/metrics` endpoint               |
| `tenant-agent-isolation.yaml` | Every `tenant-*` namespace | Isolates agent pods — restricts all ingress/egress to the minimum required |

## How the Label Selector Works

The `tenant-agent-isolation` policy uses `podSelector.matchLabels`:

```yaml
podSelector:
  matchLabels:
    agentrax.io/agent: "true"
```

The `AgentDeployment` reconciler (`internal/controller/agentdeployment_controller.go`)
stamps this label onto every agent `Deployment`'s pod template via `agentLabels()`.
No manual labelling is needed — all agent pods are automatically covered.

## Traffic Model

```
┌─────────────────────────────────────────────────────┐
│                 tenant-finance namespace            │
│                                                     │
│   [Agent Pod] agentrax.io/agent=true                │
│       │                                             │
│       ├─ Ingress ← port 8080 ← [Prometheus]         │
│       │           (monitoring namespace only)       │
│       │                                             │
│       ├─ Egress → port 6443 → [kube-apiserver]      │
│       ├─ Egress → port 53   → [CoreDNS]             │
│       │                                             │
│       └─ ALL OTHER TRAFFIC: BLOCKED                 │
└─────────────────────────────────────────────────────┘
```

## Applying the Policy to Tenant Namespaces

The `tenant-agent-isolation.yaml` NetworkPolicy must be applied to each tenant
namespace. The policy is **not** automatically applied by the operator — it is
applied once by a platform admin when provisioning a tenant namespace.

### Apply Manually

```bash
# Apply to a specific tenant namespace:
kubectl apply -n tenant-finance \
  -f config/network-policy/tenant-agent-isolation.yaml

kubectl apply -n tenant-marketing \
  -f config/network-policy/tenant-agent-isolation.yaml
```

### Apply via Kustomize (Development)

The default Kustomize overlay applies both network policies to the `agentrax-system`
namespace for development/testing. The `tenant-agent-isolation` policy in this
context validates the manifest schema; in production it must be applied per tenant
namespace as above.

```bash
kubectl apply -k config/default/
```

### Apply via Helm (Recommended for Production)

When installing via Helm, set `networkPolicy.enabled: true` (Phase 1 Helm
integration — coming in a future release):

```bash
helm upgrade --install agentrax charts/agentrax/ \
  --set networkPolicy.enabled=true
```

## Labelling the Prometheus Namespace

The ingress rule allows traffic from namespaces labelled `monitoring: enabled`.
Apply this label to the namespace where Prometheus Operator / kube-prometheus-stack
is installed:

```bash
kubectl label namespace monitoring monitoring=enabled
# Or, if using the default kube-prometheus-stack namespace name:
kubectl label namespace monitoring monitoring=enabled
```

## Required CNI Support

This NetworkPolicy relies on a Container Network Interface (CNI) plugin that
**enforces** `NetworkPolicy` objects. Verify your CNI supports this:

| Environment      | Supported CNI                 |
| ---------------- | ----------------------------- |
| Kind (local dev) | Kindnet (default) ✅          |
| Azure AKS        | Azure CNI or Calico ✅        |
| AWS EKS          | VPC CNI + Calico or Cilium ✅ |
| GKE              | Dataplane V2 (Cilium) ✅      |

> **Note**: Flannel does **not** enforce NetworkPolicy by default. Use Calico or
> Cilium as a replacement CNI if Flannel is your cluster default.

## Verifying the Policy

After applying, verify that the policy is active and that an agent pod has
the correct label:

```bash
# Confirm agent pod has the isolation label:
kubectl get pods -n tenant-finance -L agentrax.io/agent

# Confirm the NetworkPolicy is present:
kubectl get networkpolicy -n tenant-finance

# Test that cross-tenant traffic is blocked (from within an agent pod):
kubectl exec -n tenant-finance <agent-pod> -- \
  curl --connect-timeout 2 http://<service-in-tenant-marketing>
# Expected: connection timed out (blocked)

# Test that Kubernetes API access is allowed:
kubectl exec -n tenant-finance <agent-pod> -- \
  curl -k https://kubernetes.default.svc:443/healthz
# Expected: "ok"
```
