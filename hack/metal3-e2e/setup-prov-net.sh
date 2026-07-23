#!/usr/bin/env bash
# Create node-local br-prov + Multus + NetworkAttachmentDefinition for PXE L2.
set -euo pipefail
# shellcheck source=common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

require_kind_node
install_cni_plugins

echo "==> installing Multus ${MULTUS_VERSION} (${MULTUS_MANIFEST})"
kubectl apply -f "${MULTUS_MANIFEST}"
kubectl -n kube-system rollout status daemonset/kube-multus-ds --timeout=180s

echo "==> ensuring ${BR_NAME} on ${KIND_NODE} (${BR_CIDR})"
docker exec "${KIND_NODE}" bash -c "
  set -euo pipefail
  if ! ip link show ${BR_NAME} >/dev/null 2>&1; then
    ip link add ${BR_NAME} type bridge
  fi
  ip addr replace ${BR_CIDR} dev ${BR_NAME}
  ip link set ${BR_NAME} up
  # Allow IPA/dnsmasq traffic across the bridge.
  iptables -C FORWARD -i ${BR_NAME} -j ACCEPT 2>/dev/null || iptables -I FORWARD -i ${BR_NAME} -j ACCEPT
  iptables -C FORWARD -o ${BR_NAME} -j ACCEPT 2>/dev/null || iptables -I FORWARD -o ${BR_NAME} -j ACCEPT
"

echo "==> applying NetworkAttachmentDefinition ${NAD_NS}/${NAD_NAME}"
envsubst '${NAD_NS} ${NAD_NAME} ${BR_NAME}' \
  < "${ROOT_DIR}/hack/metal3-e2e/fixtures/nad-provisioning.yaml" | kubectl apply -f -

echo "==> provisioning network ready (${BR_NAME} @ ${BR_CIDR}, NAD ${NAD_NS}/${NAD_NAME})"
