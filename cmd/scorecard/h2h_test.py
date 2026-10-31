"""Offline checks for the head-to-head scorecard driver."""
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import unittest

import h2h


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        self.rfile.read(length)
        if self.path == "/Users/AuthenticateByName":
            body = {"AccessToken": "token", "User": {"Id": "user"}}
            self._json(200, body)
            return
        if self.path.startswith("/Items/") and self.path.endswith("/PlaybackInfo"):
            self._json(200, {"PlaySessionId": "session", "MediaSources": [{"Id": "source", "SupportsDirectPlay": True, "Size": 1000}]})
            return
        if self.path == "/Sessions/Playing/Stopped" or self.path == "/Sessions/Logout":
            self.send_response(204)
            self.end_headers()
            return
        self.send_response(404)
        self.end_headers()

    def do_GET(self):
        if self.path.startswith("/Videos/") and "static=true" in self.path:
            start = 0
            header = self.headers.get("Range") or ""
            if header.startswith("bytes="):
                start = int(header.split("=", 1)[1].split("-", 1)[0] or 0)
            payload = bytes([start % 251]) * 16
            self.send_response(206)
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            return
        if self.path.startswith("/Users/user/Items"):
            body = {"Items": [{"Id": "item", "Name": "Sintel", "Path": "/media/Sintel.mkv", "MediaSources": [{"Id": "source", "Path": "/media/Sintel.mkv", "Size": 1000}]}], "TotalRecordCount": 1}
            self._json(200, body)
            return
        self.send_response(404)
        self.end_headers()

    def _json(self, status, body):
        raw = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, *_):
        return


class H2HTests(unittest.TestCase):
    def test_nearest_rank_matches_scorecard(self):
        values = [float(n) for n in range(1, 21)]
        self.assertEqual(h2h.nearest_rank(values, 50), values[(20 * 50 + 99) // 100 - 1])
        self.assertEqual(h2h.nearest_rank(values, 95), values[(20 * 95 + 99) // 100 - 1])

    def test_catalogue_counts_are_compared_as_maps(self):
        self.assertNotEqual({"Movie": 6, "Episode": 40}, {"Movie": 4, "Episode": 40})
        self.assertEqual({"Movie": 4, "Episode": 40}, {"Movie": 4, "Episode": 40})

    def test_cardinality_blocks_a_latency_winner(self):
        self.assertEqual(h2h.latency_winner(10, 11, 0, 6, 4), "inconclusive_cardinality")
        self.assertEqual(h2h.latency_winner(10, 11, 0, 4, 4), "reference")
        self.assertEqual(h2h.latency_winner(12, 11, 0, 4, 4), "ether")
        self.assertEqual(h2h.latency_winner(10, 11, 1, 4, 4), "inconclusive")

    def test_direct_span_reads_bytes_and_stops(self):
        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.shutdown)
        self.addCleanup(server.server_close)
        client = h2h.Client("http://127.0.0.1:%d" % server.server_address[1], "test")
        client.login("viewer", "secret")
        row = h2h.playback_span(client, "item", "source", h2h.DIRECT_PROFILE, seek_size=400)
        self.assertEqual(row["delivery"], "direct")
        self.assertGreater(row["ms"], 0)
        client.logout()


if __name__ == "__main__":
    unittest.main()
