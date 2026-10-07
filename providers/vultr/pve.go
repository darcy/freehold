package vultr

import "fmt"

// PVEInstallScript turns the fresh Debian instance into the PVE host the
// Proxmox provider expects: the apt route on the instance's own Debian
// release (codename read live, no hardcode). Encodes the spike's findings —
// the pmxcfs /etc/hosts requirement, the http repo + enterprise-held release
// key, the pve-firewall persistence trap, the node's lxc/qemu-server dirs,
// and a `local` dir storage with rootdir content. LXC-only: no VM will ever
// boot here (cloud instances have no nested virt) — the appliance needs none.
//
// No apostrophes (the script may ride a single-quoted sh -c). Idempotent:
// an already-PVE host short-circuits (a re-adopt re-ensures cheaply).
func PVEInstallScript() string {
	return fmt.Sprintf(`set -e
if command -v pct >/dev/null 2>&1 && command -v pvesm >/dev/null 2>&1 && [ -f /etc/pve/storage.cfg ]; then
  echo pve-install-ok
  exit 0
fi
HN=$(hostname -s)
IP=$(ip -4 -o addr show scope global | awk "{print \$4}" | head -1 | cut -d/ -f1)
if grep -qE "^127\.0\.1\.1" /etc/hosts; then
  sed -i "s/^127\.0\.1\.1.*/$IP $HN/" /etc/hosts
elif ! grep -qE "[[:space:]]$HN([[:space:]]|$)" /etc/hosts; then
  echo "$IP $HN" >> /etc/hosts
fi
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq curl gnupg >/dev/null
CODENAME=$(. /etc/os-release && echo $VERSION_CODENAME)
# The apt route needs DEBIAN (the release key + repo are Debian's) — a
# wrong-image instance (an os_id drift, e.g. Ubuntu 22.04 behind id 1743)
# fails here with the reason instead of a 404 deep in the key fetch.
case "$CODENAME" in
  bullseye|bookworm|trixie) ;;
  *) echo "the host is $CODENAME, not a Debian release - destroy this instance and re-run (the provider picks the newest Debian image from the catalog)" >&2; exit 1 ;;
esac
curl -fsSL https://enterprise.proxmox.com/debian/proxmox-release-$CODENAME.gpg -o /etc/apt/trusted.gpg.d/proxmox-release-$CODENAME.gpg
echo "deb http://download.proxmox.com/debian/pve $CODENAME pve-no-subscription" > /etc/apt/sources.list.d/pve-no-subscription.list
# The index predates the PVE repo — refresh or proxmox-ve is unlocatable.
apt-get update -qq
apt-get -o Dpkg::Options::=--force-confold full-upgrade -y -qq
apt-get -o Dpkg::Options::=--force-confold install -y -qq proxmox-ve postfix open-iscsi || apt-get -o Dpkg::Options::=--force-confold install -y -qq proxmox-ve postfix
# pve-firewall ships 14 drop rules that PERSIST past a stop and silently
# strangle guest egress (spike) — never start it; the freehold ruleset rules.
systemctl disable --now pve-firewall >/dev/null 2>&1 || true
mkdir -p /etc/pve/nodes/$HN/lxc /etc/pve/nodes/$HN/qemu-server /var/lib/vz/template/cache
if ! pvesm status 2>/dev/null | grep -q "^local "; then
cat > /etc/pve/storage.cfg <<CFG
dir: local
	path /var/lib/vz
	content iso,vztmpl,images,rootdir
CFG
fi
command -v pct >/dev/null
command -v pvesm >/dev/null
echo pve-install-ok
`)
}
