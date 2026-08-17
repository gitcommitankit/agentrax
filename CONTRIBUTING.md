# Contributing to Agentrax

Thank you for your interest in contributing to **Agentrax**! We welcome contributions from developers, DevOps engineers, and AI infrastructure enthusiasts.

---

## Code of Conduct

Please be respectful and collaborative in all communications, issue threads, and pull requests.

---

## Development Prerequisites

Ensure you have the following installed on your development machine:

- **Go**: `1.23` or later
- **Docker**: `20.10+` with BuildKit enabled
- **Kind**: `v0.22+` (for local E2E cluster testing)
- **Kubectl**: `v1.30+`
- **Helm**: `v3.14+`
- **Kustomize & Controller-Gen**: Automatically managed via `make kustomize` and `make controller-gen` (installed to `./bin`)

---

## Getting Started

1. **Fork and Clone**:
   ```bash
   git clone https://github.com/<your-username>/agentrax.git
   cd agentrax
   ```

2. **Install Local Git Quality Hooks**:
   ```bash
   make setup-git-hooks
   ```
   *Installs pre-commit hooks (`go fmt`, `go vet`, `golangci-lint`) and pre-push hooks (`codegen-drift`, `make test`, `helm lint`).*

3. **Run Unit & EnvTest Integration Tests**:
   ```bash
   make test
   ```

4. **Run Linter**:
   ```bash
   make lint
   ```

5. **Generate Code & CRD Manifests**:
   If you modify types in `api/v1alpha1/`, regenerate DeepCopy methods, CRD schemas, and RBAC roles:
   ```bash
   make manifests generate
   ```

---

## Running Local E2E Tests (Kind)

To test changes against a live Kind cluster:

```bash
# 1. Create Kind cluster
kind create cluster --name agentrax-e2e

# 2. Deploy external dependencies (cert-manager, Prometheus CRDs, Gateway API CRDs)
make deploy-deps

# 3. Run the automated live E2E suite
go test ./test/e2e/... -v -count=1 --timeout 15m
```

---

## Code Guidelines & Standards

- **Error Wrapping**: Wrap errors with context (e.g. `fmt.Errorf("reconciling deployment: %w", err)`).
- **GoDoc Comments**: Every exported package, type, constant, and function must have a GoDoc comment starting with the symbol name.
- **CRD Comments**: Comments on fields in `api/v1alpha1/` double as OpenAPI schema descriptions—keep them clear and precise.
- **Status Updates**: Always update status last in reconcile loops using `apimeta.SetStatusCondition`.

---

## Submitting Pull Requests

1. Create a feature branch: `git checkout -b feature/my-feature-name`.
2. Commit your changes with clear, descriptive commit messages.
3. Ensure all tests pass: `make lint && make test`.
4. Open a Pull Request against the `main` branch with a description of the problem solved and testing performed.
