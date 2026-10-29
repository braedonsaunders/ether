#!/usr/bin/env bash
# Idempotently configure users, libraries and per-viewer state.
# Run on the Mac with tunnel.sh open. Content lives on gpu-workstation in
# ~/prism-reference-lab/media and is mounted read-only by all three servers.
# Seed sources: Blender Foundation open movies (CC BY 3.0) and The Scene / The
# Scene 2.0 from Internet Archive (CC BY-NC-ND 3.0); do not transcode those TV files.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
python3 - "$here/lab.env" <<'PY'
import json,sys,urllib.error,urllib.parse,urllib.request
E=dict(line.rstrip('\n').split('=',1) for line in open(sys.argv[1]) if '=' in line)
def call(base,path,method='GET',body=None,headers=None):
 h={'Accept':'application/json','Content-Type':'application/json'};h.update(headers or {})
 req=urllib.request.Request(base+path,data=None if body is None else json.dumps(body).encode(),headers=h,method=method)
 try:
  with urllib.request.urlopen(req,timeout=60) as r:
   b=r.read();return r.status,json.loads(b) if b and 'json' in r.headers.get('Content-Type','') else None
 except urllib.error.HTTPError as e: raise RuntimeError(f'{method} {path} returned HTTP {e.code}: {e.read().decode()[:250]}')
def jf_auth(base,owner,password,client):
 h={'Authorization':f'MediaBrowser Client="{client}", Device="Mac", DeviceId="prism-lab-seed", Version="1.0"'}
 _,a=call(base,'/Users/AuthenticateByName','POST',{'Username':owner,'Pw':password},h)
 h['Authorization']+=', Token="'+a['AccessToken']+'"';return h,a['User']['Id']
def ensure_mb(base,users,owner,owner_pw,adult,adult_pw,child,child_pw,client,emby=False):
 h,user_id=jf_auth(base,owner,owner_pw,client)
 if emby:
  h={'X-Emby-Authorization':f'MediaBrowser Client="{client}", Device="Mac", DeviceId="prism-lab-seed", Version="1.0"'}
  _,a=call(base,'/Users/AuthenticateByName','POST',{'Username':owner,'Pw':owner_pw},h);h['X-Emby-Token']=a['AccessToken'];user_id=a['User']['Id']
 _,rows=call(base,'/Users',headers=h);names={u['Name']:u for u in rows}
 for name,pw in ((adult,adult_pw),(child,child_pw)):
  if name not in names:
   _,u=call(base,'/Users/New','POST',{'Name':name},h);names[name]=u;print(client,'created user',name)
  if emby:
   call(base,f"/Users/{names[name]['Id']}/Password",'POST',{'Id':names[name]['Id'],'NewPw':pw,'ResetPassword':False},h)
  else:
   # Startup/API-created accounts can be rerun without changing existing passwords.
   pass
 if not emby:
  childrow=names.get(child)
  policy=dict(childrow.get('Policy') or {})
  policy['MaxParentalRating']=7
  call(base,f"/Users/{childrow['Id']}/Policy",'POST',policy,h)
 _,folders=call(base,'/Library/VirtualFolders',headers=h);existing={x.get('Name') for x in folders}
 for name,kind in (('Movies','movies'),('TV','tvshows')):
  if name not in existing:
   q=urllib.parse.urlencode({'name':name,'collectionType':kind,'paths':'/media/'+name})
   call(base,'/Library/VirtualFolders?'+q,'POST',headers=h);print(client,'created library',name)
 call(base,'/Library/Refresh','POST',headers=h)
 _,adult_auth=jf_auth(base,adult,adult_pw,client)
 # use viewer token, never the owner token, for state writes
 _,items=call(base,f'/Users/{adult_auth}/Items?Recursive=true&IncludeItemTypes=Movie,Episode&Fields=Path',headers=({'X-Emby-Token':h['X-Emby-Token']} if emby else {'Authorization':h['Authorization']}))
 return h,adult_auth,items
# The owner and accounts are created through each server's startup/admin APIs. JF 12.x
# auth tokens must be carried in MediaBrowser Authorization; X-Emby-Token returns 401.
jbase='http://127.0.0.1:18096'; jh,juid,jitems=ensure_mb(jbase,[],E['JELLYFIN_OWNER_USER'],E['JELLYFIN_OWNER_PASSWORD'],E['JELLYFIN_ADULT_USER'],E['JELLYFIN_ADULT_PASSWORD'],E['JELLYFIN_CHILD_USER'],E['JELLYFIN_CHILD_PASSWORD'],'Jellyfin Prism Seed')
ebase='http://127.0.0.1:18097/emby'; eh,euid,eitems=ensure_mb(ebase,[],E['EMBY_OWNER_USER'],E['EMBY_OWNER_PASSWORD'],E['EMBY_ADULT_USER'],E['EMBY_ADULT_PASSWORD'],E['EMBY_CHILD_USER'],E['EMBY_CHILD_PASSWORD'],'Emby Prism Seed',True)
def mb_state(base,headers,uid,items):
 rows=items.get('Items',[])
 chosen=[(next((x for x in rows if 'Big Buck Bunny' in x['Name']),None),'favorite'),(next((x for x in rows if 'The Scene S01E01' in x.get('Path','')),None),'played'),(next((x for x in rows if 'The Scene S01E02' in x.get('Path','')),None),'progress')]
 for item,kind in chosen:
  if not item: continue
  ticks=item.get('RunTimeTicks') or 600000000
  body={'ItemId':item['Id'],'IsFavorite':kind=='favorite','Played':kind=='played','PlayCount':1 if kind=='played' else 0,'PlaybackPositionTicks':ticks//20 if kind=='progress' else 0}
  route=(f"/Users/{uid}/Items/{item['Id']}/UserData" if base.endswith('/emby') else f"/UserItems/{item['Id']}/UserData?userId={uid}")
  call(base,route,'POST',body,headers)
 print('adult state updated for',sum(bool(i) for i,_ in chosen),'items')
mb_state(jbase,jh,juid,jitems); mb_state(ebase,eh,euid,eitems)
# Silo build-1810 uses native /api/v2 (not Jellyfin compatibility routes).
sbase='http://127.0.0.1:18098';_,s=call(sbase,'/api/v2/auth/login','POST',{'username':E['SILO_OWNER_USER'],'password':E['SILO_OWNER_PASSWORD']}); sh={'Authorization':'Bearer '+s['access_token']}
_,libs=call(sbase,'/api/v2/libraries',headers=sh); libnames={x['name'] for x in libs.get('items',[])}
for name,kind,path in (('Movies','movies','/mnt/media/Movies'),('TV','series','/mnt/media/TV')):
 if name not in libnames: call(sbase,'/api/v2/libraries','POST',{'name':name,'type':kind,'paths':[path]},sh);print('Silo created library',name)
_,info=call(sbase,'/api/v2/system/info',headers=sh);print('Silo native API',info.get('api_version','v2'))
PY
