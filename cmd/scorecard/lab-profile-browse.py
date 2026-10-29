#!/usr/bin/env python3
"""Run only recorded profile GETs with matching response projections; credentials on stdin."""
import json, subprocess, sys, urllib.request, urllib.error
secrets=json.load(sys.stdin)

def request(base,path,token='',body=None):
    auth='MediaBrowser Client="Scorecard", Device="Lab", DeviceId="scorecard-preflight", Version="1"'
    if token: auth+=', Token="'+token+'"'
    headers={'Authorization':auth,'X-Emby-Authorization':auth,'X-Emby-Token':token,'Content-Type':'application/json'}
    with urllib.request.urlopen(urllib.request.Request(base+path,data=json.dumps(body).encode() if body is not None else None,headers=headers),timeout=30) as response:
        return json.loads(response.read() or b'{}')

def login(base,creds):
    value=request(base,'/Users/AuthenticateByName',body={'Username':creds['user'],'Pw':creds['password']})
    return value['AccessToken'],value['User']['Id']

def projection(response):
    items=response.get('Items')
    if not isinstance(items,list):raise ValueError('response has no item list')
    # Stable user-visible identity projection; other response fields can differ.
    rows=sorted((x.get('Name',''),x.get('Type',''),x.get('CollectionType',''),x.get('IndexNumber') or 0,x.get('ParentIndexNumber') or 0) for x in items)
    return {'total':response.get('TotalRecordCount',len(items)),'items':rows}

cleanup_failures=[]
output={'caveat':'Preflight compares visible item projections; views may match by collection type despite different configured names. Metadata, viewer policies and bytes are not fully aligned, so timings remain exploratory.','protocols':{}}
for protocol,reference,ether,profile,viewProbe,spec in [
 ('jellyfin','http://127.0.0.1:8096','http://127.0.0.1:18096','/profiles/jellyfin/swiftfin.json','user views','/specs/jellyfin-openapi-12.4.json'),
 ('emby','http://127.0.0.1:8097/emby','http://127.0.0.1:18097','/profiles/emby/emby-sdk.json','views','/specs/emby-openapi-v2.json')]:
    logins=[]
    try:
        rt,ru=login(reference,secrets[protocol]);logins.append((reference,rt))
        et,eu=login(ether,secrets['fixture']);logins.append((ether,et))
        selected=[];preflight=[]
        for name,probe,rpath,epath in [('views',viewProbe,f'/Users/{ru}/Views',f'/Users/{eu}/Views'),('resume','resume',f'/Users/{ru}/Items/Resume',f'/Users/{eu}/Items/Resume'),('next_up','next up',f'/Shows/NextUp?UserId={ru}',f'/Shows/NextUp?UserId={eu}')]:
            rp=projection(request(reference,rpath,rt));ep=projection(request(ether,epath,et));match=rp==ep
            alias_equal=False
            if name=='views' and not match:
                def view_types(p): return sorted((row[1],row[2]) for row in p['items'])
                alias_equal=rp['total']==ep['total'] and view_types(rp)==view_types(ep)
                match=alias_equal
            preflight.append({'name':name,'reference':rp,'ether':ep,'projection_equal':rp==ep,'view_type_only_equal':alias_equal})
            if not match:continue
            def target(base,uid,token):
                headers={'X-Emby-Token':token}
                if protocol=='jellyfin': headers['Authorization']='MediaBrowser Client="Swiftfin", Device="Apple TV", DeviceId="scorecard-profile", Version="1.0", Token="'+token+'"'
                return {'url':base,'variables':{'user_id':uid,'device_id':'scorecard-profile','access_token':token},'headers':headers}
            selected.append({'name':name,'profile_file':profile,'flow':'home','probe':probe,'protocol':protocol,'spec_file':spec,'reference':target(reference,ru,rt),'ether':target(ether,eu,et)})
        results=[]
        if selected:
            config={'samples':100,'concurrency':[1,10,50],'scenarios':selected}
            proc=subprocess.run(['docker','exec','-i','prism-scorecard-qsb','scorecard','-config','/dev/stdin'],input=json.dumps(config),text=True,capture_output=True,check=True)
            results=json.loads(proc.stdout)
        output['protocols'][protocol]={'preflight':preflight,'results':results}
    finally:
        for index,(base,token) in enumerate(logins):
            try:request(base,'/Sessions/Logout',token,{})
            except Exception as exc:
                if isinstance(exc,urllib.error.HTTPError):exc.close()
                failure={'protocol':protocol,'target':'reference' if index==0 else 'ether'}
                cleanup_failures.append(failure);print('logout failed for '+protocol+' '+failure['target'],file=sys.stderr)
output['cleanup_failures']=cleanup_failures
print(json.dumps(output,indent=2))
if cleanup_failures:sys.exit(1)
