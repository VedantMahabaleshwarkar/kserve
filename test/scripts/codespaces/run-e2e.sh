#!/bin/bash

# Copyright 2025 The KServe Authors.
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

# Orchestrator script for running KServe e2e tests in GitHub Codespaces.
# Delegates to existing gh-actions scripts for each stage.
#
# Usage: ./test/scripts/codespaces/run-e2e.sh <suite> [options]
#
# Suites (pytest markers):
#   predictor, explainer, graph, raw, helm, llm, vllm, modelcache,
#   "transformer or mms or collocation", "llminferenceservice and cluster_cpu",
#   path_based_routing, kourier, autoscaling, rawcipn
#
# Options:
#   --skip-build         Skip image builds (reuse existing)
#   --skip-setup         Skip minikube + deps + kserve setup
#   --install-method     "kustomize" (default) or "helm"
#   --network-layer      "istio" (default), "istio-ingress", "envoy-gatewayapi", "istio-gatewayapi"
#   --parallelism N      pytest parallelism (default varies by suite)
#   --teardown           Tear down minikube and exit
#   --force-deps         Force re-install dependencies even if already present
#   --force-install      Force re-install KServe even if already running

set -o errexit
set -o nounset
set -o pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
GH_ACTIONS_DIR="${PROJECT_ROOT}/test/scripts/gh-actions"

# ============================================================================
# Argument Parsing
# ============================================================================

SUITE=""
SKIP_BUILD=false
SKIP_SETUP=false
INSTALL_METHOD="kustomize"
NETWORK_LAYER="istio"
PARALLELISM=""
TEARDOWN=false
FORCE_DEPS=false
FORCE_INSTALL=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --skip-build)      SKIP_BUILD=true; shift ;;
        --skip-setup)      SKIP_SETUP=true; shift ;;
        --install-method)  INSTALL_METHOD="$2"; shift 2 ;;
        --network-layer)   NETWORK_LAYER="$2"; shift 2 ;;
        --parallelism)     PARALLELISM="$2"; shift 2 ;;
        --teardown)        TEARDOWN=true; shift ;;
        --force-deps)      FORCE_DEPS=true; shift ;;
        --force-install)   FORCE_INSTALL=true; shift ;;
        --help|-h)
            head -35 "$0" | tail -28
            exit 0
            ;;
        -*)
            echo "Unknown option: $1" >&2
            exit 1
            ;;
        *)
            if [[ -z "$SUITE" ]]; then
                SUITE="$1"
            else
                echo "Unexpected argument: $1 (suite already set to '$SUITE')" >&2
                exit 1
            fi
            shift
            ;;
    esac
done

# Handle --teardown
if [[ "$TEARDOWN" == true ]]; then
    echo "Tearing down minikube..."
    minikube delete --all 2>/dev/null || true
    echo "Done."
    exit 0
fi

if [[ -z "$SUITE" ]]; then
    echo "Usage: $0 <suite> [options]" >&2
    echo "Run '$0 --help' for details." >&2
    exit 1
fi

# ============================================================================
# Environment Setup
# ============================================================================

export KO_DOCKER_REPO="${KO_DOCKER_REPO:-kserve}"
export TAG="${TAG:-$(git -C "${PROJECT_ROOT}" rev-parse HEAD)}"
export DOCKER_IMAGES_PATH="${DOCKER_IMAGES_PATH:-/tmp/docker-images}"

source "${PROJECT_ROOT}/kserve-images.sh"

# Export for sub-scripts
export GITHUB_SHA="${TAG}"
export INSTALL_METHOD
export NETWORK_LAYER

# ============================================================================
# Suite Configuration
# ============================================================================

# Default parallelism per suite
get_default_parallelism() {
    case "$1" in
        modelcache|explainer|vllm) echo "1" ;;
        llm)                       echo "2" ;;
        *)                         echo "6" ;;
    esac
}

# Deployment mode per suite
get_deployment_mode() {
    case "$1" in
        raw|rawcipn|autoscaling) echo "raw" ;;
        *)                       echo "serverless" ;;
    esac
}

# Whether suite needs multi-node minikube
needs_multinode() {
    [[ "$1" == "modelcache" ]]
}

