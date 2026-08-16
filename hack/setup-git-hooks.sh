#!/usr/bin/env bash
# Copyright 2026.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -o errexit
set -o nounset
set -o pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOOKS_DIR="${REPO_ROOT}/.git/hooks"

if [ ! -d "${HOOKS_DIR}" ]; then
    echo "Error: .git/hooks directory not found. Are you in a git repository?"
    exit 1
fi

echo "Setting up Agentrax Git hooks in ${HOOKS_DIR}..."

# 1. Create pre-commit hook
cat << 'EOF' > "${HOOKS_DIR}/pre-commit"
#!/usr/bin/env bash
set -e

echo "🔍 [pre-commit] Checking formatting, vet, and lint..."

# Run go fmt
echo "  -> go fmt ./..."
go fmt ./...

# Run go vet
echo "  -> go vet ./..."
go vet ./...

# Run golangci-lint
echo "  -> golangci-lint run..."
if [ -f "./bin/golangci-lint" ]; then
    ./bin/golangci-lint run
else
    make lint
fi

echo "✅ [pre-commit] All pre-commit checks passed!"
EOF

chmod +x "${HOOKS_DIR}/pre-commit"

# 2. Create pre-push hook
cat << 'EOF' > "${HOOKS_DIR}/pre-push"
#!/usr/bin/env bash
set -e

echo "🧪 [pre-push] Running full test suite and manifest verification..."

# Verify manifests and code generation
echo "  -> make manifests generate..."
make manifests generate

# Run test suite
echo "  -> make test..."
make test

# Verify kustomize render
echo "  -> Verifying config/default kustomization..."
if [ -f "./bin/kustomize" ]; then
    ./bin/kustomize build config/default > /dev/null
fi

echo "✅ [pre-push] All pre-push checks passed!"
EOF

chmod +x "${HOOKS_DIR}/pre-push"

echo "✨ Git hooks installed successfully! (pre-commit & pre-push)"
