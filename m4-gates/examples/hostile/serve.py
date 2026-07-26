"""Serve the build-time probe report, and re-run the probes at RUN time.

Two phases matter and they are different threats:

  build time — the Dockerfile's RUN lines, which on the host path execute as
               root outside any microVM. That report is baked into the image.
  run time   — this app, which is an ordinary Krill guest and is covered by
               the F6 egress baseline rather than by builder isolation.

Serving both from one page means F5 and F6 can be read off a single URL.
"""

import http.server
import json
import os
import socket
import subprocess

REPORT = "/report/probes.txt"


def runtime_probes():
    """The same questions, asked from inside a running app guest."""
    out = {}

    def tcp(host, port, timeout=3):
        try:
            with socket.create_connection((host, port), timeout=timeout):
                return "OPEN"
        except Exception as e:  # noqa: BLE001 — the failure text is the finding
            return f"blocked ({type(e).__name__})"

    out["admin_api_127"] = tcp("127.0.0.1", 9091)
    out["admin_api_gateway"] = tcp(os.environ.get("KRILL_GW", "172.16.0.1"), 9091)
    out["ssh_gateway"] = tcp(os.environ.get("KRILL_GW", "172.16.0.1"), 22)
    out["other_guest"] = tcp("172.16.0.2", 8000)
    out["metadata"] = tcp("169.254.169.254", 80)
    out["smtp_587"] = tcp("example.com", 587)
    out["smtp_25"] = tcp("example.com", 25)
    out["https_arbitrary"] = tcp("example.com", 443)
    out["dns"] = tcp("1.1.1.1", 53)
    try:
        out["dns_resolve"] = socket.gethostbyname("example.com")
    except Exception as e:  # noqa: BLE001
        out["dns_resolve"] = f"blocked ({type(e).__name__})"
    return out


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):  # noqa: N802 — stdlib naming
        if self.path == "/runtime.json":
            body = json.dumps(runtime_probes(), indent=2).encode()
            ctype = "application/json"
        else:
            try:
                with open(REPORT, "rb") as f:
                    build = f.read()
            except OSError as e:
                build = f"no build report: {e}".encode()
            body = (
                b"=== BUILD-TIME PROBES (F5) ===\n" + build +
                b"\n=== RUN-TIME PROBES (F6) ===\n" +
                json.dumps(runtime_probes(), indent=2).encode() + b"\n"
            )
            ctype = "text/plain; charset=utf-8"
        self.send_response(200)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    # A best-effort look at whether anything the build wrote outside its own
    # tree survived into the image.
    for path in ("/host-root-pwned", "/srv/krill/PWNED"):
        if os.path.exists(path):
            print(f"PERSISTED: {path}", flush=True)
    subprocess.run(["true"], check=False)
    http.server.HTTPServer(("0.0.0.0", 8000), Handler).serve_forever()
