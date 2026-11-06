#!/usr/bin/env python3
"""Isolated generated-media Newznab + real yEnc NNTP fixture. No provider proxy."""
import argparse
import binascii
from collections import Counter
from datetime import datetime, timezone
from email.utils import format_datetime
from html import escape
import http.server
import json
import re
from pathlib import Path
import socketserver
import threading
from urllib.parse import parse_qs, urlsplit


class Fixture:
    def __init__(self, media, origin, tmdb, release, fixture_id='ether035-fixture', article_prefix='ether035-ha-part'):
        if not all(re.fullmatch(r'[a-zA-Z0-9_-]{1,64}', v) for v in (fixture_id, article_prefix)):
            raise ValueError('invalid fixture namespace')
        self.fixture_id, self.article_prefix = fixture_id, article_prefix
        self.data = media.read_bytes()
        self.filename = media.name
        self.origin, self.tmdb, self.release = origin.rstrip('/'), tmdb, release
        self.chunks = [self.data[i:i+256*1024] for i in range(0,len(self.data),256*1024)]
        self.articles = {f'{self.article_prefix}-{i+1:05d}@fixture.invalid': self.encode(i) for i in range(len(self.chunks))}
        self.stats = Counter()
        self.lock = threading.Lock()

    def count(self, key, value=1):
        with self.lock:
            self.stats[key] += value

    def snapshot(self):
        with self.lock:
            return dict(self.stats, generatedBytes=len(self.data), parts=len(self.chunks))

    def encode(self, index):
        data = self.chunks[index]
        header = (f'=ybegin part={index+1} total={len(self.chunks)} line=128 size={len(self.data)} name={self.filename}\r\n'
                  f'=ypart begin={index*256*1024+1} end={index*256*1024+len(data)}\r\n').encode()
        lines, line = [], bytearray()
        for byte in data:
            value = (byte+42)&255
            encoded = bytes((61,(value+64)&255)) if value in (0,10,13,61) else bytes((value,))
            if len(line)+len(encoded)>128:
                lines.append(bytes(line)); line.clear()
            line.extend(encoded)
        if line:
            lines.append(bytes(line))
        crc = binascii.crc32(data)&0xffffffff
        tail = f'=yend size={len(data)} part={index+1} pcrc32={crc:08x}'
        if index == len(self.chunks)-1:
            tail += f' crc32={binascii.crc32(self.data)&0xffffffff:08x}'
        return header+b'\r\n'.join(lines)+b'\r\n'+tail.encode()+b'\r\n'

    def nzb(self):
        segments = ''.join(f'<segment bytes="{len(chunk)}" number="{i+1}">{self.article_prefix}-{i+1:05d}@fixture.invalid</segment>'
                           for i,chunk in enumerate(self.chunks))
        return (f'<?xml version="1.0" encoding="UTF-8"?><nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">'
                f'<file poster="lab@fixture.invalid" date="1577836800" subject="&quot;{escape(self.filename)}&quot; yEnc (1/{len(self.chunks)})">'
                f'<groups><group>alt.binaries.ether.fixture</group></groups><segments>{segments}</segments></file></nzb>').encode()

    def rss(self):
        item_url = escape(self.origin+'/api?t=get&amp;id='+self.fixture_id).replace('&amp;amp;','&amp;')
        now = format_datetime(datetime.now(timezone.utc))
        return (f'<?xml version="1.0"?><rss version="2.0" xmlns:newznab="http://www.newznab.com/DTD/2010/feeds/attributes/">'
                f'<channel><title>Ether generated fixture</title><item><title>{escape(self.release)}</title>'
                f'<guid isPermaLink="false">{self.fixture_id}</guid><link>{item_url}</link><pubDate>{now}</pubDate>'
                f'<enclosure url="{item_url}" length="{len(self.data)}" type="application/x-nzb"/>'
                f'<newznab:attr name="category" value="2000"/><newznab:attr name="size" value="{len(self.data)}"/>'
                f'<newznab:attr name="tmdbid" value="{self.tmdb}"/><newznab:attr name="files" value="1"/>'
                f'</item></channel></rss>').encode()


