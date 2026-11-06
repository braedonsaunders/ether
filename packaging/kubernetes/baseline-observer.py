#!/usr/bin/env python3
"""Bounded read-only lab observer. Only the synchronous baseline starts it.

All output is private. Expectation JSON pins roles (allocation oracle shape),
held0 (podUID/containerID/imageID), fixture and ingress (name/podUID). No auth,
media requests, migrations, drain, reset, release, deletion or configuration write.
"""
import argparse
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import time


def main():
    p = argparse.ArgumentParser(description=__doc__)
    for name in ('kubectl', 'expected', 'session', 'request-id', 'ready-file', 'output-dir'):
        p.add_argument('--'+name, required=True)
    a = p.parse_args()
    if not re.fullmatch('[A-Za-z0-9_-]{1,96}', a.session):
        return 1
    os.umask(0o077)
    out, ready = Path(a.output_dir), Path(a.ready_file)
    if ready.parent != out or ready.exists() or not out.is_dir():
        return 1
    expected = json.loads(Path(a.expected).read_text())
    if set(expected['roles']) != {'source', 'encoder', 'controller', 'delivery'}:
        return 1
    k = [a.kubectl, '--context', 'kind-ether035-ha', '--request-timeout=2s', '-n', 'ether035-ha']
    stopping = False
    def stop(_signum, _frame):
        nonlocal stopping
        stopping = True  # Finish/reap the current bounded child, then final reads.
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    number = 0
    def call(label, argv, text=None):
        nonlocal number
        number += 1
        result = subprocess.run(k+argv, input=text, capture_output=True, text=True, timeout=3)
        record = {'wallTime':time.time(), 'monotonicNS':time.monotonic_ns(),
                  'exitCode':result.returncode, 'stdout':result.stdout, 'stderr':result.stderr}
        (out/f'observer-{number:04d}-{label}.private.json').write_text(json.dumps(record)+'\n')
        if result.returncode:
            raise RuntimeError('read-only-observation-failed')
        return result.stdout
    def pods_check():
        items = json.loads(call('resources', ['get', 'pods,services,endpointslices', '-o', 'json']))['items']
        pods = {x['metadata']['name']:x for x in items if x['kind']=='Pod'}
        for role, e in expected['roles'].items():
            pod = pods[e['name']]
            state = next(x for x in pod['status']['containerStatuses'] if x['name']=='ether')
            if not (pod['metadata']['uid']==e['podUID'] and state['containerID']==e['containerID']
                    and state['imageID']==e['imageID'] and state['ready'] and 'running' in state['state']):
                raise RuntimeError('role-instance-drift')
        pod = pods['ether035-roles-ether-server-0'];e = expected['held0']
        state = next(x for x in pod['status']['containerStatuses'] if x['name']=='ether')
        if not (pod['metadata']['uid']==e['podUID'] and state['containerID']==e['containerID']
                and state['imageID']==e['imageID'] and not state['ready'] and 'running' in state['state']):
            raise RuntimeError('held-instance-drift')
        for name in ('fixture', 'ingress'):
            e = expected[name]
            if pods[e['name']]['metadata']['uid'] != e['podUID']:
                raise RuntimeError('infrastructure-instance-drift')
        for service, role in [('ether035-roles-ether', 'controller'), ('ether035-roles-ether-media', 'delivery')]:
            eps = [e for x in items if x['kind']=='EndpointSlice'
                   and x['metadata']['labels'].get('kubernetes.io/service-name')==service
                   for e in x.get('endpoints', []) if e.get('conditions', {}).get('ready') is True]
            if len(eps)!=1 or eps[0].get('targetRef', {}).get('uid')!=expected['roles'][role]['podUID']:
                raise RuntimeError('public-endpoint-drift')
    def database(initial=False):
        sql = f'''BEGIN READ ONLY;
SELECT json_build_object('at',clock_timestamp(),
'placement',(SELECT row_to_json(x) FROM (SELECT session,generation,controller,controller_boot,node,boot,retired FROM placement_sessions WHERE session='{a.session}') x),
'route',(SELECT route::json FROM clusterplayback_routes WHERE session='{a.session}'),
'claims',(SELECT coalesce(json_agg(x),'[]') FROM (SELECT scope,units FROM placement_claims WHERE session='{a.session}') x),
'retirement',(SELECT receipt::jsonb - 'Operation' FROM placement_history_receipts WHERE session='{a.session}'),
'members',(SELECT json_agg(x) FROM (SELECT name,id,incarnation,draining,protocol_version,caps::json->'roles' AS roles FROM cluster_nodes WHERE name LIKE 'ether035-roles-%') x),
'schemaCount',(SELECT count(*) FROM schema_migrations)); COMMIT;'''
        x = json.loads(call('authority', ['exec', '-i', 'ether035-ha-pg-0', '--', 'psql', '-U', 'ether', '-d', 'ether', '-Atq', '-v', 'ON_ERROR_STOP=1'], sql))
        if x['schemaCount'] != 257:
            raise RuntimeError('schema-drift')
        if initial and (x['placement'] is not None or x['route'] is not None):
            raise RuntimeError('session-already-exists')
        for role, e in expected['roles'].items():
            rows = [m for m in x['members'] if m['name']==e['name']]
            if len(rows)!=1 or not (rows[0]['id']==e['nodeID'] and rows[0]['incarnation']==e['boot']
                    and rows[0]['draining']==0 and rows[0]['protocol_version']>0 and rows[0]['roles']==[e['role']]):
                raise RuntimeError('role-membership-drift')
        rows = [m for m in x['members'] if m['name']=='ether035-roles-ether-server-0']
        h = expected['held0']
        if len(rows)!=1 or not (rows[0]['id']==h['nodeID'] and rows[0]['incarnation']==h['boot'] and rows[0]['draining']==1):
            raise RuntimeError('held-membership-drift')
        if x['placement'] and x['route']:
            r, q, roles = x['placement'], x['route'], expected['roles']
            if not (r['node']==roles['encoder']['nodeID'] and r['boot']==roles['encoder']['boot']
                    and r['controller']==roles['controller']['nodeID'] and r['controller_boot']==roles['controller']['boot']
                    and q['Source']['NodeID']==roles['source']['nodeID'] and q['Source']['Boot']==roles['source']['boot']):
                raise RuntimeError('allocation-fallback-or-drift')
    def counters():
        call('source-counters', ['exec', expected['fixture']['name'], '--', 'python3', '-c',
             "import urllib.request;print(urllib.request.urlopen('http://127.0.0.1:8080/stats',timeout=1).read().decode())"])
    process = '''for d in /proc/[0-9]*; do [ -r "$d/comm" ] || continue; c=$(cat "$d/comm" 2>/dev/null) || continue; [ "$c" = ffmpeg ] || continue; printf 'PID:%s\\n' "${d##*/}"; cat "$d/stat"; head -c 16384 "$d/cmdline" | base64 -w0; printf '\\n'; done'''
    result = {'failed':False, 'errorCode':None}
    try:
        pods_check(); database(initial=True); counters()
        ready.write_text(json.dumps({'ready':True,'wallTime':time.time(),'monotonicNS':time.monotonic_ns()})+'\n')
        deadline = time.monotonic()+60
        while not stopping and time.monotonic()<deadline:
            call('encoder-processes', ['exec', expected['roles']['encoder']['name'], '-c', 'ether', '--', '/bin/sh', '-ec', process])
            database()
            if not stopping:
                time.sleep(0.25)
        if not stopping:
            raise RuntimeError('observer-wall-bound')
    except Exception as e:
        result = {'failed':True, 'errorCode':str(e) if type(e) is RuntimeError else type(e).__name__}
    finally:
        try:
            database(); counters(); pods_check()
        except Exception as e:
            result = {'failed':True, 'errorCode':type(e).__name__}
        (out/'observer-completion.json').write_text(json.dumps(result)+'\n')
    return int(result['failed'])


if __name__=='__main__':
    raise SystemExit(main())
