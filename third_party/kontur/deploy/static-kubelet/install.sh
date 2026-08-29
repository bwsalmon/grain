#!/usr/bin/env bash
# Installs and starts a standalone kubelet on this node: containerd (CRI
# enabled), the CNI plugins containerd needs to give pods a network
# namespace, and kubelet itself, configured to run only static pods from
# /etc/kubernetes/manifests with no apiserver in the picture. See
# README.md for what this is for and how it fits with kontur's "run" and
# "netshim" modes.
#
# Tested on Debian 12 (bookworm) and Debian-derivatives. Must run as root.
set -euo pipefail

KUBELET_VERSION="${KUBELET_VERSION:-v1.31.0}"
STATIC_POD_PATH="${STATIC_POD_PATH:-/etc/kubernetes/manifests}"

if [ "$(id -u)" -ne 0 ]; then
  echo "install.sh must run as root" >&2
  exit 1
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64) arch=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

echo "==> installing containerd and CNI plugins"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends containerd containernetworking-plugins ca-certificates curl

echo "==> writing containerd config (CRI enabled, local registry mirror)"
install -d -m0755 /etc/containerd
install -m0644 "${script_dir}/containerd-config.toml" /etc/containerd/config.toml
systemctl enable containerd >/dev/null
systemctl restart containerd

echo "==> writing CNI node network config"
install -d -m0755 /etc/cni/net.d
install -m0644 "${script_dir}/cni/10-kontur.conflist" /etc/cni/net.d/10-kontur.conflist

echo "==> fetching kubelet ${KUBELET_VERSION} (${arch})"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
base="https://dl.k8s.io/release/${KUBELET_VERSION}/bin/linux/${arch}"
curl -fsSL -o "${tmp}/kubelet" "${base}/kubelet"
curl -fsSL -o "${tmp}/kubelet.sha256" "${base}/kubelet.sha256"
(cd "${tmp}" && echo "$(cat kubelet.sha256)  kubelet" | sha256sum -c -)
install -m0755 "${tmp}/kubelet" /usr/local/bin/kubelet

echo "==> writing kubelet config"
install -d -m0755 /etc/kubernetes
install -m0644 "${script_dir}/kubelet-config.yaml" /etc/kubernetes/kubelet-config.yaml
install -d -m0755 "${STATIC_POD_PATH}"
if [ "${STATIC_POD_PATH}" != "/etc/kubernetes/manifests" ]; then
  sed -i "s#^staticPodPath:.*#staticPodPath: ${STATIC_POD_PATH}#" /etc/kubernetes/kubelet-config.yaml
fi
install -d -m0755 /var/lib/kubelet

echo "==> installing kubelet.service"
install -m0644 "${script_dir}/kubelet.service" /etc/systemd/system/kubelet.service
systemctl daemon-reload
systemctl enable --now kubelet

cat <<EOF

kubelet is running in standalone mode (no apiserver, no node registration).
Drop static pod manifests into ${STATIC_POD_PATH} and kubelet will start
them directly, e.g.:

  cp ${script_dir}/manifests/kontur-static-pod.yaml ${STATIC_POD_PATH}/

Check status with 'systemctl status kubelet', 'journalctl -u kubelet -f',
or 'crictl ps' / 'crictl logs <container>' against
/run/containerd/containerd.sock (kubectl won't work: there's no apiserver
for it to talk to). See README.md for the local image registry these
manifests expect at localhost:5000.
EOF
