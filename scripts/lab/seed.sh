#!/usr/bin/env bash
# Idempotently configure users, libraries and per-viewer state.
# Run on the Mac with tunnel.sh open. Content lives on gpu-workstation in
# ~/prism-reference-lab/media and is mounted read-only by all three servers.
# Seed sources: Blender Foundation open movies (CC BY 3.0) and The Scene / The
# Scene 2.0 from Internet Archive (CC BY-NC-ND 3.0); do not transcode those TV files.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
LAB_PORT_BASE="${LAB_PORT_BASE:-18096}"
python3 - "$here/lab.env" "$LAB_PORT_BASE" <<'PY'
import json,sys,urllib.error,urllib.parse,urllib.request
E=dict(line.rstrip('\n').split('=',1) for line in open(sys.argv[1]) if '=' in line)
port_base=int(sys.argv[2])
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
jbase=f'http://127.0.0.1:{port_base}'; jh,juid,jitems=ensure_mb(jbase,[],E['JELLYFIN_OWNER_USER'],E['JELLYFIN_OWNER_PASSWORD'],E['JELLYFIN_ADULT_USER'],E['JELLYFIN_ADULT_PASSWORD'],E['JELLYFIN_CHILD_USER'],E['JELLYFIN_CHILD_PASSWORD'],'Jellyfin Prism Seed')
ebase=f'http://127.0.0.1:{port_base+1}/emby'; eh,euid,eitems=ensure_mb(ebase,[],E['EMBY_OWNER_USER'],E['EMBY_OWNER_PASSWORD'],E['EMBY_ADULT_USER'],E['EMBY_ADULT_PASSWORD'],E['EMBY_CHILD_USER'],E['EMBY_CHILD_PASSWORD'],'Emby Prism Seed',True)
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
sbase=f'http://127.0.0.1:{port_base+2}'
try:
 _,s=call(sbase,'/api/v2/auth/login','POST',{'username':E['SILO_OWNER_USER'],'password':E['SILO_OWNER_PASSWORD']})
except RuntimeError:
 call(sbase,'/api/v2/auth/setup','POST',{'username':E['SILO_OWNER_USER'],'email':'labowner@example.test','password':E['SILO_OWNER_PASSWORD'],'create_default_profile':True,'default_profile_name':'Owner'})
 _,s=call(sbase,'/api/v2/auth/login','POST',{'username':E['SILO_OWNER_USER'],'password':E['SILO_OWNER_PASSWORD']})
sh={'Authorization':'Bearer '+s['access_token']}
_,accounts=call(sbase,'/api/v2/admin/users',headers=sh);known={x.get('username'):x for x in accounts.get('items',[])}
for name,email,key in ((E['SILO_ADULT_USER'],'adult@example.test','SILO_ADULT_PASSWORD'),(E['SILO_CHILD_USER'],'child@example.test','SILO_CHILD_PASSWORD')):
 if name not in known:
  call(sbase,'/api/v2/admin/users','POST',{'username':name,'email':email,'password':E[key],'role':'user','create_default_profile':True})
  print('Silo created user',name)
_,libs=call(sbase,'/api/v2/libraries',headers=sh);libnames={x['name'] for x in libs.get('items',[])}
for name,kind,path in (('Movies','movies','/mnt/media/Movies'),('TV','series','/mnt/media/TV')):
 if name not in libnames: call(sbase,'/api/v2/libraries','POST',{'name':name,'type':kind,'paths':[path]},sh);print('Silo created library',name)
def silo_viewer(name,password):
 _,auth=call(sbase,'/api/v2/auth/login','POST',{'username':name,'password':password});h={'Authorization':'Bearer '+auth['access_token']}
 _,rows=call(sbase,'/api/v2/profiles',headers=h);h['X-Profile-Id']=next((x['id'] for x in rows.get('items',[]) if x.get('is_primary')),rows['items'][0]['id'])
 return h
child_headers=silo_viewer(E['SILO_CHILD_USER'],E['SILO_CHILD_PASSWORD'])
_,child_profiles=call(sbase,'/api/v2/profiles',headers=child_headers);child_id=child_headers['X-Profile-Id'];child=next(x for x in child_profiles['items'] if x['id']==child_id)
if not child.get('is_child') or child.get('max_content_rating')!='PG' or child.get('max_advisory_age')!=12:
 call(sbase,f'/api/v2/profiles/{child_id}','PATCH',{'is_child':True,'max_content_rating':'PG','max_advisory_age':12},child_headers)
adult_headers=silo_viewer(E['SILO_ADULT_USER'],E['SILO_ADULT_PASSWORD'])
call(sbase,'/api/v2/favorites/movie-tmdb-10378','PUT',headers=adult_headers)
call(sbase,'/api/v2/watched/episode-tvdb-78927-1-1','POST',headers=adult_headers)
# The H.264 episode can require an unavailable audio adaptation on some image
# builds. Keep the watched episode above for Next Up, and use the known-playable
# Big Buck Bunny H.264 version for the adult resume marker.
_,versions=call(sbase,'/api/v2/catalog/items/movie-tmdb-10378/versions',headers=adult_headers);version=next((x for x in versions.get('items',[]) if x.get('codec_video')=='h264'),versions['items'][0])
_,cap=call(sbase,'/api/v2/playback/capabilities',headers=adult_headers)
import uuid
body={'installation_id':cap['installation_id'],'protocol_version':3,'client_features':['playback_plan_v3'],'file_id':str(version['file_id']),'profile_id':adult_headers['X-Profile-Id'],'playback_attempt_id':str(uuid.uuid4()),'quality_preference':'auto','subtitle_fidelity_preference':'compatible','client_capabilities':{'video_evidence':'exact','audio_evidence':'exact','codecs_video':['h264'],'codecs_video_hardware':['h264'],'codecs_audio':['mp3','aac'],'containers':['mp4'],'hdr':False,'max_resolution':'1080p','video_decode':[{'codec':'h264','profiles':['high'],'levels':[41],'bit_depths':[8],'max_width':1920,'max_height':1080,'max_frame_rate':60,'max_bitrate_kbps':20000,'hardware':True}]},'client_playback_context':{'protocol_version':3,'form_factor':'phone','app_version':'1.0.0','app_build':'seed','device':{},'output':{},'deliveries':{'original_http':{'enabled':True,'supported_on_device':True,'containers':['mp4'],'video_codecs':['h264'],'audio_decode_codecs':['mp3','aac'],'audio_passthrough_codecs':[],'subtitles':{'embedded_text':True,'sidecar_text':True,'ass_styling':False,'embedded_bitmap':False,'sidecar_bitmap':False,'font_attachments':False},'features':[],'auth_header_refresh':True,'validated_claims':[],'transformations':[]}}}}
_,opened=call(sbase,'/api/v2/playback/start','POST',body,adult_headers)
if opened.get('outcome')!='playable' or not opened.get('session_id'): raise RuntimeError('Silo adult progress seed did not open playback')
session=opened['session_id'];call(sbase,f'/api/v2/playback/{session}/progress','POST',{'installation_id':cap['installation_id'],'sequence':1,'position':60,'is_paused':False},adult_headers)
call(sbase,f'/api/v2/playback/{session}','DELETE',{'installation_id':cap['installation_id'],'stop_id':str(uuid.uuid4()),'is_paused':False,'position':60,'sequence':2},adult_headers)
_,info=call(sbase,'/api/v2/system/info',headers=sh);print('Silo native API',info.get('api_version','v2'),'adult state updated')
PY
