#!/bin/sh
# Sourced by the fresh-host qualification scripts. A "fresh host" is a throwaway privileged
# systemd container with the packages a stock server image has (python3, nftables) plus
# a sudo-capable "tester" user with a login session (linger), so system and per-user services work.
#   needs: docker (command, may be "sudo docker"), base image, optional platform, container name, work dir
#   provides: fresh_host_up, fresh_host_down, inside (run a shell command as root), inside_user
inside() { $docker exec "$name" sh -c "$1"; }
inside_user() { $docker exec -u tester -e XDG_RUNTIME_DIR=/run/user/1001 -w /home/tester "$name" bash -lc "$1"; }

fresh_host_up() { # [extra docker run arguments...]
 cat > "$work/Dockerfile" <<DOCKERFILE
FROM $base
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends systemd systemd-sysv dbus libpam-systemd ca-certificates curl sudo iproute2 iptables nftables kmod procps python3 \\
 && rm -rf /var/lib/apt/lists/* \\
 && systemctl mask getty.target console-getty.service systemd-udevd.service systemd-udevd-kernel.socket systemd-udevd-control.socket systemd-modules-load.service sys-kernel-config.mount sys-kernel-debug.mount sys-kernel-tracing.mount \\
 && useradd -m -u 1001 -s /bin/bash tester && echo 'tester ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/tester
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
DOCKERFILE
 $docker build -q ${platform:+--platform "$platform"} -t "$name" "$work" >/dev/null
 $docker volume create "$name" >/dev/null
 $docker run -d --name "$name" ${platform:+--platform "$platform"} --privileged --cgroupns=host --tmpfs /run --tmpfs /run/lock \
  -v /sys/fs/cgroup:/sys/fs/cgroup:rw -v "$name:/var/lib/docker" "$@" "$name" >/dev/null
 for _ in $(seq 1 30); do inside 'systemctl is-system-running 2>/dev/null | grep -qE "running|degraded"' && break; sleep 1; done
 inside 'loginctl enable-linger tester; for _ in $(seq 1 20); do test -d /run/user/1001 && exit 0; sleep 1; done; exit 1'
}

fresh_host_down() { $docker rm -f "$name" >/dev/null 2>&1 || true; $docker volume rm "$name" >/dev/null 2>&1 || true; $docker rmi "$name" >/dev/null 2>&1 || true; }
