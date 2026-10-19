# A one-node Proxmox VE for the end-to-end tests, on the kernel of the machine running the container:
# Proxmox VE's packages on Debian 13 without Proxmox VE's kernel, systemd as PID 1, and VMs on the
# host's /dev/kvm. test/e2e/ci/up.sh builds and starts it; it is never published.
FROM debian:trixie-slim@sha256:a29215f6a35e51e22adffa17f89e9d2ef06214e64a2bad10d765c46aea49f11f

ARG KEY_URL=https://enterprise.proxmox.com/debian/proxmox-archive-keyring-trixie.gpg
ARG KEY_SHA256=136673be77aba35dcce385b28737689ad64fd785a797e57897589aed08db6e45
ENV DEBIAN_FRONTEND=noninteractive container=docker

# The container runs on the host's kernel, so nothing may pull Proxmox VE's.
COPY no-kernel.pref /etc/apt/preferences.d/no-kernel.pref

SHELL ["/bin/bash", "-o", "errexit", "-o", "pipefail", "-c"]

# The tests are of the current Proxmox VE, and its repository keeps no older versions to pin to; the
# base image is pinned by digest.
# hadolint ignore=DL3008
RUN apt-get update -qq \
    && apt-get install -y -qq --no-install-recommends ca-certificates curl \
    && curl -fsSL --proto '=https' -o /usr/share/keyrings/proxmox-archive-keyring.gpg "$KEY_URL" \
    && echo "$KEY_SHA256  /usr/share/keyrings/proxmox-archive-keyring.gpg" | sha256sum -c --quiet - \
    && printf '%s\n' 'Types: deb' 'URIs: http://download.proxmox.com/debian/pve' 'Suites: trixie' \
        'Components: pve-no-subscription' 'Signed-By: /usr/share/keyrings/proxmox-archive-keyring.gpg' \
        > /etc/apt/sources.list.d/pve-install-repo.sources \
    && echo 'postfix postfix/main_mailer_type select Local only' | debconf-set-selections \
    && echo 'postfix postfix/mailname string pve.e2e.internal' | debconf-set-selections \
    # The slim image leaves out everything under /usr/share/doc, where pve-manager keeps the appliance
    # index its install script reads and the keys pveam checks container templates with. The file
    # sorts after the image's own, so its rule comes last and wins.
    && echo 'path-include /usr/share/doc/pve-manager/*' > /etc/dpkg/dpkg.cfg.d/pve-manager \
    && apt-get update -qq \
    # pve-container for the appliance's LXC; openssh-client because Proxmox VE makes each
    # container's SSH host keys with ssh-keygen; e2fsprogs, which every full Debian has and
    # pve-container leaves undeclared, formats a container's disk on directory storage; ifupdown2
    # brings up the VMs' bridge; openssl for the appliance's installer and jq for its test, which run
    # on the node.
    && apt-get install -y -qq --no-install-recommends systemd systemd-sysv dbus kmod procps iproute2 \
        ifupdown2 iptables pve-manager qemu-server pve-container pve-cluster postfix socat openssh-client \
        e2fsprogs openssl jq \
    # Installing pve-manager adds the subscription-only repository, which answers 401 without a
    # subscription and fails every later apt-get update.
    && rm -f /etc/apt/sources.list.d/pve-enterprise.sources \
    && apt-get clean && rm -rf /var/lib/apt/lists/* /etc/machine-id /var/lib/dbus/machine-id

# systemd counts a container as one, and lxcfs's unit does not start inside one; containers then
# fail to start ("lxcfs not running?" from its mount hook).
COPY lxcfs.conf /etc/systemd/system/lxcfs.service.d/container.conf
COPY interfaces /etc/network/interfaces
# No watchdog to arm, the clock is the host's, and no getty or first-boot prompt has a terminal; the
# machine ID is made at each start. nftables.service would flush the rules the container runtime
# keeps in the container for its DNS, so the NAT is added beside them through iptables-nft, Debian's
# default; kernels built without the legacy tables have nothing else.
RUN systemctl mask watchdog-mux.service nftables.service systemd-timesyncd.service getty.target \
        console-getty.service systemd-firstboot.service systemd-networkd-wait-online.service \
    && systemctl enable lxcfs.service \
    && rm -f /etc/network/interfaces.new

STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
