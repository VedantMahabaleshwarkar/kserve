#!/bin/bash
set -o errexit
set -o nounset
set -o pipefail

echo "=== KServe E2E Codespace Setup ==="

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Versions (matching CI: .github/actions/minikube-setup/action.yml)
KUBECTL_VERSION="v1.34.4"
MINIKUBE_VERSION="v1.38.1"

# --- kubectl ---
if command -v kubectl &>/dev/null && [[ "$(kubectl version --client -o json 2>/dev/null | jq -r '.clientVersion.gitVersion')" == "${KUBECTL_VERSION}" ]]; then
    echo "kubectl ${KUBECTL_VERSION} already installed"
else
    echo "Installing kubectl ${KUBECTL_VERSION}..."
    curl -sLO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl"
    chmod +x kubectl
    sudo mv kubectl /usr/local/bin/kubectl
    echo "kubectl $(kubectl version --client --short 2>/dev/null || kubectl version --client) installed"
fi

# --- minikube ---
if command -v minikube &>/dev/null && [[ "$(minikube version --short 2>/dev/null)" == "${MINIKUBE_VERSION}" ]]; then
    echo "minikube ${MINIKUBE_VERSION} already installed"
else
    echo "Installing minikube ${MINIKUBE_VERSION}..."
    curl -sLO "https://storage.googleapis.com/minikube/releases/${MINIKUBE_VERSION}/minikube-linux-amd64"
    chmod +x minikube-linux-amd64
    sudo mv minikube-linux-amd64 /usr/local/bin/minikube
    echo "minikube $(minikube version --short) installed"
fi

# --- helm, kustomize, yq (via existing repo scripts) ---
export REPO_ROOT
export BIN_DIR="/usr/local/bin"
source "${REPO_ROOT}/kserve-deps.env"

echo "Installing helm ${HELM_VERSION}..."
"${REPO_ROOT}/hack/setup/cli/install-helm.sh"

echo "Installing kustomize ${KUSTOMIZE_VERSION}..."
"${REPO_ROOT}/hack/setup/cli/install-kustomize.sh"

echo "Installing yq ${YQ_VERSION}..."
"${REPO_ROOT}/hack/setup/cli/install-yq.sh"

# --- uv ---
echo "Installing uv..."
"${REPO_ROOT}/hack/setup/cli/install-uv.sh"

# --- jq (needed by various scripts) ---
if ! command -v jq &>/dev/null; then
    echo "Installing jq..."
    sudo apt-get update -qq && sudo apt-get install -y -qq jq
fi

echo ""
echo "=== Setup Complete ==="
echo "Tools installed:"
echo "  kubectl:    $(kubectl version --client -o json 2>/dev/null | jq -r '.clientVersion.gitVersion' 2>/dev/null || echo 'installed')"
echo "  minikube:   $(minikube version --short 2>/dev/null || echo 'installed')"
echo "  helm:       $(helm version --short 2>/dev/null || echo 'installed')"
echo "  kustomize:  $(kustomize version --short 2>/dev/null || kustomize version 2>/dev/null || echo 'installed')"
echo "  yq:         $(yq --version 2>/dev/null || echo 'installed')"
echo "  go:         $(go version 2>/dev/null || echo 'installed')"
echo "  python:     $(python3 --version 2>/dev/null || echo 'installed')"
echo "  uv:         $(uv --version 2>/dev/null || echo 'installed')"
echo ""
echo "Next: run './test/scripts/codespaces/run-e2e.sh <suite>' to start testing"
echo "  e.g. ./test/scripts/codespaces/run-e2e.sh predictor"
echo "  e.g. ./test/scripts/codespaces/run-e2e.sh modelcache"
