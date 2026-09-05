# Agentrax — Zero-Trust Network Policy Architecture & Guide

Agentrax implements a **two-tier zero-trust network security model** enforced at the Kubernetes CNI layer. This prevents lateral movement between tenants, blocks unauthorized outbound internet access, and restricts operational endpoints to authorized components only.

---

## 1. Overview of the Two-Tier Architecture

```
                                    ┌─────────────────────────────┐
                                    │      Prometheus Pod         │
                                    │ (ns: `monitoring: enabled`) │
                                    └──────────────┬──────────────┘
                                                   │
                   TCP :8443 (HTTPS)               │ TCP :8080 (Scrape)
         ┌─────────────────────────────────────────┴────────────────────────────────────────┐
         │                                                                                  │
         ▼                                                                                  ▼
┌──────────────────────────────────────┐                           ┌──────────────────────────────────────┐
│  Tier 1: Operator Control Plane      │                           │  Tier 2: Tenant Agent Pods           │
│  Namespace: `agentrax-system`        │                           │  Namespace: `tenant-*`               │
│  Selector:                           │                           │  Selector:                           │
│    `control-plane: controller-manager│                           │    `agentrax.io/agent: "true"`       │
│                                      │                           │                                      │
│  • Ingress: Only TCP :8443 from      │                           │  • Ingress: Only TCP :8080 from      │
│    `metrics: enabled` namespaces     │                           │    `monitoring: enabled`             │
│  • Default: Drops rogue scrapes      │                           │  • Egress Allowed:                   │
│                                      │                           │    - Port 443 & 6443: kube-apiserver │
│                                      │                           │    - Port 53: kube-system CoreDNS    │
│                                      │                           │  • Egress Denied (Default):          │
│                                      │                           │    - Cross-tenant network packets    │
│                                      │                           │    - External internet egress        │
└──────────────────────────────────────┘                           └──────────────────────────────────────┘
```

---

## 2. Policy Definitions

### Tier 1: Operator Metrics Protection (`allow-metrics-traffic.yaml`)

- **Namespace**: `agentrax-system`
- **Target Pods**: `control-plane: controller-manager`
- **Policy Type**: `Ingress`
- **Rules**:
  - Allows TCP traffic on port `8443` (HTTPS metrics) **only** from namespaces labeled `metrics: enabled`.
  - Protects internal operator diagnostics and telemetry from arbitrary cluster workloads.

### Tier 2: Tenant Agent Workload Isolation (`tenant-agent-isolation.yaml`)

- **Namespace**: `tenant-*` (applied in each tenant namespace)
- **Target Pods**: `agentrax.io/agent: "true"`
  > **Note**: The `AgentDeployment` reconciler automatically stamps `agentrax.io/agent: "true"` onto every managed Pod template, ensuring that every AI agent pod is automatically governed by this policy.
- **Policy Type**: `Ingress` and `Egress` (activates **Default-Deny** in both directions).
- **Ingress Rules**:
  - Permits inbound TCP port `8080` (Prometheus metrics scrape) strictly from namespaces labeled `monitoring: enabled`.
- **Egress Rules**:
  - **Kubernetes API Server**: TCP ports `443` and `6443` (allows agents to discover services or use authorized Kubernetes cluster tools).
  - **CoreDNS**: UDP and TCP port `53` to pods matching `k8s-app in (kube-dns, coredns)` in namespace `kube-system`.
  - **Default-Deny**: All other outbound communication (such as cross-tenant probing or external internet data exfiltration) is dropped.

---

## 3. Applying the Policies

### Option A: Via Consolidated Installer (Automated)

The consolidated release installer (`dist/install.yaml`) automatically includes both NetworkPolicies under `agentrax-system`.

```bash
kubectl apply -f https://github.com/gitcommitankit/agentrax/releases/download/v0.2.0/install.yaml
```

### Option B: Applying Tenant Isolation to Individual Tenant Namespaces

To enforce tenant isolation in a new tenant namespace:

```bash
kubectl apply -n tenant-alpha -f config/network-policy/tenant-agent-isolation.yaml
```

---

## 4. How to Verify Isolation in a Live Cluster

You can verify that the network security perimeter is working using a standard ephemeral pod:

### 1. Verify CoreDNS & API Server Egress is Permitted:

```bash
kubectl run test-dns --rm -it --restart=Never -n tenant-alpha \
  --labels="agentrax.io/agent=true" \
  --image=busybox -- nslookup kubernetes.default
```

- **Expected Result**: Successfully resolves the IP address of `kubernetes.default.svc.cluster.local`.

### 2. Verify External Internet Access is Blocked:

```bash
kubectl run test-egress --rm -it --restart=Never -n tenant-alpha \
  --labels="agentrax.io/agent=true" \
  --image=busybox -- wget -T 3 -qO- http://1.1.1.1:80
```

- **Expected Result**: Connection times out (`wget: download timed out`). Unapproved egress is blocked cold.
