#!/usr/bin/env bash
# Best-effort diagnostics dump for Metal3 e2e runs into ${OUT_DIR}.
# Runs after possibly-broken setups (CI `if: always()`), so it must never fail.
# shellcheck source=common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"
set +e # common.sh enables errexit; a missing resource must not abort the rest.

OUT_DIR="${OUT_DIR:-artifacts}"
IRONIC_NS="${IRONIC_NS:-baremetal-operator-system}"
mkdir -p "${OUT_DIR}"

kubectl get pods -A >"${OUT_DIR}/pods.txt" 2>&1
kubectl get network-attachment-definitions -A -o yaml >"${OUT_DIR}/nads.yaml" 2>&1
kubectl get virtualmachinebmc,vm,vmi -A -o yaml >"${OUT_DIR}/kvbmc.yaml" 2>&1
kubectl get bmh -A -o yaml >"${OUT_DIR}/bmh.yaml" 2>&1
docker exec "${KIND_NODE}" ip addr >"${OUT_DIR}/node-ip.txt" 2>&1
docker exec "${KIND_NODE}" ip link show "${BR_NAME}" >"${OUT_DIR}/br-prov.txt" 2>&1

# Per-stage timestamps to locate provisioning stalls: dnsmasq shows
# DHCP rounds + TFTP events, httpd shows kernel/initramfs GETs,
# conductor shows deploy transitions and heartbeat waits.
IR=$(kubectl -n "${IRONIC_NS}" get pod -l ironic.metal3.io/app=ironic-service \
  -o jsonpath='{.items[0].metadata.name}')
if [[ -n "${IR}" ]]; then
  for c in dnsmasq httpd ironic; do
    kubectl -n "${IRONIC_NS}" logs "${IR}" -c "${c}" --timestamps >"${OUT_DIR}/ironic-${c}.log" 2>&1
  done
fi

echo "==> diagnostics written to ${OUT_DIR}/"
