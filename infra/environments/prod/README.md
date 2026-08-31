# Production Environment — Azure AKS (Stub)

This environment targets an Azure Kubernetes Service (AKS) cluster for production workloads. The `agentrax_stack` module is cloud-agnostic; the production configuration differs from `dev` in four ways:

1. **Remote State Backend**: State stored in Azure Blob Storage with state locking.
2. **Cloud Provider Authentication**: Uses `azurerm` / `azapi` providers authenticated via Azure OIDC / Workload Identity.
3. **High Availability**: `agentrax_leader_elect = true` with $\ge 2$ controller replicas.
4. **Workload Identity**: Cloud identity parameters passed via `agentrax_extra_values`.

### Activation Runbook (Future — Not Yet Implemented)

> [!NOTE]
> The `infra/environments/prod/` directory is a stub. No `main.tf` exists here yet. The steps below are guidance for when the production Terraform root module is implemented.

1. Provision the target AKS cluster and retrieve its kubeconfig credentials.
2. Configure `backend.tf` with the Azure Blob Storage container coordinates.
3. Export Azure authentication environment variables (`ARM_CLIENT_ID`, `ARM_TENANT_ID`, `ARM_SUBSCRIPTION_ID`, `ARM_USE_OIDC=true`).
4. Execute deployment:
   ```bash
   terraform -chdir=infra/environments/prod init
   terraform -chdir=infra/environments/prod apply
   ```