# Whether suite needs metrics-server
needs_metrics_server() {
    case "$1" in
        raw|rawcipn|autoscaling) return 0 ;;
        *) return 1 ;;
    esac
}

# Whether suite needs KEDA
needs_keda() {
    case "$1" in
        raw|rawcipn|autoscaling) return 0 ;;
        *) return 1 ;;
    esac
}

# Whether this is an LLMISvc suite
is_llmisvc_suite() {
    [[ "$1" == *"llminferenceservice"* ]]
}

# Which build scripts to run per suite
# Returns: "core" and/or "predictor,transformer" "explainer" "graph" "llmisvc"
get_build_targets() {
    local suite="$1"
    case "$suite" in
        predictor)
            echo "core predictor,transformer" ;;
        "transformer or mms or collocation"|transformer|mms|collocation)
            echo "core predictor,transformer explainer" ;;
        explainer)
            echo "core explainer predictor,transformer" ;;
        graph)
            echo "core predictor graph" ;;
        raw|rawcipn|path_based_routing)
            echo "core predictor,transformer explainer" ;;
        helm)
            echo "core" ;;
        kourier)
            echo "core predictor graph" ;;
        llm|vllm|modelcache)
            echo "core predictor,transformer" ;;
        autoscaling)
            echo "core predictor" ;;
        *llminferenceservice*)
            echo "llmisvc" ;;
        *)
            echo "core predictor,transformer" ;;
    esac
}

PARALLELISM="${PARALLELISM:-$(get_default_parallelism "$SUITE")}"
DEPLOYMENT_MODE="$(get_deployment_mode "$SUITE")"

# ============================================================================
# Status Display
# ============================================================================

print_status() {
    local label="$1"
    local ok="$2"
    local detail="${3:-}"
    if [[ "$ok" == true ]]; then
        echo "[ok] ${label}${detail:+ ($detail)}"
    else
        echo "[..] ${label}${detail:+ - $detail}"
    fi
}

show_status() {
    echo ""
    echo "=== KServe E2E Environment Status ==="
    echo "Suite:          ${SUITE}"
    echo "Install method: ${INSTALL_METHOD}"
    echo "Network layer:  ${NETWORK_LAYER}"
    echo "Parallelism:    ${PARALLELISM}"
    echo "Deploy mode:    ${DEPLOYMENT_MODE}"
    echo ""

    # Minikube
    local mk_running=false
    local mk_detail=""
    if minikube status -f '{{.Host}}' 2>/dev/null | grep -q "Running"; then
        mk_running=true
        local node_count
        node_count=$(kubectl get nodes --no-headers 2>/dev/null | wc -l | tr -d ' ')
        mk_detail="${node_count} node(s)"
    else
        mk_detail="will start"
    fi
    print_status "Minikube" "$mk_running" "$mk_detail"

    # Dependencies
    local deps_ok=false
    if kubectl get namespace kserve &>/dev/null 2>&1; then
        deps_ok=true
    fi
    print_status "Dependencies" "$deps_ok" ""

    # KServe
    local kserve_ok=false
    if kubectl get deployment kserve-controller-manager -n kserve &>/dev/null 2>&1; then
        kserve_ok=true
    elif is_llmisvc_suite "$SUITE" && kubectl get deployment llmisvc-controller-manager -n kserve &>/dev/null 2>&1; then
        kserve_ok=true
    fi
    print_status "KServe installed" "$kserve_ok" "${INSTALL_METHOD}"

    # Images
    local images_ok=false
    if [[ "$SKIP_BUILD" == true ]]; then
        images_ok=true
        print_status "Images" "$images_ok" "skip-build set"
    else
        print_status "Images" "$images_ok" "will build"
    fi

    echo ""
}

show_status

# ============================================================================
# Stage 1: Minikube
# ============================================================================

