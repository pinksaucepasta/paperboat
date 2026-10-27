# The disposable machines of the final re-check (fresh_pair_lib.sh start_ct): Ubuntu 24.04 with systemd, a login-capable user
# pbt (uid 1000, passwordless sudo) and the tools the checks use inside a machine. Build: docker build -t laneh-ubuntu24-systemd -f ubuntu24-systemd.Dockerfile .
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update -qq && apt-get install -y -qq systemd systemd-sysv dbus sudo python3 curl ca-certificates iproute2 iptables procps openssh-client nftables libnss3-tools \
 && apt-get clean && rm -rf /var/lib/apt/lists/*
RUN (userdel -r ubuntu 2>/dev/null || true) && useradd -m -u 1000 -s /bin/bash pbt && echo "pbt ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/pbt && chmod 440 /etc/sudoers.d/pbt
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
