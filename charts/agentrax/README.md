# Agentrax Helm Chart

Official Helm chart for installing and managing **Agentrax** — a declarative, multi-tenant Kubernetes operator for autonomous AI agents and LLM inference workloads.

---

## Features Supported by this Chart

- **Multi-Tenant Quota Enforcement**: Dynamic admission and scaling boundaries preventing GPU, agent instance, and replica over-commitment.
- **Canary Progressive Traffic Splitting**: Automated Gateway API canary routing with Prometheus error rate and latency threshold evaluation.
- **In-Cluster MCP Discovery Registry**: Embedded Model Context Protocol (MCP) tool discovery server on port `:9090`.
- **Zero-Trust Multi-Cloud Identity**: Native secretless workload identity for **Microsoft Azure (AKS Workload Identity)** and **Amazon Web Services (AWS EKS IRSA)**.
- **Hardened Pod Security**: Runs non-root (`UID 65532`), read-only root filesystem, dropped capabilities (`ALL`), and `RuntimeDefault` seccomp profile.

---

## Prerequisites

- **Kubernetes**: `v1.28+`
- **Helm**: `v3.14+`
- **cert-manager**: `v1.13+` (required for admission webhook TLS certificates)
- **Gateway API CRDs**: `v1.0+` (required if using canary traffic splitting)

---

## Adding the Helm Repository

```bash
helm repo add agentrax https://gitcommitankit.github.io/agentrax
helm repo update
```

---

## Installing the Chart

### 1. Default Installation

Install Agentrax into the `agentrax-system` namespace:

```bash
helm install agentrax agentrax/agentrax \
  --namespace agentrax-system \
  --create-namespace
```

### 2. Installation with Prometheus Canary Evaluation

Wire Agentrax to an in-cluster Prometheus instance to enable canary threshold evaluations:

```bash
helm install agentrax agentrax/agentrax \
  --namespace agentrax-system \
  --create-namespace \
  --set prometheus.url="http://kube-prometheus-stack-prometheus.monitoring.svc:9090"
```

### 3. Installation for Azure AKS (Workload Identity)

```bash
helm install agentrax agentrax/agentrax \
  --namespace agentrax-system \
  --create-namespace \
  --set workloadIdentity.provider=azure \
  --set workloadIdentity.enabled=true \
  --set workloadIdentity.azureClientId="<MANAGED_IDENTITY_CLIENT_ID>" \
  --set workloadIdentity.azureTenantId="<AZURE_TENANT_ID>"
```

### 4. Installation for AWS EKS (IRSA)

```bash
helm install agentrax agentrax/agentrax \
  --namespace agentrax-system \
  --create-namespace \
  --set workloadIdentity.provider=aws \
  --set workloadIdentity.enabled=true \
  --set workloadIdentity.awsRoleArn="arn:aws:iam::<ACCOUNT_ID>:role/AgentraxOperatorRole"
```

---

## Upgrading the Chart

```bash
helm repo update
helm upgrade agentrax agentrax/agentrax \
  --namespace agentrax-system \
  --reuse-values
```

---

## Uninstalling the Chart

```bash
helm uninstall agentrax --namespace agentrax-system
```

> **Note**: Custom Resource Definitions (CRDs) are retained by default to prevent accidental data loss. To remove them manually:
>
> ```bash
> kubectl delete crd agentdeployments.agentrax.io tenantquotas.agentrax.io
> ```

---

## Configuration Reference (`values.yaml`)

| Parameter                                  | Description                                                         | Default                           |
| :----------------------------------------- | :------------------------------------------------------------------ | :-------------------------------- |
| `replicaCount`                             | Number of controller-manager pod replicas                           | `1`                               |
| `image.repository`                         | Image repository for the controller-manager                         | `ghcr.io/gitcommitankit/agentrax` |
| `image.tag`                                | Image tag (defaults to `Chart.appVersion`)                          | `""`                              |
| `image.pullPolicy`                         | Image pull policy                                                   | `IfNotPresent`                    |
| `serviceAccount.create`                    | Create a dedicated ServiceAccount                                   | `true`                            |
| `serviceAccount.name`                      | Custom ServiceAccount name (auto-generated if empty)                | `""`                              |
| `serviceAccount.annotations`               | Custom annotations for ServiceAccount                               | `{}`                              |
| `podSecurityContext.runAsNonRoot`          | Run controller container as non-root user                           | `true`                            |
| `podSecurityContext.seccompProfile.type`   | Container runtime default seccomp profile                           | `RuntimeDefault`                  |
| `securityContext.allowPrivilegeEscalation` | Disallow privilege escalation                                       | `false`                           |
| `securityContext.readOnlyRootFilesystem`   | Mount root filesystem as read-only                                  | `true`                            |
| `securityContext.capabilities.drop`        | Linux capabilities to drop                                          | `["ALL"]`                         |
| `resources.limits.cpu`                     | CPU limit for controller manager                                    | `500m`                            |
| `resources.limits.memory`                  | Memory limit for controller manager                                 | `128Mi`                           |
| `resources.requests.cpu`                   | CPU request for controller manager                                  | `10m`                             |
| `resources.requests.memory`                | Memory request for controller manager                               | `64Mi`                            |
| `manager.leaderElect`                      | Enable leader election (required if `replicaCount > 1`)             | `false`                           |
| `manager.metricsBindAddress`               | Metrics endpoint bind address (`:8443`, `:8080`, or `0` to disable) | `"0"`                             |
| `manager.metricsSecure`                    | Serve metrics securely via HTTPS                                    | `true`                            |
| `manager.healthProbeBindPort`              | Port for `/healthz` and `/readyz` probes                            | `8081`                            |
| `manager.gpuResourceName`                  | Resource name used for GPU quota accounting                         | `"nvidia.com/gpu"`                |
| `prometheus.url`                           | Base URL of Prometheus for canary metric evaluation                 | `""`                              |
| `gateway.name`                             | Name of Gateway API Gateway for canary routing                      | `"agentrax-gateway"`              |
| `gateway.namespace`                        | Namespace of Gateway API Gateway                                    | `"agentrax-system"`               |
| `registry.service.port`                    | In-cluster port for the MCP discovery service                       | `9090`                            |
| `registry.bindPort`                        | Container bind port for MCP discovery HTTP server                   | `9090`                            |
| `registry.ttl`                             | Inactivity TTL window before purging stale agents                   | `"90s"`                           |
| `mcp.healthInterval`                       | Interval between background MCP health checks                       | `"30s"`                           |
| `otlp.endpoint`                            | gRPC endpoint for OTLP trace exporter (empty to disable tracing)    | `""`                              |
| `otlp.insecure`                            | Use plaintext gRPC for local development tracing                    | `false`                           |
| `workloadIdentity.enabled`                 | Enable keyless cloud IAM pod credentials                            | `false`                           |
| `workloadIdentity.provider`                | Cloud provider (`azure` or `aws`)                                   | `"azure"`                         |
| `workloadIdentity.azureClientId`           | Azure User-Assigned Managed Identity Client ID                      | `""`                              |
| `workloadIdentity.azureTenantId`           | Azure Active Directory Tenant ID                                    | `""`                              |
| `workloadIdentity.awsRoleArn`              | AWS IAM Role ARN for IRSA                                           | `""`                              |
