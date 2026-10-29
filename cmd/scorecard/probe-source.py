#!/usr/bin/env python3
"""Inspect fixture playback plans without asserting a client decision; secrets on stdin."""
import json, sys, urllib.request, urllib.error
credentials = json.load(sys.stdin)

def query(base, path, token='', body=None):
    auth = 'MediaBrowser Client="Scorecard", Device="Lab", DeviceId="scorecard-probe", Version="1"'
    if token: auth += ', Token="' + token + '"'
    headers = {'Authorization': auth, 'X-Emby-Authorization': auth, 'X-Emby-Token': token, 'Content-Type': 'application/json'}
    request = urllib.request.Request(base + path, data=json.dumps(body).encode() if body is not None else None, headers=headers)
    with urllib.request.urlopen(request, timeout=120) as response:
        return json.load(response)

cleanup_failures=[]
for label, base, user in [('reference','http://127.0.0.1:8096',credentials),('ether','http://127.0.0.1:18096',{'username':'fixture-viewer','password':'fixture-password'})]:
    account = query(base, '/Users/AuthenticateByName', body={'Username':user['username'],'Pw':user['password']})
    token, uid = account['AccessToken'], account['User']['Id']
    try:
        listing = query(base, '/Users/'+uid+'/Items?Recursive=true&IncludeItemTypes=Movie&Fields=Path', token)
        movies=[x for x in listing['Items'] if x.get('Name') == 'Big Buck Bunny']
        if label == 'reference': movies=[x for x in movies if '1080p' in x.get('Path','')]
        if len(movies)!=1: raise RuntimeError(label+' expected exactly one matching movie')
        plan=query(base,'/Items/'+movies[0]['Id']+'/PlaybackInfo',token,{'UserId':uid,'IsPlayback':False})
        sources=plan.get('MediaSources',[])
        print(label,'media sources',len(sources),'declared direct play',sum(bool(x.get('SupportsDirectPlay')) for x in sources),'declared transcode',sum(bool(x.get('SupportsTranscoding')) for x in sources))
    finally:
        try: query(base, '/Sessions/Logout', token, {})
        except Exception as exc:
            if isinstance(exc,urllib.error.HTTPError):exc.close()
            cleanup_failures.append(label);print('logout failed for '+label,file=sys.stderr)
if cleanup_failures: sys.exit(1)
