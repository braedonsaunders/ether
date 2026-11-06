#!/usr/bin/env python3
"""One reviewed lab HLS attempt; defaults to offline preflight, never retries Start.

Private JSON: session, requestID, startURL, stopURL, headers, ffmpeg, ffprobe,
ffmpegSHA256, ffprobeSHA256, samplers [{name, argv}]. Sampler argv must include
{readyFile} and {outputDir}; write the fresh ready file only after observing is
ready. No credentials/URLs belong in printed output. --run needs separate lab
authorization; preflight/dry parsing performs no HTTP or subprocess invocation.
"""
import argparse
from collections import Counter
import hashlib
import json
import math
import os
from pathlib import Path
import re
import resource
import signal
import subprocess
import sys
import time
from urllib.error import HTTPError
from urllib.parse import parse_qs, urljoin, urlsplit
from urllib.request import build_opener, HTTPRedirectHandler, ProxyHandler, Request


class Failure(Exception):
    pass


def require(condition, code):
    if not condition:
        raise Failure(code)


def origin(raw):
    u = urlsplit(raw)
    require(u.scheme == 'http' and u.hostname in ('127.0.0.1', 'localhost')
            and not u.username and not u.password and not u.fragment, 'lab-origin')
    return u.scheme, u.hostname, u.port or 80


def authored(base, raw):
    # Reject paths whose interpretation would be changed by URL normalization.
    require(raw and raw == raw.strip() and not any(ord(c) < 33 for c in raw), 'uri-space')
    u = urlsplit(raw)
    require(not u.fragment and not u.username and not u.password
            and not any(x in ('.', '..') for x in u.path.split('/'))
            and '%' not in u.path and not u.path.startswith('//'), 'uri-path')
    result = urljoin(base, raw)
    require(origin(result) == origin(base), 'uri-origin')
    return result  # Never decode/re-encode the authored query or add credentials.


def master_variant(base, data):
    lines = data.decode('utf-8').splitlines()
    require(lines and lines[0] == '#EXTM3U', 'master-format')
    require(sum(x.startswith('#EXT-X-STREAM-INF:') for x in lines) == 1, 'master-variant-count')
    uris = [x for x in lines if x and not x.startswith('#')]
    require(len(uris) == 1, 'master-uri-count')
    return authored(base, uris[0])


def segments(base, data):
    lines = data.decode('utf-8').splitlines()
    require(lines and lines[0] == '#EXTM3U', 'variant-format')
    sequence, duration, out = 0, None, []
    for line in lines[1:]:
        require(not line.startswith(('#EXT-X-KEY:', '#EXT-X-MAP:', '#EXT-X-STREAM-INF:',
                                     '#EXT-X-DISCONTINUITY')), 'unsupported-hls')
        if line.startswith('#EXT-X-MEDIA-SEQUENCE:'):
            sequence = int(line.split(':', 1)[1])
        elif line.startswith('#EXTINF:'):
            require(duration is None, 'missing-segment-uri')
            duration = float(line.split(':', 1)[1].split(',')[0])
            require(math.isfinite(duration) and 0 < duration <= 10, 'segment-duration')
        elif line and not line.startswith('#'):
            require(duration is not None, 'unexpected-variant-uri')
            out.append((sequence, duration, authored(base, line)))
            sequence, duration = sequence + 1, None
    require(duration is None and sequence >= 0, 'incomplete-variant')
    return out


