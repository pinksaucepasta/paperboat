# The app behind the proxy. "/" is happy only when the proxy tells it the visitor used https;
# /force-ssl behaves like Rails force_ssl and Django SECURE_SSL_REDIRECT; /sse, /range and /upload
# are the streaming, partial-content and Expect: 100-continue probes.
import http.server, json, sys, time

class H(http.server.BaseHTTPRequestHandler):
    def seen(self):
        h = self.headers
        return {"host": h.get("Host"), "xfp": h.get("X-Forwarded-Proto"), "xfh": h.get("X-Forwarded-Host"),
                "xff": h.get("X-Forwarded-For"), "forwarded": h.get("Forwarded"), "peer": self.client_address[0]}

    def reply(self, status, body, headers=()):
        self.send_response(status)
        for k, v in headers:
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        seen = self.seen()
        happy = seen["xfp"] == "https"
        if self.path == "/force-ssl" and not happy:
            return self.reply(301, b"", [("Location", "https://%s/force-ssl" % seen["host"])])
        if self.path == "/sse":
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.end_headers()
            for i in range(3):
                self.wfile.write(b"data: %d\n\n" % i)
                self.wfile.flush()
                time.sleep(0.7)
            return
        if self.path.startswith("/slow"):  # first byte after ?seconds=N (the 100 s first-byte probe)
            time.sleep(min(int(self.path.partition("seconds=")[2] or 1), 120))
            return self.reply(200, b"slow ok")
        if self.path == "/range":
            data = bytes(range(256)) * 4
            start, end = 0, len(data) - 1
            spec = self.headers.get("Range", "")
            if spec.startswith("bytes="):
                a, _, b = spec[6:].partition("-")
                start, end = int(a), int(b) if b else len(data) - 1
                return self.reply(206, data[start:end + 1], [("Content-Range", "bytes %d-%d/%d" % (start, end, len(data)))])
            return self.reply(200, data)
        self.reply(200 if happy else 400, json.dumps({"app_happy": happy, **seen}).encode())

    def do_POST(self):
        size = len(self.rfile.read(int(self.headers.get("Content-Length", 0))))
        self.reply(200, json.dumps({"received": size}).encode())

    def log_message(self, *a):
        pass

# a machine's daemon owns 127.100.0.x:9000 for local access to peers, so a tunnel check binds loopback only: app.py 127.0.0.1
http.server.ThreadingHTTPServer((sys.argv[1] if len(sys.argv) > 1 else "0.0.0.0", 9000), H).serve_forever()
