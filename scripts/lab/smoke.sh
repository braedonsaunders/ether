#!/usr/bin/env bash
# Authenticate to each local reference server and query its discovery/home API.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
python3 - "$here/lab.env" <<'PY'
import json, sys, urllib.error, urllib.request
E={k:v for k,v in (line.rstrip('\n').split('=',1) for line in open(sys.argv[1]) if '=' in line)}
def request(base,path,method='GET',body=None,headers=None):
    h={'Accept':'application/json','Content-Type':'application/json'}
    if headers: h.update(headers)
    data=None if body is None else json.dumps(body).encode()
    req=urllib.request.Request(base+path,data=data,headers=h,method=method)
    try:
        with urllib.request.urlopen(req,timeout=20) as r: return r.status,json.loads(r.read()) if r.headers.get('Content-Type','').startswith('application/json') else None
    except urllib.error.HTTPError as e: raise RuntimeError(f'{base}{path} returned HTTP {e.code}')
def report(label,status): print(f'{label}: HTTP {status}')
# Jellyfin 12 removed X-Emby-Token compatibility; pass token in MediaBrowser auth.
jf='http://127.0.0.1:18096'
h={'Authorization':'MediaBrowser Client="Prism smoke", Device="Mac", DeviceId="prism-lab-smoke", Version="1.0"'}
s,d=request(jf,'/Users/AuthenticateByName','POST',{'Username':E['JELLYFIN_ADULT_USER'],'Pw':E['JELLYFIN_ADULT_PASSWORD']},h); report('jellyfin sign-in',s)
h['Authorization']+=', Token="'+d['AccessToken']+'"'
s,_=request(jf,f"/Users/{d['User']['Id']}/Views",headers=h); report('jellyfin library views',s)
# Emby native auth and per-user views.
e='http://127.0.0.1:18097/emby'; h={'X-Emby-Authorization':'MediaBrowser Client="Prism smoke", Device="Mac", DeviceId="prism-lab-smoke", Version="1.0"'}
s,d=request(e,'/Users/AuthenticateByName','POST',{'Username':E['EMBY_ADULT_USER'],'Pw':E['EMBY_ADULT_PASSWORD']},h); report('emby sign-in',s)
h['X-Emby-Token']=d['AccessToken']; s,_=request(e,f"/Users/{d['User']['Id']}/Views",headers=h); report('emby library views',s)
# Silo native server API is v2 in build-1810; compatibility API stays disabled.
s,d=request('http://127.0.0.1:18098','/api/v2/auth/login','POST',{'username':E['SILO_OWNER_USER'],'password':E['SILO_OWNER_PASSWORD']}); report('silo sign-in',s)
s,_=request('http://127.0.0.1:18098','/api/v2/account/me',headers={'Authorization':'Bearer '+d['access_token']}); report('silo account',s)
PY
