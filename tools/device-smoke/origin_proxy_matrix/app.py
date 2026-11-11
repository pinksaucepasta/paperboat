# The app behind the proxy: happy only when the proxy tells it the visitor used https.
import http.server, json
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        seen = {"host": self.headers.get("Host"), "xfp": self.headers.get("X-Forwarded-Proto"), "xfh": self.headers.get("X-Forwarded-Host")}
        body = json.dumps({"app_happy": seen["xfp"] == "https", **seen}).encode()
        self.send_response(200 if seen["xfp"] == "https" else 400)
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass
http.server.ThreadingHTTPServer(("0.0.0.0", 9000), H).serve_forever()
