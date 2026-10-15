"""Serve a local OpenAI-compatible response for the v0.4 manual walkthrough."""

from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        if self.path != "/v1/chat/completions":
            self.send_error(404)
            return
        if self.headers.get("Authorization") != "Bearer sk-test-openrouter-manual":
            self.send_error(401)
            return
        length = int(self.headers.get("Content-Length", "0"))
        request = json.loads(self.rfile.read(length))
        response = {
            "id": "chatcmpl-manual-fixture",
            "object": "chat.completion",
            "created": 1,
            "model": request.get("model", "manual-fixture"),
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": "fixture answer"},
                "finish_reason": "stop",
            }],
            "usage": {"prompt_tokens": 2, "completion_tokens": 2, "total_tokens": 4},
        }
        body = json.dumps(response).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, _format, *_args):
        pass


if __name__ == "__main__":
    print("local paid-model fixture listening on 127.0.0.1:8090")
    ThreadingHTTPServer(("127.0.0.1", 8090), Handler).serve_forever()
