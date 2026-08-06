# Agentrax — Development Rules & Code Guidelines

## Go Code Conventions

1. **Error Handling**:
   - Always return errors up the call stack from reconcile helpers; never swallow them silently.
   - Wrap errors with context using `fmt.Errorf("reconciling deployment: %w", err)`.

2. **Reconciler Pattern**:
   - Fetch the object first; if not found (`apierrors.IsNotFound`), return `ctrl.Result{}` immediately — it was deleted.
   - Always update `status` last, after all child resources are reconciled. Never update status mid-reconcile.
   - Use `controllerutil.CreateOrUpdate` for all owned child resources (Deployment, Service, ServiceMonitor, HPA).
   - Requeue transient errors with `ctrl.Result{RequeueAfter: ...}`, not `ctrl.Result{Requeue: true}`.

3. **Owner References & Finalizers**:
   - Set owner references on all child resources so Kubernetes garbage collects them when the parent is deleted.
   - The `AgentDeploymentFinalizer` (`agentrax.io/mcp-deregister`) must be added on create and only removed after confirmed MCP deregistration.

4. **Status Conditions**:
   - Use `meta.SetStatusCondition` from `k8s.io/apimachinery/pkg/api/meta` — never manipulate `status.conditions` slices directly.
   - Standard condition types are defined as constants in `api/v1alpha1/agentdeployment_types.go`.

5. **API Group**:
   - The API group is `agentrax.io` (not `agentrax.agentrax.io`). All `+kubebuilder:rbac` markers and `groupversion_info.go` must use `agentrax.io`.

## Commenting & Documentation Standards

1. **Essential, Non-Redundant Comments Only**:
   - Comments must be concise. Don't state what the code already shows.
   - Only add comments to explain _why_ non-obvious logic exists (e.g., concurrency edge cases, finalizer ordering, in-flight quota reservations, sample-size gating).

2. **Mandatory GoDoc on All Exported Symbols**:
   - Every exported package, type, struct field, constant, variable, and function MUST have a GoDoc comment starting with the symbol name.
   - Example: `// AgentDeploymentSpec defines the desired state of an AgentDeployment.`

3. **CRD Field Comments Double as OpenAPI Descriptions**:
   - Comments on fields in `api/v1alpha1/` are parsed by `controller-gen` into CRD OpenAPI schema descriptions. Keep them user-facing and precise.

4. **Update Docs on Architecture Changes**:
   - When an architecture boundary or CRD field changes, update `docs/agentrax.md` and `.agents/skills/agentrax-context/SKILL.md` in the same commit.
