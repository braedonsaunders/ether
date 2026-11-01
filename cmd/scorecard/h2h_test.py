"""Offline checks for the head-to-head scorecard driver."""
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import unittest

import h2h


class Handler(BaseHTTPRequestHandler):
    media_range = None

    def _bare(self):
        path = self.path
        prefix = ""
        if path == "/emby" or path.startswith("/emby/"):
            prefix = "/emby"
            path = path[len(prefix):] or "/"
        return prefix, path

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        self.rfile.read(length)
        _, path = self._bare()
        if path == "/Users/AuthenticateByName":
            body = {"AccessToken": "token", "User": {"Id": "user"}}
            self._json(200, body)
            return
        if path.startswith("/Items/") and path.endswith("/PlaybackInfo"):
            self._json(200, {"PlaySessionId": "session", "MediaSources": [{"Id": "source", "SupportsDirectPlay": True, "Size": 1000}]})
            return
        if path == "/Sessions/Playing/Stopped" or path == "/Sessions/Logout":
            self.send_response(204)
            self.end_headers()
            return
        self.send_response(404)
        self.end_headers()

    def do_GET(self):
        prefix, path = self._bare()
        if path.startswith("/Videos/") and "static=true" in path:
            self.send_response(307)
            self.send_header("Location", prefix + "/sessions/media/clip")
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        if path.startswith("/sessions/media/clip"):
            Handler.media_range = self.headers.get("Range")
            start = 0
            header = Handler.media_range or ""
            if header.startswith("bytes="):
                start = int(header.split("=", 1)[1].split("-", 1)[0] or 0)
            payload = bytes([start % 251]) * 16
            self.send_response(206)
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            return
        if path.startswith("/Users/user/Items"):
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
        self.assertEqual(Handler.media_range, "bytes=400-65935")
        self.assertGreater(row["ms"], 0)
        client.logout()
        prefixed = h2h.Client("http://127.0.0.1:%d/emby" % server.server_address[1], "test-emby")
        prefixed.login("viewer", "secret")
        Handler.media_range = None
        row = h2h.playback_span(prefixed, "item", "source", h2h.DIRECT_PROFILE)
        self.assertEqual(row["delivery"], "direct")
        self.assertEqual(Handler.media_range, "bytes=0-65535")
        status, data, _, _ = prefixed.request("GET", "/emby/sessions/media/clip", extra={"Range": "bytes=0-15"})
        self.assertEqual(status, 206)
        self.assertEqual(len(data), 16)
        prefixed.logout()


if __name__ == "__main__":
    unittest.main()
