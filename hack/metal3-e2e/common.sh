#!/usr/bin/env bash
# Shared helpers for Metal3 e2e provisioning-network setup.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CLUSTER_NAME="${CLUSTER_NAME:-kvbmc-metal3-e2e}"
KIND_NODE="${KIND_NODE:-${CLUSTER_NAME}-control-plane}"
BR_NAME="${BR_NAME:-br-prov}"
# Single source of truth for the node-local provisioning L2.
BR_CIDR="${BR_CIDR:-172.22.0.1/24}"
BR_IP="${BR_IP:-${BR_CIDR%%/*}}"
BR_SUBNET="${BR_SUBNET:-${BR_IP%.*}.0/24}"
DHCP_RANGE_BEGIN="${DHCP_RANGE_BEGIN:-${BR_IP%.*}.100}"
DHCP_RANGE_END="${DHCP_RANGE_END:-${BR_IP%.*}.200}"
# envsubst only sees exported variables (heredoc bash expansion did not need this).
export CLUSTER_NAME KIND_NODE BR_NAME BR_CIDR BR_IP BR_SUBNET DHCP_RANGE_BEGIN DHCP_RANGE_END

# Version pins live in the Makefile (exported). Fallbacks keep direct script use working.
MULTUS_VERSION="${MULTUS_VERSION:-v4.2.2}"
MULTUS_MANIFEST="${MULTUS_MANIFEST:-https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/${MULTUS_VERSION}/deployments/multus-daemonset-thick.yml}"
NAD_NS="${NAD_NS:-default}"
NAD_NAME="${NAD_NAME:-provisioning}"
export MULTUS_VERSION MULTUS_MANIFEST NAD_NS NAD_NAME

CNI_PLUGINS_VERSION="${CNI_PLUGINS_VERSION:-v1.6.2}"
export CNI_PLUGINS_VERSION

kind_node_exists() {
  docker inspect "${KIND_NODE}" >/dev/null 2>&1
}

require_kind_node() {
  if ! kind_node_exists; then
    echo "error: kind node container ${KIND_NODE} not found; create the cluster first (make metal3-e2e-setup)" >&2
    exit 1
  fi
}

# Kind node images ship only a subset of CNI binaries; Multus bridge NAD needs "bridge".
install_cni_plugins() {
  local arch tmp need=()
  arch="$(docker exec "${KIND_NODE}" uname -m)"
  case "${arch}" in
    x86_64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
  esac
  for plugin in bridge; do
    if ! docker exec "${KIND_NODE}" test -x "/opt/cni/bin/${plugin}"; then
      need+=("${plugin}")
    fi
  done
  if [[ ${#need[@]} -eq 0 ]]; then
    echo "==> CNI plugins already present on ${KIND_NODE}"
    return 0
  fi
  echo "==> installing CNI plugins (${need[*]}) ${CNI_PLUGINS_VERSION} on ${KIND_NODE}"
  tmp="$(mktemp -d)"
  curl -fsSL -o "${tmp}/cni-plugins.tgz" \
    "https://github.com/containernetworking/plugins/releases/download/${CNI_PLUGINS_VERSION}/cni-plugins-linux-${arch}-${CNI_PLUGINS_VERSION}.tgz"
  tar -xzf "${tmp}/cni-plugins.tgz" -C "${tmp}"
  for plugin in "${need[@]}"; do
    docker cp "${tmp}/${plugin}" "${KIND_NODE}:/opt/cni/bin/${plugin}"
    docker exec "${KIND_NODE}" chmod +x "/opt/cni/bin/${plugin}"
  done
  rm -rf "${tmp}"
}
