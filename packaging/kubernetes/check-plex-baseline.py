#!/usr/bin/env python3
"""Offline transport/control checks. No HTTP, observer, decoder or media acceptance."""
import contextlib
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import sys
import tempfile
from unittest.mock import patch
from urllib.error import HTTPError

root=Path(__file__).resolve().parent
spec=importlib.util.spec_from_file_location('plex_baseline',root/'plex-baseline.py')
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
base='http://127.0.0.1:18036'
session='offline-fixture'
start=base+'/video/:/transcode/universal/start.m3u8?session='+session
stop=base+'/video/:/transcode/universal/stop?session='+session
variant=base+'/video/:/transcode/universal/session/'+session+'/base/index.m3u8?X-Ether-Stream-Token=fixture%2Bvalue&X-Ether-Read-Token=fixture2'
master=('#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\n'+variant+'\n').encode()
playlist=b'#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:4,\n0.ts?fixture=%2B\n#EXTINF:4,\n1.ts?fixture=%2B\n'
c={'session':session,'requestID':'offline','startURL':start,'stopURL':stop,'durationSeconds':8,
   'minimumSegments':2,'headers':{},'samplers':[]}
class Reply:
    status=200
    def __init__(self,body):self.body=body
    def read(self,_):return self.body
    def __enter__(self):return self
    def __exit__(self,*_):pass
class Opener:
    def __init__(self,scenario):self.scenario=scenario;self.urls=[]
    def open(self,req,timeout):
        url=req.full_url;self.urls.append(url)
        if (self.scenario=='http403' and url==variant) or (self.scenario=='stop403' and url==stop):
            raise HTTPError(url,403,'private',{},io.BytesIO(b'Forbidden'))
        if url==start:return Reply(b'bad' if self.scenario=='bad-master' else master)
        if url==stop:return Reply(b'')
        if url==variant:return Reply(playlist)
        return Reply(b'G'+b'\x00'*187)
with tempfile.TemporaryDirectory(prefix='ether-plex-baseline-check-') as td:
    d=Path(td)
    for scenario in ['success','http403','stop403','bad-master','delayed','packet-failure','not-ready','whole-budget']:
        out=d/scenario;out.mkdir();cfg={**c,'durationSeconds':7,'minimumSegments':1} if scenario=='whole-budget' else c;a=m.Attempt(cfg,out);a.opener=Opener(scenario)
        def ready():
            if scenario=='not-ready':raise m.Failure('sampler-not-ready')
            a.event('samplers-ready')
        a.prepare_samplers=ready
        def check(_):
            if scenario=='packet-failure':raise m.Failure('nonincreasing-dts')
        a.check_media=check
        if scenario=='delayed':
            collect=a.collect
            def delayed(data):
                a.start_response-=2_000_000_000
                collect(data)
            a.collect=delayed
        with patch.object(m.time,'sleep'):
            result=a.run()
        urls=a.opener.urls
        assert urls.count(start)==(0 if scenario=='not-ready' else 1),(scenario,urls)
        assert urls.count(stop)==(0 if scenario=='not-ready' else 1),(scenario,urls)
        assert result['failed']==(scenario not in ('success','whole-budget')),(scenario,result)
        assert result['retirementVerified'] is False
        if scenario=='success':
            assert urls[1]==variant  # Full authored query remains byte-identical.
            assert result['mediaBytes']==376
            event=next(e for e in a.events if e['kind']=='first-media-get')
            assert 0<=event['startResponseToFirstGETSeconds']<1
        if scenario=='whole-budget':assert result['mediaBytes']==188 and len(urls)==4
        if scenario=='http403':assert urls==[start,variant,stop]
        if scenario=='delayed':assert urls==[start,stop] and result['errorCode']=='delayed-first-get'
    for uri in ['http://example.test/secret','../secret','/%2fsecret','//other/secret']:
        try:m.authored(start,uri)
        except m.Failure:pass
        else:raise AssertionError('unsafe URI accepted')
    # Default CLI preflight and dry parse cannot open sockets or start children.
    tool=d/'fixture-tool';tool.write_text('offline fixture, never executed');tool.chmod(0o700)
    digest=hashlib.sha256(tool.read_bytes()).hexdigest()
    cfg={**c,'headers':{'Host':'ether035-roles.test','X-Plex-Token':'ether-cast-fixture',
         'X-Plex-Client-Identifier':'offline','X-Plex-Session-Identifier':session,
         'X-Ether-Lab-Request-ID':'offline'},'ffmpeg':str(tool),'ffprobe':str(tool),
         'ffmpegSHA256':digest,'ffprobeSHA256':digest,
         'samplers':[{'name':'fixture','argv':[sys.executable,str(tool),'{readyFile}','{outputDir}'],'scriptSHA256':digest,'inputs':[{'path':str(tool),'sha256':digest}]}]}
    config=d/'config.private.json';config.write_text(json.dumps(cfg));config.chmod(0o600)
    saved=d/'master.private';saved.write_bytes(master)
    with patch.object(sys,'argv',['plex-baseline.py','--config',str(config),'--dry-parse-master',str(saved)]), \
         patch.object(m,'build_opener',side_effect=AssertionError('HTTP forbidden')), \
         patch.object(m.subprocess,'Popen',side_effect=AssertionError('subprocess forbidden')), \
         contextlib.redirect_stdout(io.StringIO()) as output:
        assert m.main()==0
        assert json.loads(output.getvalue())['httpRequests']==0
