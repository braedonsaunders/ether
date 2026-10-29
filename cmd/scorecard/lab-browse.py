#!/usr/bin/env python3
"""Opt-in preliminary GET timings; read lab passwords from stdin, never disk."""
import json, subprocess, sys, urllib.request, urllib.error, urllib.error

secrets = json.load(sys.stdin)

def request(base, path, token=None, body=None):
    authorization = 'MediaBrowser Client="Swiftfin", Device="Scorecard", DeviceId="scorecard-browse", Version="1.0"'
    if token: authorization += ', Token="' + token + '"'
    headers = {'Authorization': authorization, 'X-Emby-Authorization': authorization, 'X-Emby-Token': token or '', 'Content-Type': 'application/json'}
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + path, data=data, headers=headers)
    with urllib.request.urlopen(req, timeout=30) as response:
        return json.loads(response.read() or b'{}')

def login(base, user, password):
    result = request(base, '/Users/AuthenticateByName', body={'Username': user, 'Pw': password})
    return result['AccessToken'], result['User']['Id']

all_results = {}
cleanup_failures = []
for protocol, reference, ether in [('jellyfin','http://127.0.0.1:8096','http://127.0.0.1:18096'),('emby','http://127.0.0.1:8097/emby','http://127.0.0.1:18097')]:
    tokens = []
    try:
        rt, ru = login(reference, secrets[protocol]['user'], secrets[protocol]['password']); tokens.append((reference, rt))
        et, eu = login(ether, secrets['fixture']['user'], secrets['fixture']['password']); tokens.append((ether, et))
        def target(base, token, path):
            return {'url': base + path, 'headers': {'Authorization': 'MediaBrowser Client="Swiftfin", Device="Scorecard", DeviceId="scorecard-browse", Version="1.0", Token="'+token+'"', 'X-Emby-Token':token, 'Accept':'application/json'}}
        scenarios=[]
        for name, rp, ep in [('views',f'/Users/{ru}/Views',f'/Users/{eu}/Views'),('resume',f'/Users/{ru}/Items/Resume',f'/Users/{eu}/Items/Resume'),('next_up',f'/Shows/NextUp?UserId={ru}',f'/Shows/NextUp?UserId={eu}'),('sorted_movies',f'/Users/{ru}/Items?Recursive=true&IncludeItemTypes=Movie&SortBy=SortName&StartIndex=0&Limit=20',f'/Users/{eu}/Items?Recursive=true&IncludeItemTypes=Movie&SortBy=SortName&StartIndex=0&Limit=20')]:
            scenarios.append({'name':name,'reference':target(reference,rt,rp),'ether':target(ether,et,ep)})
        config={'samples':100,'concurrency':[1,10,50],'scenarios':scenarios}
        proc=subprocess.run(['docker','exec','-i','prism-scorecard-qsb','scorecard','-config','/dev/stdin'],input=json.dumps(config),text=True,capture_output=True,check=True)
        all_results[protocol]=json.loads(proc.stdout)
    finally:
        for index,(base,token) in enumerate(tokens):
            try: request(base,'/Sessions/Logout',token,body={})
            except Exception as exc:
                if isinstance(exc,urllib.error.HTTPError):exc.close()
                failure={'protocol':protocol,'target':'reference' if index==0 else 'ether'}
                cleanup_failures.append(failure);print('logout failed for '+protocol+' '+failure['target'],file=sys.stderr)
all_results['_cleanup_failures']=cleanup_failures
print(json.dumps(all_results,indent=2))
if cleanup_failures: sys.exit(1)
