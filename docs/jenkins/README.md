# Jenkins CI/CD for Agentrax

This document explains how to run the Agentrax Jenkins pipeline locally using Docker.

---

## Prerequisites

- Docker installed and running
- `kubectl` configured with access to a running `kind` cluster
- A GitHub Container Registry (GHCR) token with `write:packages` scope

---

## 1. Run Jenkins in Docker

```bash
docker run -d --name jenkins \
  -p 8080:8080 \
  -p 50000:50000 \
  -v jenkins_home:/var/jenkins_home \
  -v /var/run/docker.sock:/var/run/docker.sock \
  jenkins/jenkins:lts-jdk17
```

The `-v /var/run/docker.sock` mount gives Jenkins agents access to the host Docker daemon so `make docker-build` works without Docker-in-Docker complexity.

Retrieve the initial admin password:

```bash
docker exec jenkins cat /var/jenkins_home/secrets/initialAdminPassword
```

Open `http://localhost:8080` and complete the setup wizard. Install the **recommended plugins** plus:

- **Pipeline** (usually included)
- **AnsiColor**
- **Slack Notification**

---

## 2. Create Jenkins Credentials

In **Manage Jenkins → Credentials → (global)**, add:

| ID           | Kind        | Value                                  |
| ------------ | ----------- | -------------------------------------- |
| `GHCR_USER`  | Secret text | Your GitHub username                   |
| `GHCR_TOKEN` | Secret text | PAT with `write:packages` scope        |

---

## 3. Create a Multibranch Pipeline

1. **New Item → Multibranch Pipeline** — name it `agentrax`.
2. Under **Branch Sources**, add a **GitHub** source:
   - Repository URL: `https://github.com/gitcommitankit/agentrax`
   - Credentials: add a GitHub PAT credential for private access if needed.
3. Under **Build Configuration**, set:
   - **Mode**: `by Jenkinsfile`
   - **Script Path**: `Jenkinsfile` (the default)
4. Save and let Jenkins scan branches. It automatically discovers `main` and any feature branches.

---

## 4. Pipeline Stages

```
Lint ──────────────────────────┐
                               │ (parallel)
Helm Lint ──────────────────── ┘
     │
     ▼
   Test
     │
     ▼
 Docker Build  (push on main only)
     │
     ▼
 Integration Test  ← Agentrax-specific
     │  make deploy-deps
     │  make deploy
     │  hack/assert-reconciliation.sh
     │  [always] make undeploy
     ▼
 Helm Deploy  (main only, manual approval)
```

| Stage | What runs | Fail condition |
| :--- | :--- | :--- |
| **Lint** | `make lint` + `helm lint charts/agentrax/` | Any lint error |
| **Test** | `make test` (unit + envtest) | Any test failure |
| **Docker Build** | `make docker-build` + push on `main` | Docker build error |
| **Integration Test** | `deploy-deps` → `deploy` → `assert-reconciliation.sh` | Reconciliation timeout or terminal phase |
| **Helm Deploy** | `helm upgrade --install ... --atomic` | Requires ops-team approval; rollback on hook failure |

---

## 5. Integration Test in Detail

**Stage 4** is the Agentrax-specific addition. It:

1. Installs `cert-manager`, Prometheus Operator CRDs, and Gateway API CRDs via `make deploy-deps` (idempotent).
2. Deploys the newly built operator image into the cluster via `make deploy`.
3. Runs [`hack/assert-reconciliation.sh`](../../hack/assert-reconciliation.sh), which:
   - Creates the `agentrax-jenkins-test` namespace.
   - Applies [`hack/testdata/sample-agentdeployment.yaml`](../../hack/testdata/sample-agentdeployment.yaml).
   - Polls `status.phase` every 3 seconds until `Running` or 60-second timeout.
   - Exits non-zero on `RolloutFailed`, `Degraded`, or timeout — failing the Jenkins stage.

The `post { always }` block runs `make undeploy` regardless of pass/fail, keeping the cluster clean for subsequent builds.

---

## 6. Slack Notifications

Configure the Jenkins Slack plugin (**Manage Jenkins → System → Slack**):

- Workspace: your Slack workspace name
- Credential: add a **Secret text** credential containing the Slack Bot token
- Default channel: `#agentrax-ci`

The pipeline posts:
- `❌ FAILED` on any stage failure
- `✅ passed` on a successful full pipeline run

---

## 7. Updating the Pipeline

The `Jenkinsfile` lives at the repository root. Changes are picked up automatically on the next branch scan or build trigger — no Jenkins UI changes required.
