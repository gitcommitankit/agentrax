# Agentrax — Observability, Alerting & Distributed Tracing Guide

Agentrax provides enterprise-grade, full-stack observability purpose-built for multi-tenant AI and LLM agent workloads. This includes **custom Prometheus metrics**, **Prometheus alerting rules**, **pre-built Grafana dashboards**, and **OpenTelemetry distributed tracing** correlated with structured JSON logs.

---

## 1. Custom Prometheus Metrics

Agentrax instruments both operator latency and tenant quota saturation using native Prometheus histograms and gauges.

| Metric Name                           | Type      | Labels                 | Description                                                          |
| :------------------------------------ | :-------- | :--------------------- | :------------------------------------------------------------------- |
| `agentrax_reconcile_duration_seconds` | Histogram | `controller`, `tenant` | Wall-clock execution time of each reconcile loop per tenant.         |
| `agentrax_tenant_quota_usage_ratio`   | Gauge     | `tenant`               | Ratio of used replicas to maximum allowed replicas (`0.0` to `1.0`). |

### How to Scrape Operator Metrics

The operator exposes metrics on the `agentrax-metrics-service` Service in `agentrax-system`.

1. **Service Definition**:
   Port `8443` (HTTPS) or port `8080` (HTTP).
2. **Prometheus Operator Integration**:
   If using `kube-prometheus-stack`, create a `ServiceMonitor` targeting the service, or scrape using the Kubernetes API proxy:
   ```bash
   kubectl get --raw /api/v1/namespaces/agentrax-system/services/http:agentrax-metrics-service:http/proxy/metrics
   ```

---

## 2. Prometheus Alerting Rules (`PrometheusRule`)

Agentrax includes production-tested alerting thresholds packaged as a Kubernetes `PrometheusRule` resource in [`config/prometheus/alerting-rules.yaml`](../../config/prometheus/alerting-rules.yaml).

### Alert Specifications

| Alert Name                         | Severity   | Condition          | Window    | Action / Impact                                                                    |
| :--------------------------------- | :--------- | :----------------- | :-------- | :--------------------------------------------------------------------------------- |
| **`AgentraxReconcileLatencyHigh`** | `critical` | P99 latency > 2.0s | 5 minutes | Reconciler is stalled, experiencing API server rate limits or resource contention. |
| **`AgentraxTenantQuotaHigh`**      | `warning`  | Usage ratio > 0.90 | 2 minutes | Tenant has consumed >90% of allowed replicas; impending admission rejects.         |

### Applying Alerting Rules

```bash
kubectl apply -f config/prometheus/alerting-rules.yaml
```

Verify that Prometheus has loaded the rule:

```bash
kubectl get prometheusrule -n agentrax-system agentrax-alerts
```

---

## 3. Grafana Dashboard

Agentrax provides a curated, pre-built Grafana dashboard packaged as a ConfigMap in [`config/grafana/dashboard-configmap.yaml`](../../config/grafana/dashboard-configmap.yaml).

### Dashboard Panels

1. **Reconciliation Execution Rate**: Time-series showing operations per second broken down by tenant.
2. **Controller-Runtime Error Rate**: Real-time error frequency per controller.
3. **Reconcile Latency Percentiles**: P50 and P99 latency overlaid with the 2-second alert threshold.
4. **Tenant Quota Saturation Gauge**: Interactive gauge with dynamic `$tenant` dropdown (green `<80%`, yellow `80-90%`, red `>90%`).

### Importing the Dashboard

#### Automatic Sidecar Import (Recommended)

If running `kube-prometheus-stack`, applying the ConfigMap will automatically import the dashboard via the Grafana sidecar:

```bash
kubectl apply -f config/grafana/dashboard-configmap.yaml
```

#### Manual Import

Extract the JSON from the ConfigMap and import it into your Grafana UI under **Dashboards $\rightarrow$ New $\rightarrow$ Import**:

```bash
kubectl get configmap -n agentrax-system agentrax-grafana-dashboard -o jsonpath='{.data.agentrax-overview\.json}' > dashboard.json
```

---

## 4. Distributed Tracing (OpenTelemetry + Jaeger)

Agentrax features native **OpenTelemetry (OTel)** tracing across all critical reconciliation stages. Every trace is automatically correlated with structured `slog` logs.

```
[Agentrax Reconcile Request]
  │
  ├── span: "reconcile" (trace_id: a7f8b912c34...)
  │     │
  │     ├── span: "fetch_crd"
  │     ├── span: "reconcile_children"
  │     │     ├── span: "reconcile_deployment"
  │     │     ├── span: "reconcile_service"
  │     │     └── span: "reconcile_hpa"
  │     ├── span: "evaluate_canary_step" (during progressive rollouts)
  │     └── span: "update_status"
```

### Trace & Log Correlation

When OpenTelemetry is active, Agentrax extracts `trace_id` and `span_id` from the active context and injects them into structured JSON logs:

```json
{
  "time": "2026-09-05T08:35:12.104Z",
  "level": "INFO",
  "msg": "reconciled stable deployment",
  "controller": "agentdeployment",
  "tenant": "tenant-alpha",
  "trace_id": "8a31e8bc2973167b4582f0ad613e54c8",
  "span_id": "7b39f1c2901a5e42"
}
```

> **SRE Benefit**: When an error appears in logs, copy the `trace_id` and paste it directly into Jaeger to view the exact waterfall and call timing across the entire Kubernetes lifecycle!

---

## 5. Setting Up Tracing Locally with Jaeger

You can test distributed tracing in a local Kind cluster using the all-in-one Jaeger deployment:

### 1. Deploy Jaeger

```bash
kubectl create namespace agentrax-system --dry-run=client -o yaml | kubectl apply -f -

kubectl apply -n agentrax-system -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: jaeger
spec:
  replicas: 1
  selector:
    matchLabels:
      app: jaeger
  template:
    metadata:
      labels:
        app: jaeger
    spec:
      containers:
      - name: jaeger
        image: jaegertracing/all-in-one:latest
        ports:
        - containerPort: 4317
        - containerPort: 16686
---
apiVersion: v1
kind: Service
metadata:
  name: jaeger
spec:
  selector:
    app: jaeger
  ports:
  - name: otlp-grpc
    port: 4317
    targetPort: 4317
  - name: ui
    port: 16686
    targetPort: 16686
EOF
```

### 2. Configure Agentrax to Export Traces to Jaeger

Pass the OTLP flags to the manager:

```bash
--otlp-endpoint=jaeger.agentrax-system.svc:4317
--otlp-insecure=true
```

Or via Helm:

```bash
helm upgrade --install agentrax ./charts/agentrax \
  --namespace agentrax-system \
  --set otlp.endpoint=jaeger.agentrax-system.svc:4317 \
  --set otlp.insecure=true
```

### 3. View Spans in Jaeger UI

Port-forward the Jaeger web console to your browser:

```bash
kubectl port-forward -n agentrax-system svc/jaeger 16686:16686
```

Open [http://localhost:16686](http://localhost:16686) in your browser and select service `agentrax-operator` to inspect live traces!
