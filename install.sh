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
APT=(apt-get -o DPkg::Lock::Timeout=600 -o Acquire::http::Timeout=15 -o Acquire::Retries=1 -y -qq)

PKGS=(iproute2 iptables iputils-ping iperf3 curl ca-certificates conntrack python3 unzip tar kmod ethtool irqbalance wireguard-tools)

# Iranian servers often can't reach the default Ubuntu mirrors; fall back to Iranian mirrors for this
# install only (a temporary sources list, the server's apt config is left untouched)
apt_install() {
  "${APT[@]}" update >/dev/null 2>&1; "${APT[@]}" install "$@" >/dev/null 2>&1 && return 0
  echo "[!] default Ubuntu mirrors failed - retrying through an Iranian mirror (this install only)"
  local m list=/tmp/lighttun-apt.list o
  for m in http://mirror.arvancloud.ir/ubuntu http://ir.archive.ubuntu.com/ubuntu http://mirror.iranserver.com/ubuntu; do
    printf 'deb %s %s main universe\ndeb %s %s-updates main universe\n' "$m" "$VERSION_CODENAME" "$m" "$VERSION_CODENAME" > "$list"
    o=(-o Dir::Etc::sourcelist="$list" -o Dir::Etc::sourceparts=/nonexistent)
    if "${APT[@]}" "${o[@]}" update >/dev/null 2>&1 && "${APT[@]}" "${o[@]}" install "$@" >/dev/null 2>&1; then
      rm -f "$list"; echo "[+] packages installed from $m"; return 0
    fi
  done
  rm -f "$list"; return 1
}

echo "[*] installing packages"
apt_install "${PKGS[@]}" || { echo "[x] could not install packages: ${PKGS[*]}"; exit 1; }

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
