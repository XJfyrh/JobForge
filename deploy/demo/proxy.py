"""Loopback-published demo ingress with one fixed upstream and no credentials."""

from __future__ import annotations

import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

HOP = {
    "connection",
    "transfer-encoding",
    "content-length",
    "proxy-authorization",
    "proxy-authenticate",
    "keep-alive",
    "te",
    "trailer",
    "upgrade",
}


class Proxy(BaseHTTPRequestHandler):
    """Forward one HTTP exchange; do not retry or expose an arbitrary proxy URL."""

    def do_GET(self) -> None:
        """Forward a read without adding an execution identity."""
        self.forward()

    def do_POST(self) -> None:
        """Preserve the caller's original mutation and operation key."""
        self.forward()

    def forward(self) -> None:
        """Use a fixed upstream, bounded bodies and one non-retrying exchange."""
        target = urlsplit(self.path)
        if (
            target.scheme
            or target.netloc
            or not self.path.startswith(("/ui", "/v2/", "/demo.json"))
        ):
            self.send_error(404)
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            self.send_error(400)
            return
        if not 0 <= length <= 131072 or self.headers.get("Transfer-Encoding"):
            self.send_error(413)
            return
        self.connection.settimeout(15)
        upstream = http.client.HTTPConnection("demo", 8093, timeout=15)
        try:
            body = self.rfile.read(length)
            if len(body) != length:
                self.send_error(400)
                return
            headers = {k: v for k, v in self.headers.items() if k.lower() not in HOP}
            upstream.request(self.command, self.path, body=body, headers=headers)
            response = upstream.getresponse()
            data = response.read(4194305)
            if len(data) > 4194304:
                self.send_error(502)
                return
            self.send_response(response.status)
            for key, value in response.getheaders():
                if key.lower() not in HOP:
                    self.send_header(key, value)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        except (OSError, http.client.HTTPException):
            self.send_error(502)
        finally:
            upstream.close()

    def log_message(self, format: str, *args: object) -> None:
        """Avoid recording request paths or caller credentials."""


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 8080), Proxy).serve_forever()
