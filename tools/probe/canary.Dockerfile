# Canary machine (R-06): a small systemd container that runs pb as user pbt (uid 1000). tools/probe/canary-setup.sh.
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends systemd systemd-sysv dbus dbus-user-session \
      libpam-systemd sudo curl ca-certificates python3 iproute2 iptables nftables libnss3-tools procps \
      openssh-client openssh-server \
 && rm -rf /var/lib/apt/lists/* \
 && (userdel -r ubuntu 2>/dev/null || true) && useradd -m -s /bin/bash -u 1000 pbt \
 && echo 'pbt ALL=(ALL) NOPASSWD:ALL' >/etc/sudoers.d/pbt && systemctl mask getty.target console-getty.service
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
