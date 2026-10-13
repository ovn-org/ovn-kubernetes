#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0


# Install Prometheus on infra nodes in kind cluster
# This script installs the kube-prometheus-stack helm chart on infra nodes

set -euo pipefail

# Returns the full directory name of the script
DIR="$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"

# Check if kubectl is available
command_exists() {
    command -v "$@" > /dev/null 2>&1
}

if ! command_exists kubectl; then
    echo "Error: kubectl is not installed or not in PATH"
    exit 1
fi

if ! command_exists helm; then
    echo "Error: helm is not installed or not in PATH"
    exit 1
fi

# Set default values
PROMETHEUS_NAMESPACE=${PROMETHEUS_NAMESPACE:-monitoring}
PROMETHEUS_RELEASE_NAME=${PROMETHEUS_RELEASE_NAME:-kube-prometheus-stack}
LOG_FILE=${LOG_FILE:-prometheus-install.log}

echo "Installing Prometheus on nodes with prometheus-node=true label..."
echo "Namespace: ${PROMETHEUS_NAMESPACE}"
echo "Release name: ${PROMETHEUS_RELEASE_NAME}"
echo "Log file: ${LOG_FILE}"

# Create log file and redirect all output
exec > >(tee -a "${LOG_FILE}")
exec 2>&1

# Wait for API server to be fully ready
echo "Waiting for Kubernetes API server to be ready..."
kubectl wait --for=condition=Ready nodes --all --timeout=300s || true
sleep 5

# Create namespace if it doesn't exist
kubectl create namespace "${PROMETHEUS_NAMESPACE}" --dry-run=client -o yaml | kubectl apply --validate=false -f -

# Add prometheus-community helm repository
echo "Adding prometheus-community helm repository..."
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update

# Scrape ovn-kubernetes itself. Out of the box the stack only collects platform
# metrics, so without this every ovnkube_, ovn_ and ovs_ query returns nothing.
# The secret has to exist before Prometheus starts, hence before helm.
echo "Creating ovn-kubernetes scrape config secret..."
kubectl create secret generic ovnk-scrape-config \
    --namespace "${PROMETHEUS_NAMESPACE}" \
    --from-file=ovnk-scrape.yaml="${DIR}/prometheus-ovnk-scrape.yaml" \
    --dry-run=client -o yaml | kubectl apply -f -

OVNK_SCRAPE_ARGS=(
    --set prometheus.prometheusSpec.additionalScrapeConfigsSecret.enabled=true
    --set prometheus.prometheusSpec.additionalScrapeConfigsSecret.name=ovnk-scrape-config
    --set prometheus.prometheusSpec.additionalScrapeConfigsSecret.key=ovnk-scrape.yaml
)

# Check if there are nodes with prometheus-node=true label
PROMETHEUS_NODES=$(kubectl get nodes -l prometheus-node=true --no-headers 2>/dev/null | wc -l)

if [ "${PROMETHEUS_NODES}" -gt 0 ]; then
    echo "Found ${PROMETHEUS_NODES} prometheus node(s), installing Prometheus on nodes with prometheus-node=true label..."
    # Install on prometheus nodes using values file with node selectors
    helm upgrade --install "${PROMETHEUS_RELEASE_NAME}" prometheus-community/kube-prometheus-stack \
        --namespace "${PROMETHEUS_NAMESPACE}" \
        --values "${DIR}/prometheus-values.yaml" \
        "${OVNK_SCRAPE_ARGS[@]}" \
        --wait --timeout=10m
else
    echo "No nodes with prometheus-node=true label found, installing Prometheus on any available nodes..."
    # Install without node selector if no prometheus nodes are available
    helm upgrade --install "${PROMETHEUS_RELEASE_NAME}" prometheus-community/kube-prometheus-stack \
        --namespace "${PROMETHEUS_NAMESPACE}" \
        --set prometheusOperator.tls.enabled=false \
        --set prometheusOperator.admissionWebhooks.enabled=false \
        --set prometheusOperator.admissionWebhooks.patch.enabled=false \
        "${OVNK_SCRAPE_ARGS[@]}" \
        --wait --timeout=10m
fi

echo "Waiting for Prometheus pods to be ready..."
kubectl wait --for=condition=ready pod -l "release=${PROMETHEUS_RELEASE_NAME}" -n "${PROMETHEUS_NAMESPACE}" --timeout=300s


# Mark nodes running prometheus pods as unschedulable
echo "Marking nodes running Prometheus as unschedulable..."
PROM_NODES=$(kubectl get pods -n "${PROMETHEUS_NAMESPACE}" -l "release=${PROMETHEUS_RELEASE_NAME}" -o jsonpath='{.items[*].spec.nodeName}' | tr ' ' '\n' | sort -u)
if [ -n "${PROM_NODES}" ]; then
    for node in ${PROM_NODES}; do
        echo "Marking node ${node} as unschedulable..."
        kubectl cordon "${node}"
    done
    echo "Marked $(echo "${PROM_NODES}" | wc -w) node(s) running Prometheus as unschedulable"
else
    echo "No nodes found running Prometheus pods"
fi

echo "Prometheus installation completed successfully!"
echo "Access Prometheus at: kubectl port-forward -n ${PROMETHEUS_NAMESPACE} svc/${PROMETHEUS_RELEASE_NAME}-prometheus 9090:9090"
echo "Access Grafana at: kubectl port-forward -n ${PROMETHEUS_NAMESPACE} svc/${PROMETHEUS_RELEASE_NAME}-grafana 3000:80"
echo "Default Grafana credentials: admin / prom-operator"
echo "Installation logs saved to: ${LOG_FILE}"
