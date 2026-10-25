#!/usr/bin/env python3
"""Mock OpenAI-compatible upstream for local router testing.

Usage: mock_model_server.py <port> <model_id>
Serves:
  GET  /v1/models             -> {"data":[{"id": MODEL_ID}]}
  POST /v1/chat/completions   -> echoes back the "model" field it received,
                                  and 404s if it doesn't match MODEL_ID
                                  (mimics real LM Studio "model_not_found" behavior)
"""
import json
import sys
from http.server import HTTPServer, BaseHTTPRequestHandler
from socketserver import ThreadingMixIn

PORT = int(sys.argv[1])
MODEL_ID = sys.argv[2]


class ThreadingHTTPServer(ThreadingMixIn, HTTPServer):
    daemon_threads = True


class Handler(BaseHTTPRequestHandler):
    def _json(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path.startswith("/v1/models"):
            self._json(200, {"data": [{"id": MODEL_ID}]})
        else:
            self._json(404, {"error": "not found"})

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(length) if length else b""
        try:
            payload = json.loads(raw.decode() or "{}")
        except Exception:
            payload = {}
        requested_model = payload.get("model", "")
        if requested_model != MODEL_ID:
            self._json(404, {
                "error": {
                    "code": "model_not_found",
                    "message": f"Model '{requested_model}' was not found. Available models include: '{MODEL_ID}'"
                }
            })
            return
        self._json(200, {
            "model": MODEL_ID,
            "choices": [{"message": {"content": f"ok-from-{MODEL_ID}"}}]
        })

    def log_message(self, fmt, *args):
        print(f"[{MODEL_ID}:{PORT}] " + (fmt % args))


if __name__ == "__main__":
    print(f"Mock model server MODEL_ID={MODEL_ID} on 0.0.0.0:{PORT}", flush=True)
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
