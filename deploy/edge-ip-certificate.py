#!/usr/bin/env python3
"""Renew the enrolled edge's public IP certificate without changing its identity."""
import argparse
import ipaddress
import json
import os
from pathlib import Path
import subprocess


def renew(args):
    os.umask(0o077)
    state = Path(args.state_dir).resolve()
    deployment = json.loads((state / "tunnel/deployment.json").read_text())
    address = ipaddress.ip_address(deployment["connector_advertise_host"])
    if not address.is_global:
        raise ValueError("the enrolled edge must advertise a public IP address")
    acme = state / "public-acme"
    acme.mkdir(mode=0o700, exist_ok=True)
    certificate = acme / "live/paperboat-edge-ip/fullchain.pem"
    private_key = certificate.with_name("privkey.pem")
    with (acme / "renew.log").open("w") as log:
        if certificate.exists() and subprocess.run(
            ["openssl", "x509", "-in", str(certificate), "-noout", "-checkend", str(args.renew_before)],
            stdout=log, stderr=subprocess.STDOUT, timeout=10,
        ).returncode == 0:
            return

        def run(step, command, timeout=30):
            result = subprocess.run(command, stdout=log, stderr=subprocess.STDOUT, timeout=timeout)
            if result.returncode:
                raise RuntimeError(step + " failed; inspect the protected certificate renewal log")

        run("stop edge for HTTP certificate validation", ["systemctl", "stop", args.service])
        primary = None
        try:
            run("issue public IP certificate", [
                "docker", "run", "--rm", "--network", "host", "--mount",
                "type=bind,source=" + str(acme) + ",target=/etc/letsencrypt",
                args.certbot_image, "certonly", "--standalone", "--non-interactive",
                "--agree-tos", "--email", args.email, "--preferred-profile", "shortlived",
                "--ip-address", str(address), "--cert-name", "paperboat-edge-ip", "--force-renewal",
            ], timeout=180)
            run("configure public HTTPS", [args.pbh, "configure-tls", "--state-dir", str(state),
                                           "--https-cert", str(certificate), "--https-key", str(private_key)], timeout=90)
        except Exception as error:
            primary = error
        finally:
            try:
                run("restore edge service", ["systemctl", "start", args.service])
            except Exception as error:
                if primary is None:
                    primary = error
        if primary is not None:
            raise primary


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--state-dir", default="/var/lib/paperboat-selfhost")
    parser.add_argument("--pbh", default="/usr/local/lib/paperboat-selfhost/pbh")
    parser.add_argument("--service", default="paperboat-selfhost.service")
    parser.add_argument("--certbot-image", default="certbot/certbot:v5.4.0@sha256:c23159d30afdd9c97960578aa4654f5901de6cae394958f894074dedd55e599d")
    parser.add_argument("--renew-before", type=int, default=172800)
    parser.add_argument("--email", default=os.environ.get("PAPERBOAT_CERTIFICATE_EMAIL"))
    args = parser.parse_args()
    if not args.email or not 86400 <= args.renew_before <= 259200:
        parser.error("configure the certificate contact email and a 1–3 day renewal window")
    try:
        renew(args)
    except Exception as error:
        print(str(error) if isinstance(error, RuntimeError) else "certificate configuration failed; inspect the protected state and renewal log")
        return 1
    print("Public edge certificate is current; enrolled identity preserved.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
