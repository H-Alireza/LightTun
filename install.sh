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

export DEBIAN_FRONTEND=noninteractive
# fresh servers often run unattended-upgrades on first boot; wait for it instead of failing
lock_busy() { fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/lib/apt/lists/lock >/dev/null 2>&1; }
if command -v fuser >/dev/null && lock_busy; then
  echo "[*] another apt process is running (usually automatic updates) - waiting for it, up to 15 min"
  for _ in $(seq 180); do lock_busy || break; sleep 5; done
fi
APT=(apt-get -o DPkg::Lock::Timeout=600 -y -qq)

echo "[*] installing packages"
"${APT[@]}" update
"${APT[@]}" install iproute2 iptables iputils-ping iperf3 curl ca-certificates conntrack python3 \
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
