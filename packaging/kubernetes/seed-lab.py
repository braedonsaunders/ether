#!/usr/bin/env python3
"""Seed an isolated generated-media lab via shipping owner APIs + fixture-only SQL.

No external account claim. Credentials stay in the supplied private directory.
Requires a loopback port-forward to a full/server node or the lab Ingress,
and the named lab PostgreSQL pod.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import secrets
import subprocess
import time
from urllib.parse import urlsplit
from urllib.request import Request, urlopen


def save(path, text):
    fd = os.open(path, os.O_WRONLY|os.O_CREAT|os.O_TRUNC,0o600)
    os.fchmod(fd,0o600)
    with os.fdopen(fd,'w') as out: out.write(text)


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--base',required=True)
    p.add_argument('--host',choices=['ether035-roles.test'],help='Approved lab Ingress Host header')
    p.add_argument('--namespace',default='ether035-ha')
    p.add_argument('--context', choices=['kind-ether035-ha', 'ether035-multihost'], default='kind-ether035-ha')
    p.add_argument('--kubectl',default='kubectl')
    p.add_argument('--credentials-dir',required=True,type=Path)
    p.add_argument('--refresh-only',action='store_true',help='Refresh fixture delegation without rewriting source configuration')
    p.add_argument('--long-fixture',action='store_true',help='Add isolated 600s title900002 and its own fixture source')
    a=p.parse_args()
    if a.namespace != 'ether035-ha' or urlsplit(a.base).hostname not in ('127.0.0.1','localhost'):
        p.error('only ether035-ha namespace and loopback API origin are allowed')
    k=[a.kubectl,'--context',a.context,'--request-timeout=15s','-n',a.namespace]
    pg=json.loads(subprocess.check_output(k+['get','pod','ether035-ha-pg-0','-o','json'],timeout=20))
    if pg['metadata']['labels'].get('app')!='ether035-ha-pg':
        p.error('PostgreSQL pod is not the lab fixture')
    a.credentials_dir.mkdir(parents=True,exist_ok=True)
    a.credentials_dir.chmod(0o700)
    credentials_path=a.credentials_dir/'local-owner.json'
    existing=credentials_path.exists()
    credentials=json.loads(credentials_path.read_text()) if existing else dict(username='ether035-lab-owner',password=secrets.token_urlsafe(24))
    if credentials['username']!='ether035-lab-owner': p.error('unexpected local viewer')
    headers={'Content-Type':'application/json','Origin':a.base,'X-Ether-Request':'1'}
    if a.host:
        headers['Host']=a.host
        headers['Origin']=urlsplit(a.base).scheme+'://'+a.host
    def call(path, method='GET', value=None):
        with urlopen(Request(a.base+path,headers=headers,method=method,
                             data=None if value is None else json.dumps(value).encode()),timeout=30) as response:
            return json.load(response)
    try:
        login=call('/ether/api/auth/local' if existing else '/ether/api/setup/local','POST',credentials)
        save(credentials_path,json.dumps(credentials))
        save(a.credentials_dir/'admin-token',login['token'])
        headers['X-Plex-Token']=login['token']
        config=call('/ether/api/config')['config']
        if config['plex'].get('claimed'):
            p.error('refusing a claimed server')
        if not a.refresh_only:
            allowed={'ether035-ha-source','ether035-ha-long-source'}
            providers=config['usenet']['providers'] or []
            indexers=config['indexers'] or []
            if any(v['id'] not in allowed for v in providers+indexers):
                p.error('refusing to replace nonfixture sources')
            fixture_id='ether035-ha-long-source' if a.long_fixture else 'ether035-ha-source'
            fixture_host=fixture_id+'.ether035-ha.svc.cluster.local'
            config['usenet']['providers']=[dict(id=fixture_id,name='Ether generated fixture',
                host=fixture_host,port=1190,tls=False,
                username='',password='',connections=4,enabled=True,priority=0)]+[v for v in providers if v['id']!=fixture_id]
            config['usenet']['minIdle']=1
            config['indexers']=[dict(id=fixture_id,name='Ether generated fixture',
                url='http://'+fixture_host+':8080',apiKey='',priority=0,
                enabled=True,protocol='usenet',movieCategories=[2000])]+[v for v in indexers if v['id']!=fixture_id]
            call('/ether/api/config','PUT',config)
    except Exception as error:
        # HTTP exception strings/response bodies can contain credential material.
        raise SystemExit('lab owner/config API failed: '+type(error).__name__)
    token='ether-cast-'+secrets.token_urlsafe(24)
    save(a.credentials_dir/'playback-token',token)
    save(a.credentials_dir/'playback-headers.json',json.dumps({
        'X-Plex-Token':token,'X-Plex-Client-Identifier':'ether035-ha-generated-client',
        'X-Plex-Product':'Plex Web','X-Plex-Platform':'Chrome'}))
    now=int(time.time())
    # Mirrors the current detailed catalog fixture columns. No generated columns,
    # identity cloning, raw upstream account token, or production DB URL involved.
    tmdb=900002 if a.long_fixture else 900001
    rating_key=1000000000000000+tmdb
    title='Ether Long Cluster Fixture' if a.long_fixture else 'Ether Cluster Fixture'
    runtime=600 if a.long_fixture else 180
    sql=f'''BEGIN;
INSERT INTO catalog_titles(key,type,tmdb_id,title,original_title,sort_title,year,released_ts,runtime_sec,language,content_rating,content_rating_rank,browsable,added_at,fetched_at,detail,detail_at,primary_released_ts,year_source,year_checked_at)
VALUES({rating_key},'movie',{tmdb},'{title}','{title}','{title.lower()}',2020,1577836800,{runtime},'en','PG',2,1,{now},{now},1,{now},1577836800,'primary',{now})
ON CONFLICT(key) DO UPDATE SET detail=1,detail_at={now},fetched_at={now},primary_released_ts=1577836800,year_source='primary',year_checked_at={now};
INSERT INTO auth_delegations(token_hash,user_id,expires_at)
SELECT '{hashlib.sha256(token.encode()).hexdigest()}',id,{now+900} FROM auth_users
WHERE username='ether035-lab-owner' AND active=1 AND origin='local';
COMMIT;
'''
    result=subprocess.run(k+['exec','-i','ether035-ha-pg-0','--','psql','-U','ether','-d','ether',
        '-v','ON_ERROR_STOP=1','-q'],input=sql,text=True,capture_output=True)
    if result.returncode: raise SystemExit('isolated lab data seed failed (details withheld)')
    print('generated title, fixture sources and short-lived local playback delegation seeded; credentials saved privately')


if __name__=='__main__': main()
