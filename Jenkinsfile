// Agentrax — Declarative Jenkins CI/CD Pipeline
//
// Stages:
//   1. Lint            — golangci-lint + Helm chart lint (parallel)
//   2. Test            — unit & integration tests via envtest
//   3. Docker Build    — build image tagged with short Git SHA, push on main
//   4. Integration Test — deploy to kind test namespace, assert reconciliation
//   5. Helm Deploy     — manual approval gate then helm upgrade --install
//
// Requirements:
//   - Jenkins agent with label 'docker' and Docker-in-Docker socket access
//   - Credentials: GHCR_USER (string), GHCR_TOKEN (secret text)
//   - Jenkins Slack plugin configured for Slack notifications

pipeline {
  agent { label 'docker' }

  environment {
    // Go workspace inside the Jenkins workspace to avoid polluting $HOME
    GOPATH     = "${WORKSPACE}/.go"
    GOMODCACHE = "${WORKSPACE}/.go/pkg/mod"
    // Image tag is the short Git SHA for traceability
    IMAGE_TAG  = "${env.GIT_COMMIT?.take(8) ?: env.BUILD_NUMBER}"
    IMAGE      = "ghcr.io/gitcommitankit/agentrax:${IMAGE_TAG}"
    // Kubernetes namespace used exclusively for integration testing
    TEST_NS      = "agentrax-jenkins-test"
    // Kind cluster and kubectl context for integration testing
    KIND_CLUSTER = "${env.KIND_CLUSTER ?: 'agentrax-dev'}"
    KUBE_CONTEXT = "${env.KUBE_CONTEXT ?: 'kind-agentrax-dev'}"
  }

  options {
    // Abort if the full pipeline exceeds 45 minutes
    timeout(time: 45, unit: 'MINUTES')
    // Keep the last 10 build logs; discard older ones to save disk space
    buildDiscarder(logRotator(numToKeepStr: '10'))
    // Prevent concurrent builds on the same branch to avoid races on the
    // shared kind cluster used in Stage 4
    disableConcurrentBuilds()
    ansiColor('xterm')
  }

  stages {

    // -----------------------------------------------------------------------
    // Stage 1: Lint
    // Runs golangci-lint and Helm chart lint in parallel.
    // -----------------------------------------------------------------------
    stage('Lint') {
      parallel {
        stage('Go Lint') {
          steps {
            sh 'make golangci-lint'
            sh 'make lint'
          }
        }
        stage('Helm Lint') {
          steps {
            sh 'helm lint charts/agentrax/'
            sh 'helm template test charts/agentrax/ --debug > /dev/null'
          }
        }
      }
    }

    // -----------------------------------------------------------------------
    // Stage 2: Test
    // Runs the full unit + envtest integration test suite.
    // -----------------------------------------------------------------------
    stage('Test') {
      steps {
        sh 'make envtest'
        sh 'make test'
      }
      post {
        always {
          archiveArtifacts artifacts: 'cover.out', allowEmptyArchive: true
        }
      }
    }

    // -----------------------------------------------------------------------
    // Stage 3: Docker Build
    // Builds the operator image. Pushes to GHCR only on the main branch.
    // Credentials are bound strictly within the main-branch push path.
    // -----------------------------------------------------------------------
    stage('Docker Build') {
      steps {
        sh "make docker-build IMG=${IMAGE}"
        script {
          if (env.BRANCH_NAME == 'main') {
            withCredentials([
              string(credentialsId: 'GHCR_USER',  variable: 'GHCR_USER'),
              string(credentialsId: 'GHCR_TOKEN', variable: 'GHCR_TOKEN'),
            ]) {
              sh 'echo "${GHCR_TOKEN}" | docker login ghcr.io -u "${GHCR_USER}" --password-stdin'
              sh "make docker-push IMG=${IMAGE}"
            }
          } else {
            echo "Feature branch — image built but not pushed (branch=${env.BRANCH_NAME})."
          }
        }
      }
    }

    // -----------------------------------------------------------------------
    // Stage 4: Integration Test  (Agentrax-specific stage)
    //
    // 1. Installs cert-manager, Prometheus Operator CRDs, and Gateway API CRDs
    //    via `make deploy-deps` (idempotent).
    // 2. Loads the locally built image into the kind cluster if kind is present.
    // 3. Deploys the operator into the cluster with the newly built image.
    // 4. Runs hack/assert-reconciliation.sh — polls until a sample
    //    AgentDeployment reaches status.phase == Running (60 s timeout).
    //
    // In post.always, the test namespace is deleted FIRST while the operator
    // is still running so finalizers (agentrax.io/mcp-deregister) can execute
    // cleanly, followed by `make undeploy`.
    // -----------------------------------------------------------------------
    stage('Integration Test') {
      steps {
        sh "make deploy-deps KUBECTL=\"kubectl --context ${KUBE_CONTEXT}\""
        sh "kind load docker-image ${IMAGE} --name ${KIND_CLUSTER}"
        sh "make deploy IMG=${IMAGE} KUBECTL=\"kubectl --context ${KUBE_CONTEXT}\""
        sh "TEST_NS=${TEST_NS} KUBECTL=\"kubectl --context ${KUBE_CONTEXT}\" ./hack/assert-reconciliation.sh"
      }
      post {
        always {
          sh "kubectl --context ${KUBE_CONTEXT} delete namespace ${TEST_NS} --ignore-not-found=true"
          sh "make undeploy KUBECTL=\"kubectl --context ${KUBE_CONTEXT}\" || true"
        }
      }
    }

    // -----------------------------------------------------------------------
    // Stage 5: Helm Deploy
    //
    // Runs only on the main branch. Requires explicit approval from the
    // ops-team before mutating the production cluster. --atomic ensures Helm
    // rolls back automatically if any post-install hook fails.
    // -----------------------------------------------------------------------
    stage('Helm Deploy') {
      when { branch 'main' }
      input {
        message "Deploy agentrax:${IMAGE_TAG} to production cluster?"
        ok 'Approve'
        submitter 'ops-team'
      }
      steps {
        sh """
          helm upgrade --install agentrax charts/agentrax/ \
            --namespace agentrax-system \
            --create-namespace \
            --set image.tag=${IMAGE_TAG} \
            --atomic \
            --timeout 5m
        """
      }
    }
  }

  post {
    failure {
      slackSend(
        color: 'danger',
        message: "❌ Agentrax build #${BUILD_NUMBER} FAILED on \`${BRANCH_NAME}\`: ${BUILD_URL}",
      )
    }
    success {
      slackSend(
        color: 'good',
        message: "✅ Agentrax build #${BUILD_NUMBER} passed on \`${BRANCH_NAME}\` (image: \`${IMAGE_TAG}\`)",
      )
    }
  }
}
