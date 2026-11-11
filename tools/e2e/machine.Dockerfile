# A machine of the core end-to-end (server/internal/app TestE2ECore): the device-smoke systemd image
# (tools/device-smoke/ubuntu24-systemd.Dockerfile) plus an OpenSSH server, which managed SSH (pb ssh) bridges to.
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq && apt-get install -y -qq systemd systemd-sysv dbus sudo python3 curl ca-certificates iproute2 iptables procps openssh-client openssh-server nftables libnss3-tools \
 && apt-get clean && rm -rf /var/lib/apt/lists/* && systemctl enable ssh
RUN (userdel -r ubuntu 2>/dev/null || true) && useradd -m -u 1000 -s /bin/bash pbt && echo "pbt ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/pbt && chmod 440 /etc/sudoers.d/pbt
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
