#!/usr/bin/env python3
"""Build a digest-verified lab file manifest from a reference Items listing."""
import argparse
import hashlib
import json
import os
import subprocess

p = argparse.ArgumentParser()
p.add_argument('--items', required=True)
p.add_argument('--media', required=True, help='host directory mounted as /media in reference and Ether')
p.add_argument('--ffprobe', required=True)
p.add_argument('--reference-container', required=True)
p.add_argument('--output', required=True)
a = p.parse_args()
items = json.load(open(a.items, encoding='utf8'))['Items']
series = {i['Id']: int(i.get('ProviderIds', {}).get('Tmdb', '0') or 0) for i in items if i.get('Type') == 'Series'}
entries, seen = [], set()
for item in items:
    kind, path = item.get('Type'), item.get('Path', '')
    if kind not in ('Movie', 'Episode') or not path.startswith('/media/') or path in seen:
        continue
    rel = os.path.normpath(path[len('/media/'):])
    if rel == '..' or rel.startswith('../') or os.path.isabs(rel):
        raise ValueError('reference path escapes media mount')
    host_path = os.path.join(a.media, rel)
    tmdb = int(item.get('ProviderIds', {}).get('Tmdb', '0') or 0) if kind == 'Movie' else series.get(item.get('SeriesId'), 0)
    if not tmdb or not os.path.isfile(host_path):
        raise ValueError('missing ID or media file: ' + path)
    reference_hash = subprocess.check_output(['docker', 'exec', a.reference_container, 'sha256sum', path], text=True).split()[0]
    with open(host_path, 'rb') as stream:
        host_hash = hashlib.file_digest(stream, 'sha256').hexdigest()
    if host_hash != reference_hash:
        raise ValueError('reference digest mismatch: ' + path)
    probe = json.loads(subprocess.check_output([a.ffprobe, '-v', 'error', '-show_streams', '-of', 'json', host_path]))['streams']
    video = next((s for s in probe if s['codec_type'] == 'video'), {})
    audio = next((s for s in probe if s['codec_type'] == 'audio'), {})
    entries.append({'tmdb': tmdb, 'season': item.get('ParentIndexNumber', 0) if kind == 'Episode' else 0,
                    'episode': item.get('IndexNumber', 0) if kind == 'Episode' else 0, 'path': path, 'sha256': reference_hash,
                    'info': {'height': video.get('height', 0), 'videoCodec': video.get('codec_name', ''),
                             'audioCodec': audio.get('codec_name', ''), 'container': os.path.splitext(path)[1][1:].lower(), 'source': 'webdl'}})
    seen.add(path)
if not entries:
    raise ValueError('no playable items')
with open(a.output, 'x', encoding='utf8') as output:
    json.dump(entries, output, indent=2)
    output.write('\n')
print('Verified', len(entries), 'files against reference-container SHA-256')
