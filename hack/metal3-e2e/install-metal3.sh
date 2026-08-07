#!/usr/bin/env bash
# Install IrSO + Ironic CR + BMO, wired for Kind (br-prov + cluster DNS).
set -euo pipefail
# shellcheck source=common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

# Version pins live in the Makefile (exported). Fallbacks keep direct script use working.
IRSO_VERSION="${IRSO_VERSION:-v0.10.0}"
BMO_VERSION="${BMO_VERSION:-v0.13.2}"
IRONIC_VERSION="${IRONIC_VERSION:-37.0}"
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-v1.14.2}"
IRONIC_NS="${IRONIC_NS:-baremetal-operator-system}"
export IRSO_VERSION BMO_VERSION IRONIC_VERSION CERT_MANAGER_VERSION IRONIC_NS
IRSO_MANIFEST="${IRSO_MANIFEST:-https://github.com/metal3-io/ironic-standalone-operator/releases/download/${IRSO_VERSION}/install.yaml}"
BMO_MANIFEST="${BMO_MANIFEST:-https://github.com/metal3-io/baremetal-operator/releases/download/${BMO_VERSION}/baremetal-operator.yaml}"

require_kind_node

if ! kubectl get crd certificates.cert-manager.io >/dev/null 2>&1; then
  echo "==> installing cert-manager ${CERT_MANAGER_VERSION}"
  kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml"
  kubectl -n cert-manager wait --for=condition=Available --timeout=300s deployment/cert-manager-webhook
fi

echo "==> installing Ironic Standalone Operator ${IRSO_VERSION}"
kubectl apply -f "${IRSO_MANIFEST}"
kubectl -n ironic-standalone-operator-system wait --for=condition=Available \
  deployment/ironic-standalone-operator-controller-manager --timeout=180s

echo "==> ensuring namespace ${IRONIC_NS}"
kubectl create namespace "${IRONIC_NS}" --dry-run=client -o yaml | kubectl apply -f -

echo "==> applying Ironic CR version=${IRONIC_VERSION} (${BR_NAME} ${BR_IP}, DHCP ${DHCP_RANGE_BEGIN}-${DHCP_RANGE_END})"
# Limit substitution to known placeholders so stray $ in comments cannot break YAML.
envsubst '${IRONIC_NS} ${IRONIC_VERSION} ${BR_NAME} ${BR_IP} ${BR_SUBNET} ${DHCP_RANGE_BEGIN} ${DHCP_RANGE_END}' \
  < "${ROOT_DIR}/hack/metal3-e2e/fixtures/ironic.yaml" | kubectl apply -f -
kubectl -n "${IRONIC_NS}" wait --for=condition=Ready --timeout=600s ironic/ironic

SECRET=$(kubectl get ironic/ironic -n "${IRONIC_NS}" -o jsonpath='{.spec.apiCredentialsName}')
if [[ -z "${SECRET}" ]]; then
  echo "error: Ironic CR has empty spec.apiCredentialsName" >&2
  exit 1
fi
echo "==> Ironic API credentials secret: ${SECRET}"

# IrSO ClusterIP Service maps API→80, images→8080 (hostNetwork target ports 6385/6180).
# Prefer Service DNS over br-prov IP so BMO does not depend on a hardcoded address.
# Exported so envsubst on fixtures/bmo-configmap.yaml can see them.
IRONIC_ENDPOINT="http://ironic.${IRONIC_NS}.svc/v1/"
DEPLOY_KERNEL_URL="http://ironic.${IRONIC_NS}.svc:8080/images/ironic-python-agent.kernel"
DEPLOY_RAMDISK_URL="http://ironic.${IRONIC_NS}.svc:8080/images/ironic-python-agent.initramfs"
CACHEURL="http://ironic.${IRONIC_NS}.svc:8080/images"
export IRONIC_ENDPOINT DEPLOY_KERNEL_URL DEPLOY_RAMDISK_URL CACHEURL

echo "==> installing Bare Metal Operator ${BMO_VERSION}"
kubectl apply -f "${BMO_MANIFEST}"

echo "==> pointing BMO at IrSO Service (${IRONIC_ENDPOINT})"
envsubst '${IRONIC_NS} ${IRONIC_ENDPOINT} ${DEPLOY_KERNEL_URL} ${DEPLOY_RAMDISK_URL} ${CACHEURL}' \
  < "${ROOT_DIR}/hack/metal3-e2e/fixtures/bmo-configmap.yaml" | kubectl apply -f -

USER=$(kubectl get secret "${SECRET}" -n "${IRONIC_NS}" -o jsonpath='{.data.username}' | base64 -d)
PASS=$(kubectl get secret "${SECRET}" -n "${IRONIC_NS}" -o jsonpath='{.data.password}' | base64 -d)
kubectl create secret generic ironic-credentials -n "${IRONIC_NS}" \
  --from-literal=username="${USER}" --from-literal=password="${PASS}" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n "${IRONIC_NS}" patch deployment baremetal-operator-controller-manager \
  --patch-file="${ROOT_DIR}/hack/metal3-e2e/fixtures/bmo-auth-patch.yaml"
kubectl -n "${IRONIC_NS}" rollout status deployment/baremetal-operator-controller-manager --timeout=300s

echo "==> Metal3 stack ready (IrSO + Ironic + BMO)"
kubectl -n "${IRONIC_NS}" get ironic,deploy,pods
