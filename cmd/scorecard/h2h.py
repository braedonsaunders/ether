#!/usr/bin/env python3
"""Head-to-head browse and playback timings. Credentials arrive on stdin only."""
import http.client
import json
import os
import subprocess
import sys
import threading
import time
import urllib.parse
from concurrent.futures import ThreadPoolExecutor

DIRECT_PROFILE = {
    "MaxStreamingBitrate": 120000000,
    "DirectPlayProfiles": [{"Container": "mkv,mp4,mov", "Type": "Video", "VideoCodec": "h264,hevc,vp9,av1", "AudioCodec": "aac,mp3,ac3,eac3,opus,flac,vorbis"}],
    "TranscodingProfiles": [{"Container": "ts", "Type": "Video", "VideoCodec": "h264", "AudioCodec": "aac", "Protocol": "hls", "Context": "Streaming"}],
}
TRANSCODE_PROFILE = {
    "MaxStreamingBitrate": 2000000,
    "DirectPlayProfiles": [],
    "TranscodingProfiles": [{"Container": "ts", "Type": "Video", "VideoCodec": "h264", "AudioCodec": "aac", "Protocol": "hls", "Context": "Streaming"}],
}


def nearest_rank(values, percentile):
    if not values:
        return None
    ordered = sorted(values)
    return ordered[(len(ordered) * percentile + 99) // 100 - 1]


def latency_winner(reference, ether, errors, reference_total, ether_total):
    if errors or reference is None or ether is None:
        return "inconclusive"
    if reference_total != ether_total:
        return "inconclusive_cardinality"
    if ether < reference:
        return "ether"
    if ether > reference:
        return "reference"
    return "tie"


class Client:
    def __init__(self, base, device):
        parsed = urllib.parse.urlsplit(base)
        if parsed.scheme != "http" or not parsed.hostname or not parsed.port:
            raise ValueError("base URL must be http with a host and port")
        self.base = base.rstrip("/")
        self.host = parsed.hostname
        self.port = parsed.port
        self.prefix = parsed.path.rstrip("/")
        self.device = device
        self.token = ""
        self.user = ""

    def headers(self, body=False):
        auth = 'MediaBrowser Client="Scorecard", Device="h2h", DeviceId="%s", Version="1"' % self.device
        if self.token:
            auth += ', Token="%s"' % self.token
        headers = {"Authorization": auth, "Accept": "application/json"}
        if self.token:
            headers["X-Emby-Token"] = self.token
        if body:
            headers["Content-Type"] = "application/json"
        return headers

    def request(self, method, path, payload=None, extra=None, read_limit=8 << 20):
        target = urllib.parse.urlsplit(path)
        if target.scheme or target.netloc:
            if target.scheme != "http" or target.hostname != self.host or target.port != self.port:
                raise RuntimeError("response URL left the target host")
            full = target.path
            if target.query:
                full += "?" + target.query
        else:
            full = self.prefix + path
        body = None if payload is None else json.dumps(payload).encode()
        started = time.perf_counter()
        conn = http.client.HTTPConnection(self.host, self.port, timeout=60)
        try:
            headers = self.headers(body is not None)
            if extra:
                headers.update(extra)
            conn.request(method, full, body=body, headers=headers)
            response = conn.getresponse()
            first = time.perf_counter()
            data = response.read(read_limit + 1)
            if len(data) > read_limit:
                raise RuntimeError("response exceeded read limit")
            return response.status, data, (first - started) * 1000, (time.perf_counter() - started) * 1000
        finally:
            conn.close()

    def json(self, method, path, payload=None):
        status, data, _, _ = self.request(method, path, payload)
        if status < 200 or status >= 300:
            raise RuntimeError("HTTP %d" % status)
        return json.loads(data or b"null")

    def login(self, user, password):
        data = self.json("POST", "/Users/AuthenticateByName", {"Username": user, "Pw": password})
        self.token = data.get("AccessToken") or ""
        self.user = (data.get("User") or {}).get("Id") or ""
        if not self.token or not self.user:
            raise RuntimeError("login incomplete")

    def logout(self):
        if not self.token:
            return
        status, _, _, _ = self.request("POST", "/Sessions/Logout", {})
        self.token = ""
        if status < 200 or status >= 300:
            raise RuntimeError("logout failed")


def measure_get(client, path, samples, concurrency):
    durations, firsts = [], []
    errors = 0
    total = None
    lock = threading.Lock()

    def once():
        nonlocal errors, total
        try:
            status, data, first, full = client.request("GET", path)
            parsed = json.loads(data or b"null")
            ok = 200 <= status < 300
        except (RuntimeError, json.JSONDecodeError, OSError, http.client.HTTPException):
            ok, first, full, parsed = False, None, None, None
        with lock:
            if not ok:
                errors += 1
                return
            durations.append(full)
            firsts.append(first)
            if isinstance(parsed, dict) and isinstance(parsed.get("TotalRecordCount"), int):
                total = parsed["TotalRecordCount"]

    with ThreadPoolExecutor(max_workers=concurrency) as pool:
        list(pool.map(lambda _: once(), range(samples)))
    return {"samples": samples, "concurrency": concurrency, "errors": errors, "total": total,
            "p50_ms": nearest_rank(durations, 50), "p95_ms": nearest_rank(durations, 95),
            "ttfb_p50_ms": nearest_rank(firsts, 50), "ttfb_p95_ms": nearest_rank(firsts, 95)}


def stop(client, session, item):
    if not session:
        return
    try:
        client.request("POST", "/Sessions/Playing/Stopped", {"PlaySessionId": session, "ItemId": item})
    except (RuntimeError, OSError, http.client.HTTPException):
        pass


def playback_span(client, item, source, profile, start_ticks=0, seek_size=None):
    """One play-to-first-media-byte span. Returns milliseconds and a delivery label."""
    body = {"IsPlayback": True, "EnableDirectPlay": True, "EnableDirectStream": True, "EnableTranscoding": True,
            "DeviceProfile": profile, "MediaSourceId": source}
    if start_ticks:
        body["StartTimeTicks"] = start_ticks
    started = time.perf_counter()
    info = client.json("POST", "/Items/%s/PlaybackInfo" % urllib.parse.quote(item), body)
    sources = info.get("MediaSources") or []
    chosen = next((row for row in sources if row.get("Id") == source), sources[0] if sources else None)
    if not chosen:
        raise RuntimeError("playback info had no source")
    session = info.get("PlaySessionId") or ""
    try:
        transcode = chosen.get("TranscodingUrl") or ""
        if transcode:
            _read_hls(client, transcode)
            delivery = "hls"
        else:
            query = {"static": "true", "PlaySessionId": session, "MediaSourceId": source}
            if start_ticks:
                query["StartTimeTicks"] = str(start_ticks)
            path = "/Videos/%s/stream?%s" % (urllib.parse.quote(item), urllib.parse.urlencode(query))
            extra = {"Range": "bytes=%d-%d" % (seek_size, seek_size + 65535)} if seek_size else {"Range": "bytes=0-65535"}
            status, data, _, _ = client.request("GET", path, extra=extra, read_limit=65536)
            if status not in (200, 206) or not data:
                raise RuntimeError("direct read failed")
            delivery = "direct"
        return {"ms": (time.perf_counter() - started) * 1000, "delivery": delivery, "session": session,
                "supports_direct_play": bool(chosen.get("SupportsDirectPlay")), "size": chosen.get("Size")}
    finally:
        stop(client, session, item)


def _read_hls(client, path):
    current = path
    for _ in range(4):
        status, data, _, _ = client.request("GET", current, read_limit=4 << 20)
        if status != 200 or not data:
            raise RuntimeError("hls response failed")
        if not data.startswith(b"#EXTM3U"):
            return
        nxt = ""
        for line in data.decode("utf-8", "replace").splitlines():
            line = line.strip()
            if line and not line.startswith("#"):
                nxt = line
                break
        if not nxt:
            raise RuntimeError("empty hls playlist")
        current = urllib.parse.urljoin(current if "://" in current else client.base + current, nxt)


def repeat_playback(client, item, source, profile, repeats, **kwargs):
    samples, errors, delivery = [], 0, ""
    for _ in range(repeats):
        try:
            row = playback_span(client, item, source, profile, **kwargs)
            samples.append(row["ms"])
            delivery = row["delivery"]
        except (RuntimeError, json.JSONDecodeError, OSError, http.client.HTTPException, UnicodeError):
            errors += 1
    return {"errors": errors, "delivery": delivery, "p50_ms": nearest_rank(samples, 50), "p95_ms": nearest_rank(samples, 95), "samples": len(samples)}


def find_movie(client, suffix):
    data = client.json("GET", "/Users/%s/Items?Recursive=true&IncludeItemTypes=Movie&Fields=Path,MediaSources,ProviderIds&Limit=100" % client.user)
    for item in data.get("Items") or []:
        paths = [item.get("Path") or ""]
        paths += [source.get("Path") or "" for source in item.get("MediaSources") or []]
        if any(path.endswith(suffix) for path in paths):
            source = next((row for row in item.get("MediaSources") or [] if (row.get("Path") or "").endswith(suffix)), None)
            return item.get("Id"), (source or {}).get("Id") or item.get("Id"), (source or {}).get("Size")
    raise RuntimeError("playback title missing")


def read_offer(client, handle, container="", internal=""):
    quoted = urllib.parse.quote(handle)
    try:
        status, raw, _, _ = client.request("GET", "/__scorecard/offer?handle=" + quoted)
        if status == 200:
            parsed = json.loads(raw or b"null")
            if isinstance(parsed, dict):
                return parsed
    except (RuntimeError, json.JSONDecodeError, OSError, http.client.HTTPException):
        pass
    if not container or not internal.startswith("http://127.0.0.1:"):
        return None
    proc = subprocess.run(["docker", "exec", container, "wget", "-qO-", internal + "/__scorecard/offer?handle=" + quoted], capture_output=True, text=True, timeout=20)
    if proc.returncode != 0 or not proc.stdout.startswith("{"):
        return None
    try:
        parsed = json.loads(proc.stdout)
    except json.JSONDecodeError:
        return None
    return parsed if isinstance(parsed, dict) else None


def ether_source(client, name, wanted_sha, container="", internal=""):
    data = client.json("GET", "/Users/%s/Items?Recursive=true&IncludeItemTypes=Movie&Limit=100" % client.user)
    for item in data.get("Items") or []:
        if item.get("Name") != name or not item.get("Id"):
            continue
        detail = client.json("GET", "/Items/" + urllib.parse.quote(item["Id"]))
        for source in detail.get("MediaSources") or []:
            handle = source.get("Id") or ""
            if len(handle) != 36:
                continue
            offer = read_offer(client, handle, container, internal)
            if isinstance(offer, dict) and offer.get("expected_sha256", "").lower() == wanted_sha.lower():
                return item["Id"], handle
    raise RuntimeError("ether source hash was not offered")


def browse_pair(reference, ether, name, ref_path, eth_path, samples, concurrency):
    ref = measure_get(reference, ref_path, samples, concurrency)
    eth = measure_get(ether, eth_path, samples, concurrency)
    errors = ref["errors"] + eth["errors"]
    return {"name": name, "concurrency": concurrency, "reference": ref, "ether": eth,
            "p50_winner": latency_winner(ref["p50_ms"], eth["p50_ms"], errors, ref["total"], eth["total"]),
            "p95_winner": latency_winner(ref["p95_ms"], eth["p95_ms"], errors, ref["total"], eth["total"]),
            "route": ref_path.split("?")[0].replace(reference.user, "{user}")}


def playback_pair(reference, ether, ref_item, ref_source, eth_item, eth_source, repeats, label, profile, **kwargs):
    ref = repeat_playback(reference, ref_item, ref_source, profile, repeats, **kwargs)
    eth = repeat_playback(ether, eth_item, eth_source, profile, repeats, **kwargs)
    errors = ref["errors"] + eth["errors"]
    same = ref["delivery"] and ref["delivery"] == eth["delivery"]
    return {"name": label, "reference": ref, "ether": eth,
            "p50_winner": latency_winner(ref["p50_ms"], eth["p50_ms"], errors or not same, 1, 1),
            "p95_winner": latency_winner(ref["p95_ms"], eth["p95_ms"], errors or not same, 1, 1)}


def catalogue(client):
    counts = {}
    for kind in ("Movie", "Series", "Season", "Episode"):
        data = client.json("GET", "/Users/%s/Items?Recursive=true&IncludeItemTypes=%s&Limit=1" % (client.user, kind))
        counts[kind] = data.get("TotalRecordCount")
    return counts


def limits(ether_container=""):
    text = {}
    try:
        text["loadavg"] = open("/proc/loadavg", encoding="utf8").read().strip()
        mem = {}
        for line in open("/proc/meminfo", encoding="utf8"):
            if line.startswith(("MemTotal:", "MemAvailable:")):
                mem[line.split()[0][:-1]] = int(line.split()[1])
        text["meminfo_kb"] = mem
    except OSError:
        text["host"] = "unavailable"
    rows = {}
    names = ["prism-jellyfin", "prism-emby"]
    if ether_container:
        names.append(ether_container)
    for name in names:
        try:
            raw = subprocess.check_output(["docker", "inspect", "-f", "{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}}", name], text=True, timeout=15).split()
            rows[name] = {"nano_cpus": int(raw[0]), "memory_bytes": int(raw[1])}
        except (OSError, subprocess.SubprocessError, ValueError, IndexError):
            rows[name] = None
    text["containers"] = rows
    if not ether_container:
        text["ether"] = "host process, no cpu or memory cap"
    reference_caps = [rows.get("prism-jellyfin"), rows.get("prism-emby")]
    ether_cap = rows.get(ether_container) if ether_container else {"nano_cpus": 0, "memory_bytes": 0}
    text["matched_unlimited"] = bool(ether_cap) and all(row and row["nano_cpus"] == 0 and row["memory_bytes"] == 0 for row in reference_caps + [ether_cap])
    return text


def run(config):
    samples = int(config.get("samples") or 21)
    repeats = int(config.get("playback_repeats") or 5)
    provisional = bool(config.get("provisional", True))
    report = {"build": os.environ.get("SCORECARD_BUILD", ""), "provisional": provisional,
              "provisional_reason": config.get("provisional_reason", ""),
              "target": config.get("target") or "",
              "limits": limits(config.get("ether_container") or ""), "protocols": {}, "losses": []}
    for spec in config["protocols"]:
        reference = Client(spec["reference"], "h2h-" + spec["name"] + "-ref")
        ether = Client(spec["ether"], "h2h-" + spec["name"] + "-eth")
        protocol = {"browse": [], "playback": [], "errors": []}
        try:
            reference.login(spec["reference_user"], spec["reference_password"])
            ether.login(spec["ether_user"], spec["ether_password"])
            protocol["catalogue"] = {"reference": catalogue(reference), "ether": catalogue(ether)}
            ref_counts, eth_counts = protocol["catalogue"]["reference"], protocol["catalogue"]["ether"]
            protocol["catalogue_matched"] = ref_counts == eth_counts
            suffix = spec["playback_suffix"]
            sha = spec["playback_sha256"]
            bound = None
            try:
                ref_item, ref_source, ref_size = find_movie(reference, suffix)
                eth_item, eth_source = ether_source(ether, spec["playback_name"], sha, spec.get("ether_container") or "", spec.get("ether_internal") or "")
                bound = (ref_item, ref_source, ref_size, eth_item, eth_source)
            except RuntimeError as exc:
                protocol["errors"].append(str(exc) if "token" not in str(exc).lower() else "playback bind failed")
            routes = [
                ("home_views", "/Users/%s/Views" % reference.user, "/Users/%s/Views" % ether.user),
                ("library_movies", "/Users/%s/Items?Recursive=true&IncludeItemTypes=Movie&SortBy=SortName&SortOrder=Ascending&StartIndex=0&Limit=20" % reference.user,
                 "/Users/%s/Items?Recursive=true&IncludeItemTypes=Movie&SortBy=SortName&SortOrder=Ascending&StartIndex=0&Limit=20" % ether.user),
                ("library_episodes", "/Users/%s/Items?Recursive=true&IncludeItemTypes=Episode&SortBy=SortName&SortOrder=Ascending&StartIndex=0&Limit=50" % reference.user,
                 "/Users/%s/Items?Recursive=true&IncludeItemTypes=Episode&SortBy=SortName&SortOrder=Ascending&StartIndex=0&Limit=50" % ether.user),
                ("search", "/Search/Hints?searchTerm=%s&Limit=10" % urllib.parse.quote(spec["playback_name"]),
                 "/Search/Hints?searchTerm=%s&Limit=10" % urllib.parse.quote(spec["playback_name"])),
                ("next_up", "/Shows/NextUp?UserId=%s&Limit=20" % reference.user, "/Shows/NextUp?UserId=%s&Limit=20" % ether.user),
                ("resume", "/Users/%s/Items/Resume?Limit=20" % reference.user, "/Users/%s/Items/Resume?Limit=20" % ether.user),
            ]
            if bound:
                routes.insert(3, ("item_detail", "/Items/%s" % bound[0], "/Items/%s" % bound[3]))
            for concurrency in config.get("concurrency") or [1, 10]:
                for name, ref_path, eth_path in routes:
                    row = browse_pair(reference, ether, name, ref_path, eth_path, samples, concurrency)
                    protocol["browse"].append(row)
                    if row["p95_winner"] == "reference" and not provisional and protocol["catalogue_matched"]:
                        report["losses"].append({"protocol": spec["name"], "metric": name, "concurrency": concurrency, "kind": "browse_p95"})
            if bound:
                ref_item, ref_source, ref_size, eth_item, eth_source = bound
                direct = playback_pair(reference, ether, ref_item, ref_source, eth_item, eth_source, repeats, "direct_ttff", DIRECT_PROFILE)
                seek_at = int(ref_size or 0) // 2
                seek = playback_pair(reference, ether, ref_item, ref_source, eth_item, eth_source, repeats, "seek", DIRECT_PROFILE, seek_size=seek_at) if seek_at else {"name": "seek", "p95_winner": "inconclusive"}
                for client, item in ((reference, ref_item), (ether, eth_item)):
                    try:
                        client.json("POST", "/Sessions/Playing/Progress", {"ItemId": item, "PositionTicks": 300000000, "IsPaused": False})
                    except (RuntimeError, json.JSONDecodeError, OSError, http.client.HTTPException):
                        protocol["errors"].append("progress")
                resume = playback_pair(reference, ether, ref_item, ref_source, eth_item, eth_source, repeats, "resume", DIRECT_PROFILE, start_ticks=300000000)
                transcode = playback_pair(reference, ether, ref_item, ref_source, eth_item, eth_source, min(3, repeats), "transcode_ttff", TRANSCODE_PROFILE)
                protocol["playback"] = [direct, seek, resume, transcode]
            protocol["source_sha256"] = sha
            for row in protocol["playback"]:
                if row.get("p95_winner") == "reference" and not provisional and protocol.get("catalogue_matched"):
                    report["losses"].append({"protocol": spec["name"], "metric": row["name"], "kind": "playback_p95"})
        except (RuntimeError, json.JSONDecodeError, OSError, http.client.HTTPException, KeyError, ValueError) as exc:
            protocol["errors"].append(type(exc).__name__)
        finally:
            for client in (reference, ether):
                try:
                    client.logout()
                except (RuntimeError, OSError, http.client.HTTPException):
                    protocol["errors"].append("logout")
        report["protocols"][spec["name"]] = protocol
    return report


def _height(name):
    lower = name.lower()
    if "2160" in lower or "4k" in lower:
        return 2160
    if "1080" in lower:
        return 1080
    if "720" in lower:
        return 720
    return 0


def export_fixture(config):
    """Write a reference item listing and a digest-checked manifest. No credentials in either file."""
    client = Client(config["base"], "h2h-export")
    client.login(config["user"], config["password"])
    try:
        listing = client.json("GET", "/Users/%s/Items?Recursive=true&Fields=ProviderIds,Path,Overview,People,Studios,Genres,Taglines,ProductionLocations,MediaSources,UserData&IncludeItemTypes=Movie,Series,Season,Episode&Limit=1000" % client.user)
    finally:
        client.logout()
    items = listing.get("Items") or []
    if listing.get("TotalRecordCount", len(items)) != len(items):
        raise RuntimeError("item page was truncated")
    host_root = config["media_host"].rstrip("/")
    container = config["container"]
    series = {}
    for item in items:
        if item.get("Type") == "Series":
            series[item.get("Id")] = int((item.get("ProviderIds") or {}).get("Tmdb") or 0)
    entries = []
    for item in items:
        kind = item.get("Type")
        path = item.get("Path") or ""
        if kind not in ("Movie", "Episode") or not path.startswith("/media/"):
            continue
        rel = path[len("/media/"):]
        if rel.startswith("../") or rel == ".." or rel.startswith("/"):
            raise RuntimeError("media path escapes the mount")
        host_path = host_root + "/" + rel
        tmdb = int((item.get("ProviderIds") or {}).get("Tmdb") or 0) if kind == "Movie" else series.get(item.get("SeriesId"), 0)
        if tmdb <= 0:
            continue
        reference_hash = subprocess.check_output(["docker", "exec", container, "sha256sum", path], text=True, timeout=600).split()[0]
        digest = __import__("hashlib").sha256()
        with open(host_path, "rb") as stream:
            for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                digest.update(chunk)
        host_hash = digest.hexdigest()
        if host_hash != reference_hash:
            raise RuntimeError("reference digest mismatch")
        probe = subprocess.check_output(["docker", "exec", container, "ffprobe", "-v", "error", "-show_streams", "-of", "json", path], text=True, timeout=120)
        streams = json.loads(probe).get("streams") or []
        video = next((row for row in streams if row.get("codec_type") == "video"), {})
        audio = next((row for row in streams if row.get("codec_type") == "audio"), {})
        item["Path"] = host_path
        for source in item.get("MediaSources") or []:
            if source.get("Path") == path:
                source["Path"] = host_path
        entries.append({"tmdb": tmdb, "season": item.get("ParentIndexNumber") or 0 if kind == "Episode" else 0,
                        "episode": item.get("IndexNumber") or 0 if kind == "Episode" else 0, "path": host_path,
                        "sha256": reference_hash, "info": {"height": video.get("height") or _height(path),
                        "videoCodec": video.get("codec_name") or "", "audioCodec": audio.get("codec_name") or "",
                        "container": host_path.rsplit(".", 1)[-1].lower(), "source": "webdl"}})
    if not entries:
        raise RuntimeError("no playable items")
    for path in (config["items"], config["manifest"]):
        flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
        fd = os.open(path, flags, 0o600)
        with os.fdopen(fd, "w", encoding="utf8") as output:
            json.dump({"Items": items} if path == config["items"] else entries, output)
            output.write("\n")
    return {"items": len(items), "files": len(entries)}


def main():
    if len(sys.argv) > 1 and sys.argv[1] == "self-check":
        values = [float(x) for x in sys.argv[2:]]
        print(nearest_rank(values, 50), nearest_rank(values, 95))
        return
    config = json.load(sys.stdin)
    if config.get("command") == "export":
        json.dump(export_fixture(config), sys.stdout)
        sys.stdout.write("\n")
        return
    json.dump(run(config), sys.stdout, indent=2)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
