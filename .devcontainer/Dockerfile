FROM mcr.microsoft.com/devcontainers/base:ubuntu

# Versions - keep in sync with kserve-deps.env and .github/actions/minikube-setup/action.yml
ARG GO_VERSION=1.25.4
ARG PYTHON_VERSION=3.10
ARG KUBECTL_VERSION=v1.34.4
ARG MINIKUBE_VERSION=v1.38.1
ARG HELM_VERSION=v3.16.3
ARG KUSTOMIZE_VERSION=v5.5.0
ARG YQ_VERSION=v4.52.1
ARG UV_VERSION=0.7.8

# System packages
RUN apt-get update -qq \
    && apt-get install -y -qq --no-install-recommends \
        jq \
        software-properties-common \
    && rm -rf /var/lib/apt/lists/*

# Python 3.10
RUN add-apt-repository -y ppa:deadsnakes/ppa \
    && apt-get update -qq \
    && apt-get install -y -qq --no-install-recommends \
        python${PYTHON_VERSION} \
        python${PYTHON_VERSION}-venv \
        python${PYTHON_VERSION}-dev \
        python${PYTHON_VERSION}-distutils \
    && update-alternatives --install /usr/bin/python3 python3 /usr/bin/python${PYTHON_VERSION} 1 \
    && update-alternatives --set python3 /usr/bin/python${PYTHON_VERSION} \
    && rm -rf /var/lib/apt/lists/*

# Go
RUN curl -sL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" | tar -C /usr/local -xzf - \
    && ln -s /usr/local/go/bin/go /usr/local/bin/go \
    && ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt

# kubectl
RUN curl -sLO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" \
    && chmod +x kubectl \
    && mv kubectl /usr/local/bin/kubectl

# minikube
RUN curl -sLO "https://storage.googleapis.com/minikube/releases/${MINIKUBE_VERSION}/minikube-linux-amd64" \
    && chmod +x minikube-linux-amd64 \
    && mv minikube-linux-amd64 /usr/local/bin/minikube

# helm
RUN curl -sL "https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz" | tar -xzf - -C /tmp \
    && mv /tmp/linux-amd64/helm /usr/local/bin/helm \
    && rm -rf /tmp/linux-amd64

# kustomize
RUN curl -sL "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2F${KUSTOMIZE_VERSION}/kustomize_${KUSTOMIZE_VERSION}_linux_amd64.tar.gz" | tar -xzf - -C /tmp \
    && mv /tmp/kustomize /usr/local/bin/kustomize

# yq
RUN curl -sL "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/yq_linux_amd64" -o /usr/local/bin/yq \
    && chmod +x /usr/local/bin/yq

# uv
RUN curl -sL "https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-x86_64-unknown-linux-gnu.tar.gz" | tar -xzf - -C /tmp \
    && mv /tmp/uv-x86_64-unknown-linux-gnu/uv /usr/local/bin/uv \
    && rm -rf /tmp/uv-x86_64-unknown-linux-gnu

# Verify all tools
RUN go version \
    && python3 --version \
    && kubectl version --client 2>/dev/null || true \
    && minikube version --short \
    && helm version --short \
    && kustomize version \
    && yq --version \
    && uv --version \
    && jq --version
