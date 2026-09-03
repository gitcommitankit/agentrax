# Agentrax Observability

This document covers the three pillars of Agentrax operator observability:
**Prometheus metrics**, **structured JSON logging**, and **OpenTelemetry distributed tracing**.

---

## 1. Prometheus Metrics

Metrics are registered by `internal/observability/metrics.go` and exposed on the
`/metrics` endpoint (port 8080/8443 depending on `--metrics-secure`).

| Metric | Type | Labels | Description |
|---|---|---|---|
| `agentrax_reconcile_duration_seconds` | Histogram | `controller`, `tenant` | Wall-clock duration of each reconcile loop |
| `agentrax_quota_usage_ratio` | Gauge | `tenant` | Fraction of GPU/CPU quota consumed per tenant namespace |

Alerting rules live in [`config/prometheus/alerting-rules.yaml`](../../config/prometheus/alerting-rules.yaml).
The Grafana RED dashboard is in [`config/grafana/dashboard-configmap.yaml`](../../config/grafana/dashboard-configmap.yaml).

---

## 2. Structured JSON Logging

The operator emits logs as newline-delimited JSON to stdout using standard library `log/slog`, parseable by any log aggregator (Loki, Fluentd, Datadog, Cloud Logging).

### Log Level

Pass `--log-level=<level>` to the operator binary. Valid values: `debug`, `info`, `warn`, `error`.  
Default: `info`.

### Sample Log Record

```json
{
  "time": "2026-09-03T19:53:34.560879602Z",
  "level": "INFO",
  "msg": "reconciled AgentDeployment",
  "controller": "agentdeployment",
  "namespace": "tenant-search",
  "name": "query-agent",
  "trace_id": "f21eda3de0fc29df53aecfd7ea070e04",
  "span_id": "2ccc8979cbed2acf",
  "phase": "Degraded",
  "readyReplicas": 0
}
```

When `--otlp-endpoint` is set, every log record produced inside a reconcile loop
includes `trace_id` and `span_id` fields so logs can be correlated to the
matching trace in Jaeger or any OTLP-compatible backend.

---

## 3. OpenTelemetry Distributed Tracing

### Configuration

| Flag | Default | Description |
|---|---|---|
| `--otlp-endpoint` | `""` (disabled) | gRPC endpoint of the OTLP-compatible trace collector, e.g. `localhost:4317` |

When the flag is empty the operator installs a **no-op** tracer — zero overhead,
no external dependency required. Set the flag to enable live tracing.

### Span Hierarchy

Each invocation of `AgentDeploymentReconciler.Reconcile()` produces the following span tree:

```
reconcile  [tenant=<namespace>, name=<agentdeployment-name>]
├── fetch_crd
├── reconcile_children
│   ├── (Deployment reconcile)
│   ├── (Service reconcile)
│   ├── (ServiceMonitor reconcile)
│   └── (HPA + quota reconcile)
└── update_status
```

Errors in any child span are recorded with `span.RecordError(err)` and the span
status is set to `codes.Error` so they surface in trace UIs without log correlation.

### Local Development with Jaeger

Start Jaeger all-in-one (OTLP gRPC on 4317, UI on 16686):

```bash
docker run -d --name jaeger \
  -p 4317:4317 \
  -p 16686:16686 \
  jaegertracing/all-in-one:latest
```

Run the operator locally:

```bash
go run ./cmd/main.go \
  --otlp-endpoint=localhost:4317 \
  --log-level=debug \
  --metrics-secure=false \
  --registry-bind-address=:9091
```

Apply a sample `AgentDeployment`:

```bash
kubectl apply -f config/samples/agentrax_v1alpha1_agentdeployment.yaml
```

Open Jaeger UI at <http://localhost:16686>, select service **agentrax-operator**,
and click any `reconcile` trace to see the full span waterfall.

### Trace-to-Log Correlation

1. Copy the `trace_id` from the Jaeger trace detail pane.
2. In your log aggregator query for `trace_id = "<value>"`.
3. All log lines emitted during that reconcile cycle will match.

### Sampling

By default every span is sampled (`AlwaysSample`). For production high-throughput
namespaces, reduce sampling using the standard OTel environment variable:

```bash
OTEL_TRACES_SAMPLER=parentbased_traceidratio
OTEL_TRACES_SAMPLER_ARG=0.1   # 10%
```