start_minikube() {
    if [[ "$SKIP_SETUP" == true ]]; then
        echo "--- Skipping minikube setup (--skip-setup) ---"
        return
    fi

    if minikube status -f '{{.Host}}' 2>/dev/null | grep -q "Running"; then
        echo "--- Minikube already running, skipping start ---"
        return
    fi

    echo "=== Starting Minikube ==="

    if needs_multinode "$SUITE"; then
        sudo mkdir -p /tmp-images
        sudo chown -R "$(id -u):$(id -g)" /tmp-images
        minikube start \
            --driver=docker \
            --cpus=max \
            --memory=max \
            --kubernetes-version=v1.34.4 \
            --wait=all \
            --wait-timeout=6m0s \
            --nodes=3 \
            --mount --mount-string=/tmp-images:/tmp-images
        nohup minikube tunnel > /tmp/minikube-tunnel.log 2>&1 &
        echo "Minikube tunnel started in background"
    else
        minikube start \
            --driver=docker \
            --cpus=max \
            --memory=max \
            --kubernetes-version=v1.34.4 \
            --wait=all \
            --wait-timeout=6m0s
    fi

    kubectl get nodes
    kubectl get pods -n kube-system
}

# ============================================================================
# Stage 2: Dependencies
# ============================================================================

install_deps() {
    if [[ "$SKIP_SETUP" == true ]]; then
        echo "--- Skipping dependency setup (--skip-setup) ---"
        return
    fi

    if [[ "$FORCE_DEPS" != true ]] && kubectl get namespace kserve &>/dev/null 2>&1; then
        echo "--- Dependencies appear installed (kserve namespace exists), skipping ---"
        echo "    Use --force-deps to reinstall"
        return
    fi

    echo "=== Installing Dependencies ==="

    local enable_keda="false"
    if needs_keda "$SUITE"; then
        enable_keda="true"
        # Enable metrics-server addon first
        minikube addons enable metrics-server
        kubectl rollout status deployment/metrics-server -n kube-system --timeout=180s
    fi

    local enable_llmisvc="false"
    if is_llmisvc_suite "$SUITE"; then
        enable_llmisvc="true"
    fi

    "${GH_ACTIONS_DIR}/setup-deps.sh" "${DEPLOYMENT_MODE}" "${NETWORK_LAYER}" "${enable_keda}" "${enable_llmisvc}"

    # Run test overlay updates
    "${GH_ACTIONS_DIR}/update-test-overlays.sh"
}

# ============================================================================
# Stage 3: Build Images
# ============================================================================