class HTTP(http.server.BaseHTTPRequestHandler):
    def log_message(self,*args):
        pass  # Do not log query strings: indexer clients may send API keys.

    def do_GET(self):
        f = self.server.fixture
        parsed = urlsplit(self.path)
        query = parse_qs(parsed.query)
        kind = query.get('t',[''])[0]
        content_type = 'application/xml'
        if parsed.path == '/stats':
            body = json.dumps(f.snapshot()).encode(); content_type = 'application/json'
        elif parsed.path == '/healthz':
            body = b'ok'
        elif parsed.path != '/api':
            self.send_error(404); return
        elif kind == 'caps':
            f.count('caps')
            body = b'''<?xml version="1.0"?><caps><server version="1.0" title="Ether generated fixture"/><limits max="100" default="100"/><searching><search available="yes" supportedParams="q"/><movie-search available="yes" supportedParams="q,tmdbid,imdbid"/><tv-search available="no"/></searching><categories><category id="2000" name="Movies"><subcat id="2030" name="SD"/></category></categories></caps>'''
        elif kind == 'get' and query.get('id') == [f.fixture_id]:
            f.count('nzbDownloads'); body = f.nzb()
        elif kind in ('search','movie','tvsearch'):
            f.count('searches'); body = f.rss()
        else:
            self.send_error(404); return
        self.send_response(200)
        self.send_header('Content-Type',content_type)
        self.send_header('Content-Length',str(len(body)))
        self.end_headers(); self.wfile.write(body)


class NNTP(socketserver.StreamRequestHandler):
    def handle(self):
        f = self.server.fixture
        self.connection.settimeout(120)
        self.wfile.write(b'200 Ether generated-media fixture ready\r\n'); self.wfile.flush()
        while True:
            raw = self.rfile.readline(8192)
            if not raw:
                return
            args = raw.decode('ascii','replace').strip().split()
            if not args:
                return
            command = args[0].upper()
            if command == 'QUIT':
                self.wfile.write(b'205 closing\r\n'); self.wfile.flush(); return
            if command == 'CAPABILITIES':
                reply = b'101 capability list\r\nVERSION 2\r\nREADER\r\nPIPELINING\r\n.\r\n'
            elif command == 'MODE': reply = b'200 reader mode\r\n'
            elif command == 'AUTHINFO': reply = b'281 accepted lab credential\r\n' if len(args)>1 and args[1].upper()=='PASS' else b'381 password required\r\n'
            elif command == 'DATE': reply = ('111 '+datetime.now(timezone.utc).strftime('%Y%m%d%H%M%S')+'\r\n').encode()
            elif command == 'GROUP': reply = f'211 {len(f.chunks)} 1 {len(f.chunks)} alt.binaries.ether.fixture\r\n'.encode()
            elif command in ('BODY','ARTICLE','HEAD','STAT') and len(args)>1:
                article_id = args[1].strip('<>')
                body = f.articles.get(article_id)
                if body is None:
                    f.count('missingArticles'); reply = b'430 no such article\r\n'
                else:
                    f.count(command.lower()+'Requests')
                    code = {'BODY':222,'ARTICLE':220,'HEAD':221,'STAT':223}[command]
                    reply = f'{code} 0 <{article_id}> article follows\r\n'.encode()
                    headers = f'From: lab@fixture.invalid\r\nSubject: {f.filename}\r\nMessage-ID: <{article_id}>\r\n'.encode()
                    if command in ('HEAD','ARTICLE'): reply += headers
                    if command == 'ARTICLE': reply += b'\r\n'
                    if command in ('BODY','ARTICLE'):
                        f.count('articlePayloadBytes',len(body))
                        # NNTP dot transparency is independent of yEnc escaping.
                        reply += b'\r\n'.join(b'.'+line if line.startswith(b'.') else line for line in body.split(b'\r\n'))
                    if command != 'STAT':
                        if not reply.endswith(b'\r\n'): reply += b'\r\n'
                        reply += b'.\r\n'
            else: reply = b'500 unsupported fixture command\r\n'
            self.wfile.write(reply); self.wfile.flush()


class TCP(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--media',type=Path,required=True)
    p.add_argument('--origin',required=True)
    p.add_argument('--http-port',type=int,default=8080)
    p.add_argument('--nntp-port',type=int,default=1190)
    p.add_argument('--tmdb',type=int,default=900001)
    p.add_argument('--release',default='Ether.Cluster.Fixture.2020.360p.WEB-DL.MPEG4.AAC')
    p.add_argument('--fixture-id',default='ether035-fixture')
    p.add_argument('--article-prefix',default='ether035-ha-part')
    a = p.parse_args()
    fixture = Fixture(a.media,a.origin,a.tmdb,a.release,a.fixture_id,a.article_prefix)
    web = http.server.ThreadingHTTPServer(('0.0.0.0',a.http_port),HTTP); web.fixture=fixture
    nntp = TCP(('0.0.0.0',a.nntp_port),NNTP); nntp.fixture=fixture
    thread = threading.Thread(target=nntp.serve_forever); thread.start()
    print(json.dumps(dict(fixture='generated-media',parts=len(fixture.chunks),bytes=len(fixture.data))),flush=True)
    try: web.serve_forever()
    finally:
        web.server_close(); nntp.shutdown(); nntp.server_close(); thread.join()


if __name__ == '__main__': main()
