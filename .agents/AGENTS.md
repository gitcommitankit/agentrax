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

## Tooling & Developer Environment

### Serena (MCP — Semantic Code Intelligence)

This project supports **Serena** configured as an MCP server.
Serena exposes `gopls`-backed semantic tools via the Model Context Protocol. Note that Go and `gopls` must be installed and available on `PATH` before use.
The project is indexed and the config lives at `.serena/project.yml`.

**Configuration & Setup:**
Configure your MCP client with `--project-from-cwd` (for example, `serena start-mcp-server --project-from-cwd`). Serena will automatically select the project root by finding the nearest `.serena/project.yml` or `.git` boundary.

**Use Serena tools instead of text search for the following tasks:**

| Task | Use instead of |
| ---- | -------------- |
| Find where a type, function, or constant is defined | `find_symbol` / `find_declaration` rather than `grep` |
| Find all usages/call sites of a symbol across packages | `find_referencing_symbols` rather than `grep -r` |
| Understand what symbols a file or package exports | `get_symbols_overview` rather than skimming the file |
| Rename a symbol consistently across all packages | `rename_symbol` rather than manual multi-file sed |
| Navigate to where an interface is implemented | `find_implementations` rather than text search |
| Check diagnostics/type errors before proposing a fix | `get_diagnostics_for_file` |

**When NOT to use Serena:**
- Simple single-file reads — `view_file` is faster.
- Writing or replacing file content — use the standard edit tools.
- Searching for plain string literals (log messages, YAML values) — `grep` is fine.