build_and_load_images() {
    if [[ "$SKIP_BUILD" == true ]]; then
        echo "--- Skipping image builds (--skip-build) ---"
        return
    fi

    echo "=== Building Images ==="

    mkdir -p "${DOCKER_IMAGES_PATH}"

    local targets
    targets=$(get_build_targets "$SUITE")

    for target in $targets; do
        case "$target" in
            core)
                echo "--- Building core KServe images ---"
                "${GH_ACTIONS_DIR}/build-images.sh"
                ;;
            llmisvc)
                echo "--- Building LLMISvc images ---"
                "${GH_ACTIONS_DIR}/build-images.sh" llmisvc
                ;;
            graph)
                echo "--- Building graph test images ---"
                "${GH_ACTIONS_DIR}/build-graph-tests-images.sh"
                ;;
            *)
                echo "--- Building runtime images: ${target} ---"
                "${GH_ACTIONS_DIR}/build-server-runtimes.sh" "${target}"
                ;;
        esac
    done

    echo "=== Loading Images into Minikube ==="

    local image_files
    image_files=$(find "${DOCKER_IMAGES_PATH}" -maxdepth 1 -type f 2>/dev/null || true)

    if [[ -z "$image_files" ]]; then
        echo "No images found in ${DOCKER_IMAGES_PATH}"
        return
    fi

    if needs_multinode "$SUITE"; then
        # For multi-node, load into each node via mount
        cp -f "${DOCKER_IMAGES_PATH}"/* /tmp-images/ 2>/dev/null || true
        for node in minikube minikube-m02 minikube-m03; do
            echo "Loading images into ${node}..."
            for file in /tmp-images/*; do
                [[ -f "$file" ]] || continue
                minikube ssh -n "$node" -- docker image load -i "$file" 2>/dev/null || true
            done
        done
        rm -f /tmp-images/*
    else
        # Single node - load directly
        for file in ${image_files}; do
            echo "Loading $(basename "$file")..."
            minikube image load "$file"
        done
    fi

    # Clean up build artifacts to save disk
    rm -rf "${DOCKER_IMAGES_PATH}"/*

    echo "Images loaded into minikube:"
    minikube ssh -- docker image ls --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | grep "${KO_DOCKER_REPO}" || true
}

# ============================================================================
# Stage 4: Install KServe
# ============================================================================

install_kserve() {
    if [[ "$SKIP_SETUP" == true ]]; then
        echo "--- Skipping KServe install (--skip-setup) ---"
        return
    fi

    local controller_check="kserve-controller-manager"
    if is_llmisvc_suite "$SUITE"; then
        controller_check="llmisvc-controller-manager"
    fi

    if [[ "$FORCE_INSTALL" != true ]] && kubectl get deployment "$controller_check" -n kserve &>/dev/null 2>&1; then
        echo "--- KServe already installed (${controller_check} exists), skipping ---"
        echo "    Use --force-install to reinstall"
        return
    fi

    echo "=== Installing KServe ==="

    # Setup uv and Python venv
    "${GH_ACTIONS_DIR}/setup-uv.sh"

    if is_llmisvc_suite "$SUITE"; then
        export ENABLE_LLMISVC="true"
    fi

    if [[ "$INSTALL_METHOD" == "helm" ]]; then
        export INSTALL_METHOD="helm"
        export SET_KSERVE_VERSION="${TAG}"
    fi

    "${GH_ACTIONS_DIR}/setup-kserve.sh" "${DEPLOYMENT_MODE}" "${NETWORK_LAYER}"

    kubectl get pods -n kserve
}

# ============================================================================
# Stage 5: Suite-Specific Setup
# ============================================================================

suite_specific_setup() {
    if [[ "$SKIP_SETUP" == true ]]; then
        return
    fi

    echo "=== Suite-Specific Setup for '${SUITE}' ==="

    case "$SUITE" in
        modelcache)
            echo "Creating localmodel job namespace..."
            kubectl create ns kserve-localmodel-jobs 2>/dev/null || true

            echo "Labeling worker nodes for modelcache..."
            kubectl label nodes -l '!node-role.kubernetes.io/control-plane' kserve/localmodel=worker --overwrite 2>/dev/null || true

            echo "Enabling nodeselector in knative..."
            kubectl patch configmaps -n knative-serving config-features \
                --patch '{"data": {"kubernetes.podspec-nodeselector": "enabled"}}' 2>/dev/null || true

            echo "Creating model root directory on worker nodes..."
            minikube ssh -n minikube-m02 -- sudo mkdir -p -m=777 /models 2>/dev/null || true
            minikube ssh -n minikube-m03 -- sudo mkdir -p -m=777 /models 2>/dev/null || true
            ;;
        kourier)
            # Kourier needs a special ingress host
            export KSERVE_INGRESS_HOST_PORT=$(kubectl get pod -n knative-serving -l "app=3scale-kourier-gateway" \
                --output=jsonpath="{.items[0].status.podIP}"):$(kubectl get pod -n knative-serving -l "app=3scale-kourier-gateway" \
                --output=jsonpath="{.items[0].spec.containers[0].ports[0].containerPort}")
            echo "Kourier ingress: ${KSERVE_INGRESS_HOST_PORT}"
            ;;
        *)
            echo "No suite-specific setup needed."
            ;;
    esac
}

# ============================================================================
# Stage 6: Run Tests
# ============================================================================

run_tests() {
    echo ""
    echo "=== Running E2E Tests ==="
    echo "Suite:       ${SUITE}"
    echo "Parallelism: ${PARALLELISM}"
    echo "Network:     ${NETWORK_LAYER}"
    echo ""

    "${GH_ACTIONS_DIR}/run-e2e-tests.sh" "${SUITE}" "${PARALLELISM}" "${NETWORK_LAYER}"
}

# ============================================================================
# Main Pipeline
# ============================================================================

echo "=== KServe E2E Pipeline ==="
echo ""

start_minikube
install_deps
build_and_load_images
install_kserve
suite_specific_setup
run_tests

echo ""
echo "=== Pipeline Complete ==="