print('offline synchronous Plex harness checks passed; no live or media acceptance')

# Run the real observer only against an offline kubectl stub. This checks its
# readiness barrier and cooperative child reaping, never live cluster state.
import os
import signal
import subprocess
import time
with tempfile.TemporaryDirectory(prefix='ether-observer-check-') as td:
    d=Path(td);out=d/'output';out.mkdir()
    roles={axis:dict(name='ether035-roles-ether-'+role+'-'+('1' if axis=='controller' else '0'),
                    role=role,nodeID=axis,boot=axis+'-boot',podUID=axis+'-uid',
                    containerID=axis+'-container',imageID='fixture-image')
           for axis,role in [('source','source'),('encoder','transcode'),('controller','server'),('delivery','delivery')]}
    held=dict(nodeID='held',boot='held-boot',podUID='held-uid',containerID='held-container',imageID='old-image')
    exp={'roles':roles,'held0':held,'fixture':{'name':'ether035-ha-source-fixture','podUID':'fixture-uid'},
         'ingress':{'name':'ether035-ha-ingress-fixture','podUID':'ingress-uid'}}
    items=[]
    for e in list(roles.values())+[dict(held,name='ether035-roles-ether-server-0')]:
        items.append(dict(kind='Pod',metadata=dict(name=e['name'],uid=e['podUID']),
                          status=dict(containerStatuses=[dict(name='ether',containerID=e['containerID'],imageID=e['imageID'],ready=e is not held and e['nodeID']!='held',state=dict(running={}))])))
    for key in ['fixture','ingress']:
        items.append(dict(kind='Pod',metadata=dict(name=exp[key]['name'],uid=exp[key]['podUID'])))
    for service,role in [('ether035-roles-ether','controller'),('ether035-roles-ether-media','delivery')]:
        items.append(dict(kind='EndpointSlice',metadata=dict(labels={'kubernetes.io/service-name':service}),
                          endpoints=[dict(conditions=dict(ready=True),targetRef=dict(uid=roles[role]['podUID']))]))
    db=dict(placement=None,route=None,schemaCount=257,members=[dict(name=e['name'],id=e['nodeID'],incarnation=e['boot'],draining=0,protocol_version=1,roles=[e['role']]) for e in roles.values()])
    db['members'].append(dict(name='ether035-roles-ether-server-0',id='held',incarnation='held-boot',draining=1))
    for name,x in [('expected',exp),('resources',dict(items=items)),('database',db)]:
        (d/(name+'.json')).write_text(json.dumps(x))
    stub=d/'kubectl';stub.write_text('''#!/usr/bin/env python3
import sys,json
from pathlib import Path
p=Path(__file__).parent
a=sys.argv[1:]
assert not any(v in a for v in ['delete','apply','drain','rollout','patch'])
if 'get' in a: print((p/'resources.json').read_text())
elif 'psql' in a:
    assert 'READ ONLY' in sys.stdin.read()
    print((p/'database.json').read_text())
elif 'python3' in a: print('{"bodyRequests":0}')
elif '/bin/sh' in a: pass
else: sys.exit(93)
''');stub.chmod(0o700)
    child=subprocess.Popen([sys.executable,str(root/'baseline-observer.py'),'--kubectl',str(stub),
        '--expected',str(d/'expected.json'),'--session','unused-offline','--request-id','offline',
        '--ready-file',str(out/'observer.ready'),'--output-dir',str(out)],stdout=subprocess.PIPE,stderr=subprocess.PIPE)
    try:
        until=time.monotonic()+5
        while not (out/'observer.ready').exists() and child.poll() is None and time.monotonic()<until:
            time.sleep(0.01)
        assert (out/'observer.ready').exists(),'observer failed before readiness'
        child.send_signal(signal.SIGTERM)
        stdout,stderr=child.communicate(timeout=5)
        assert child.returncode==0,(child.returncode,stderr)
        assert json.loads((out/'observer-completion.json').read_text())['failed'] is False
    finally:
        if child.poll() is None:
            child.kill();child.wait()
print('offline observer readiness/shutdown checks passed; no live CLI calls')
