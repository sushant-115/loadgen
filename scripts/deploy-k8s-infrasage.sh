#!/usr/bin/env bash
set -euo pipefail

if command -v kubectl >/dev/null 2>&1; then
  KUBECTL_CMD="kubectl"
elif command -v k3s >/dev/null 2>&1; then
  KUBECTL_CMD="sudo k3s kubectl"
else
  echo "kubectl (or k3s) is required but not found in PATH"
  exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required but not found in PATH"
  exit 1
fi

if [[ -z "${INFRASAGE_API_KEY:-}" ]]; then
  echo "INFRASAGE_API_KEY is required"
  exit 1
fi

NAMESPACE="${NAMESPACE:-loadgen}"
IMAGE_NAME="${IMAGE_NAME:-loadgen:latest}"
BUILD_IMAGE="${BUILD_IMAGE:-true}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

cd "${ROOT_DIR}"

if [[ "${BUILD_IMAGE}" == "true" ]]; then
  echo "Building ${IMAGE_NAME} using Dockerfile.multi..."
  sudo docker build -f Dockerfile.multi -t "${IMAGE_NAME}" .

  echo "Importing ${IMAGE_NAME} into k3s containerd..."
  sudo docker save "${IMAGE_NAME}" | sudo k3s ctr images import -
fi

echo "Applying infrastructure manifests..."
${KUBECTL_CMD} apply -f k8s-deployment.yaml

echo "Upserting InfraSage credentials secret in namespace ${NAMESPACE}..."
${KUBECTL_CMD} -n "${NAMESPACE}" create secret generic infrasage-credentials \
  --from-literal=api-key="${INFRASAGE_API_KEY}" \
  --dry-run=client -o yaml | ${KUBECTL_CMD} apply -f -

echo "Applying application services..."
${KUBECTL_CMD} apply -f k8s-services.yaml

echo "Restarting application workloads for fresh image usage..."
${KUBECTL_CMD} -n "${NAMESPACE}" rollout restart deployment/gateway deployment/auth-service deployment/user-service deployment/order-service deployment/payment-service deployment/notification-worker deployment/traffic-generator deployment/mock-shopify || true

echo "Restarting collector to pick up secret/env changes..."
${KUBECTL_CMD} -n "${NAMESPACE}" rollout restart deployment/otel-collector

echo "Waiting for collector rollout..."
${KUBECTL_CMD} -n "${NAMESPACE}" rollout status deployment/otel-collector --timeout=180s

echo "Current pods:"
${KUBECTL_CMD} -n "${NAMESPACE}" get pods -o wide

echo "Recent collector logs:"
${KUBECTL_CMD} -n "${NAMESPACE}" logs deployment/otel-collector --tail=200

echo "Done."
