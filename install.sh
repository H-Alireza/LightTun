#!/usr/bin/env bash
# LightTun installer for Ubuntu.
#   From a copy of the repo:   sudo bash install.sh [--fec] [--rathole]
#   Straight from GitHub:      curl -fsSL https://raw.githubusercontent.com/H-Alireza/LightTun/main/install.sh | sudo bash -s -- --fec
set -euo pipefail

REPO="H-Alireza/LightTun"
BRANCH="${BRANCH:-main}"
BIN=/usr/local/bin/lighttun

[[ $EUID -eq 0 ]] || { echo "run as root"; exit 1; }
. /etc/os-release
[[ ${ID:-} == ubuntu ]] || echo "[!] LightTun is built for Ubuntu; continuing on ${PRETTY_NAME:-unknown}"

want_fec=no want_rathole=no
for a in "$@"; do
  case $a in
    --fec) want_fec=yes ;;
    --rathole) want_rathole=yes ;;
    *) echo "unknown option $a"; exit 1 ;;
  esac
done

echo "[*] installing packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq iproute2 iptables iputils-ping iperf3 curl ca-certificates conntrack python3 \
  unzip tar kmod ethtool irqbalance >/dev/null

src=""
if [[ -n ${BASH_SOURCE[0]:-} && -f ${BASH_SOURCE[0]} ]]; then
  src="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lighttun"
fi
if [[ -n $src && -f $src ]]; then
  install -m 755 "$src" "$BIN"
else
  echo "[*] downloading lighttun from $REPO"
  auth=()
  [[ -n ${GH_TOKEN:-} ]] && auth=(-H "Authorization: token $GH_TOKEN")
  curl -fsSL "${auth[@]}" "https://raw.githubusercontent.com/$REPO/$BRANCH/lighttun" -o "$BIN" \
    || { echo "[x] download failed - check that this server can reach raw.githubusercontent.com"; exit 1; }
  chmod 755 "$BIN"
fi
sed -i 's/\r$//' "$BIN"

"$BIN" setup
[[ $want_fec == yes ]] && "$BIN" install fec
[[ $want_rathole == yes ]] && "$BIN" install rathole
echo
echo "[+] done. Run: lighttun"