def validate(c):
    require(os.name == 'posix', 'posix-required')
    require(re.fullmatch(r'[A-Za-z0-9_-]{1,96}', c['session']) is not None, 'session')
    require(re.fullmatch(r'[A-Za-z0-9_-]{1,96}', c['requestID']) is not None, 'request-id')
    require(origin(c['startURL']) == origin(c['stopURL']), 'control-origin')
    require(urlsplit(c['startURL']).path == '/video/:/transcode/universal/start.m3u8'
            and urlsplit(c['stopURL']).path == '/video/:/transcode/universal/stop', 'control-route')
    for key in ('startURL', 'stopURL'):
        require(parse_qs(urlsplit(c[key]).query).get('session') == [c['session']], 'control-session')
    h = c['headers']
    require(isinstance(h, dict) and all(isinstance(k, str) and isinstance(v, str)
            and '\r' not in k+v and '\n' not in k+v for k, v in h.items()), 'headers')
    require(h.get('Host') == 'ether035-roles.test' and h.get('X-Plex-Token', '').startswith('ether-cast-')
            and h.get('X-Plex-Session-Identifier') == c['session']
            and h.get('X-Ether-Lab-Request-ID') == c['requestID']
            and h.get('X-Plex-Client-Identifier'), 'lab-identity')
    for name in ('ffmpeg', 'ffprobe'):
        tool = Path(c[name])
        require(tool.is_absolute() and tool.is_file() and os.access(tool, os.X_OK), 'tool-path')
        with tool.open('rb') as f:
            require(hashlib.file_digest(f, 'sha256').hexdigest() == c[name+'SHA256'], 'tool-hash')
    samplers = c.get('samplers', [])
    require(len(samplers) == 1, 'samplers')
    for s in samplers:
        argv = s['argv']
        require(re.fullmatch('[a-z0-9-]{1,40}', s['name']) is not None
                and isinstance(argv, list) and all(isinstance(x, str) for x in argv)
                and len(argv) >= 2 and Path(argv[0]).resolve() == Path(sys.executable).resolve()
                and Path(argv[1]).is_absolute() and Path(argv[1]).is_file()
                and any('{readyFile}' in x for x in argv)
                and any('{outputDir}' in x for x in argv), 'sampler-config')
        require(hashlib.sha256(Path(argv[1]).read_bytes()).hexdigest() == s['scriptSHA256'], 'sampler-hash')
        require(bool(s.get('inputs')), 'sampler-inputs')
        for entry in s['inputs']:
            f = Path(entry['path'])
            require(f.is_absolute() and f.is_file()
                    and hashlib.sha256(f.read_bytes()).hexdigest() == entry['sha256'], 'sampler-input-hash')
    require(5 <= c.get('durationSeconds', 30) <= 30, 'duration-bound')
    require(0.05 <= c.get('maximumFirstGETDelaySeconds', 1) <= 1, 'first-get-bound')
    require(1 <= c.get('requestTimeoutSeconds', 5) <= 10, 'request-bound')
    require(1 <= c.get('minimumSegments', 5) <= 8, 'segment-count')


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def child_limits():
    # Only our explicit observer/demux/decoder children; no daemon/container.
    resource.setrlimit(resource.RLIMIT_AS, (512*1024*1024, 512*1024*1024))
    resource.setrlimit(resource.RLIMIT_CPU, (10, 10))
    resource.setrlimit(resource.RLIMIT_FSIZE, (8*1024*1024, 8*1024*1024))


def observer_limits():
    # kubectl is Go and reserves a large virtual arena; RLIMIT_AS would prevent
    # it from starting. Observation output and CPU remain bounded, with no build.
    resource.setrlimit(resource.RLIMIT_CPU, (10, 10))
    resource.setrlimit(resource.RLIMIT_FSIZE, (8*1024*1024, 8*1024*1024))


class Attempt:
    def __init__(self, config, output):
        self.c, self.out = config, output
        self.opener = build_opener(ProxyHandler({}), NoRedirect())
        self.children, self.handles, self.events = [], [], []
        self.total, self.first_get, self.start_response = 0, None, None
        self.deadline = None
        self.events_path = output/'timeline.private.jsonl'
        self.combined = output/'collected.ts'

    def event(self, kind, **fields):
        e = dict(kind=kind, wallTime=time.time(), monotonicNS=time.monotonic_ns(), **fields)
        self.events.append(e)
        with self.events_path.open('a') as f:
            f.write(json.dumps(e)+'\n')

    def get(self, url, name, limit, first=False, stopping=False):
        require(origin(url) == origin(self.c['startURL']), 'request-origin')
        if self.deadline and not stopping:
            require(time.monotonic() < self.deadline, 'group-deadline')
        if not stopping:
            require(all(p.poll() is None for p in self.children), 'sampler-exited')
        self.event('request', resource=name)
        status, body = None, b''
        timeout = self.c.get('requestTimeoutSeconds', 5)
        if self.deadline and not stopping:
            timeout = max(0.01, min(timeout, self.deadline-time.monotonic()))
        request = Request(url, headers=self.c['headers'])
        gap = None
        try:
            if first:
                self.first_get = time.monotonic_ns()
                gap = (self.first_get-self.start_response)/1e9
                require(gap <= self.c.get('maximumFirstGETDelaySeconds', 1), 'delayed-first-get')
            # No file writes, observer setup or external work before open.
            with self.opener.open(request, timeout=timeout) as response:
                status = response.status
                body = response.read(limit+1)
                if name == 'master':
                    self.start_response = time.monotonic_ns()
                require(len(body) <= limit, 'response-bound')
                require(status == 200, 'http-status')
        except HTTPError as e:
            try:
                status, body = e.code, e.read(limit)
            finally:
                e.close()
            raise Failure('http-status') from None
        finally:
            (self.out/(name+'.private')).write_bytes(body)
            self.event('response', resource=name, status=status, bytes=len(body))
            if first:
                self.event('first-media-get', requestMonotonicNS=self.first_get,
                           startResponseMonotonicNS=self.start_response,
                           startResponseToFirstGETSeconds=gap)
        return body

    def prepare_samplers(self):
        for s in self.c['samplers']:
            ready = self.out/(s['name']+'.ready')
            argv = [x.replace('{readyFile}', str(ready)).replace('{outputDir}', str(self.out)) for x in s['argv']]
            stdout = (self.out/(s['name']+'.stdout.private')).open('wb')
            stderr = (self.out/(s['name']+'.stderr.private')).open('wb')
            self.handles += [stdout, stderr]
            child = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=stdout, stderr=stderr,
                                     start_new_session=True, preexec_fn=observer_limits)
            self.children.append(child)
            until = time.monotonic()+5
            while not ready.is_file() and child.poll() is None and time.monotonic() < until:
                time.sleep(0.02)
            require(ready.is_file() and ready.stat().st_size <= 4096 and child.poll() is None, 'sampler-not-ready')
        self.event('samplers-ready', count=len(self.children))

    def check_media(self, count):
        prefix = 'check-'+str(count)
        commands = [
            [self.c['ffprobe'], '-v', 'error', '-show_packets', '-show_entries',
             'packet=stream_index,codec_type,pts,dts,pts_time,dts_time', '-of', 'json', str(self.combined)],
            [self.c['ffmpeg'], '-nostdin', '-v', 'error', '-xerror', '-err_detect', 'explode',
             '-threads', '1', '-i', str(self.combined), '-map', '0:v:0', '-map', '0:a:0',
             '-threads', '1', '-f', 'null', '-']]
        for i, command in enumerate(commands):
            require(time.monotonic() < self.deadline, 'group-deadline')
            with (self.out/(prefix+f'-{i}.stdout')).open('wb') as stdout, \
                 (self.out/(prefix+f'-{i}.stderr')).open('wb') as stderr:
                r = subprocess.run(command, stdin=subprocess.DEVNULL, stdout=stdout, stderr=stderr,
                                   timeout=max(0.01, min(5, self.deadline-time.monotonic())), preexec_fn=child_limits)
            require(r.returncode == 0 and (self.out/(prefix+f'-{i}.stderr')).stat().st_size == 0,
                    'demux-failed' if i == 0 else 'decode-failed')
            if i == 0:
                packets = json.loads((self.out/(prefix+'-0.stdout')).read_text())['packets']
                last, types = {}, Counter()
                for packet in packets:
                    if packet.get('codec_type') not in ('audio', 'video'):
                        continue
                    index = packet['stream_index']
                    require('dts' in packet and 'pts' in packet, 'missing-packet-time')
                    dts = int(packet['dts'])
                    require(index not in last or dts > last[index], 'nonincreasing-dts')
                    last[index] = dts
                    types[packet['codec_type']] += 1
                require(types['audio'] >= 2 and types['video'] >= 2, 'missing-tracks')
        self.event('media-check', segments=count, packetTimingVerified=True,
                   strictDecoderExitVerified=True, decodedCompletenessVerified=False)

    def collect(self, master):
        variant = master_variant(self.c['startURL'], master)
        self.get(variant, 'variant-0000', 256*1024, first=True)
        last, count, media_seconds, poll = None, 0, 0.0, 0
        duration = self.c.get('durationSeconds', 30)
        # Every failed request is terminal. Re-reading an advancing event playlist
        # is normal HLS consumption, never a retry of an HTTP/protocol failure.
        while media_seconds < duration:
            entries = segments(variant, (self.out/(f'variant-{poll:04d}.private')).read_bytes())
            progressed = False
            for sequence, seconds, url in entries:
                if last is not None and sequence <= last:
                    continue
                require(last is None or sequence == last+1, 'missing-hls-sequence')
                if media_seconds+seconds > duration:
                    # End at a whole authored segment within the media budget.
                    # Never trim, normalize or rewrite the original media.
                    require(count >= self.c.get('minimumSegments', 5), 'insufficient-segments')
                    return
                data = self.get(url, f'segment-{count:06d}.ts', 16*1024*1024)
                require(data and len(data) % 188 == 0 and all(data[n] == 0x47 for n in range(0, len(data), 188)), 'invalid-ts')
                self.total += len(data)
                require(self.total <= 64*1024*1024, 'aggregate-media-bound')
                with self.combined.open('ab') as f:
                    f.write(data)
                count += 1
                self.check_media(count)
                self.event('segment', sequence=sequence, bytes=len(data), sha256=hashlib.sha256(data).hexdigest())
                last, progressed, media_seconds = sequence, True, media_seconds+seconds
                if media_seconds >= duration:
                    break
                require(time.monotonic()+seconds < self.deadline, 'group-deadline')
                time.sleep(seconds)
            if media_seconds >= duration:
                break
            require(time.monotonic()+0.1 < self.deadline, 'group-deadline')
            if not progressed:
                time.sleep(0.1)
            poll += 1
            require(poll <= 100, 'manifest-poll-bound')
            self.get(variant, f'variant-{poll:04d}', 256*1024)
        require(count >= self.c.get('minimumSegments', 5), 'insufficient-segments')

    def run(self):
        entered, stopped, failure, cleanup = False, False, None, True
        try:
            self.prepare_samplers()  # All setup precedes the only Start.
            self.deadline = time.monotonic()+50
            entered = True  # A failed/unknown Start still needs exact normal Stop.
            master = self.get(self.c['startURL'], 'master', 256*1024)
            self.collect(master)  # No tool/message/LLM or observer setup roundtrip.
        except Exception as e:
            failure = str(e) if isinstance(e, Failure) else type(e).__name__
        finally:
            # Once cleanup begins, a second terminal signal must not bypass the
            # single normal Stop or leave an owned observer child behind.
            handlers = {s: signal.signal(s, signal.SIG_IGN) for s in (signal.SIGINT, signal.SIGTERM)}
            if entered:
                try:
                    self.get(self.c['stopURL'], 'stop', 256*1024, stopping=True)
                    stopped = True
                except Exception:
                    failure = failure or 'normal-stop-failed'
            for child in self.children:
                # This is the observer we started, with its bounded kubectl
                # client. Never signal a lab workload.
                if child.poll() is None:
                    # Cooperative observer finishes and reaps its bounded client
                    # before final reads. Group termination is failure fallback.
                    child.send_signal(signal.SIGTERM)
                try:
                    child.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    os.killpg(child.pid, signal.SIGKILL)
                    cleanup = False
                    try:
                        child.wait(timeout=3)
                    except subprocess.TimeoutExpired:
                        failure = failure or 'sampler-cleanup-unresolved'
                if child.returncode != 0:
                    failure = failure or 'sampler-failed'
                try:
                    os.killpg(child.pid, 0)
                except ProcessLookupError:
                    pass
                else:
                    cleanup = False  # Reaping its parent is not descendant proof.
            for handle in self.handles:
                handle.close()
            self.event('finished', failed=failure is not None, normalStopHTTP200=stopped,
                       samplersReaped=all(p.poll() is not None for p in self.children))
            for s, handler in handlers.items():
                signal.signal(s, handler)
        summary = dict(failed=failure is not None or not cleanup, errorCode=failure,
                       normalStopHTTP200=stopped, retirementVerified=False,
                       ownedSamplersReaped=all(p.poll() is not None for p in self.children),
                       samplerGroupCleanupVerified=cleanup,
                       mediaBytes=self.total, decodedCompletenessVerified=False,
                       placementAndFencingVerified=False)
        (self.out/'summary.json').write_text(json.dumps(summary, indent=2)+'\n')
        return summary


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--config', required=True, type=Path)
    p.add_argument('--output', type=Path)
    p.add_argument('--dry-parse-master', type=Path)
    p.add_argument('--run', action='store_true', help='Only after separate exact fresh-session authorization')
    a = p.parse_args()
    os.umask(0o077)
    try:
        require(a.config.stat().st_size <= 64*1024 and a.config.stat().st_mode & 0o077 == 0, 'private-config')
        c = json.loads(a.config.read_text())
        validate(c)
        require(not (a.run and a.dry_parse_master), 'dry-parse-cannot-run')
        if a.dry_parse_master:
            master_variant(c['startURL'], a.dry_parse_master.read_bytes())
        if not a.run:
            print(json.dumps(dict(preflightPassed=True, httpRequests=0, subprocesses=0,
                                  masterParsed=bool(a.dry_parse_master), liveAcceptance=False)))
            return 0
        require(a.output is not None and not a.output.exists(), 'fresh-output-required')
        a.output.mkdir(mode=0o700, parents=True)
        def interrupted(_signum, _frame):
            raise Failure('interrupted')
        signal.signal(signal.SIGTERM, interrupted)
        signal.signal(signal.SIGINT, interrupted)
        result = Attempt(c, a.output).run()
        print(json.dumps(result))
        return int(result['failed'])
    except Exception as e:
        print(json.dumps(dict(preflightPassed=False, errorCode=str(e) if isinstance(e, Failure) else type(e).__name__)))
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
