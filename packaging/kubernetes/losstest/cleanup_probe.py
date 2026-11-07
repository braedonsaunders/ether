"""One scoped role+cleanup postcheck, inside the existing 20s runner budget."""
import argparse,datetime,hashlib,importlib.util,json,pathlib,re,subprocess,sys,time
SOURCE=pathlib.Path(__file__).resolve().parent
def load(name,path):
 s=importlib.util.spec_from_file_location(name,path);m=importlib.util.module_from_spec(s);s.loader.exec_module(m);return m
policy=load('cleanup_contract',SOURCE/'cleanup_contract.py')
ops=None
role_proof=None
def configure(backend_path,role_proof_path):
 global ops,role_proof
 ops=load('cleanup_ops',backend_path);role_proof=pathlib.Path(role_proof_path)
def now():return datetime.datetime.now(datetime.timezone.utc).isoformat()
def call(argv,end,**kwargs):
 remain=end-time.time()
 if remain<=0:raise TimeoutError('cleanup observation budget exhausted')
 return subprocess.run(argv,capture_output=True,text=True,timeout=min(2,max(.05,remain)),**kwargs)
def processes(lab,target,end):
 l=ops.LABS[lab];k=[l['state']+'/bin/kubectl','--kubeconfig',l['state']+'/kubeconfig','--request-timeout=2s','-n',l['ns']]
 result=call(k+['get','pods','-l','ether.plex/role=transcode','-o','json'],end);result.check_returncode();pods=json.loads(result.stdout)['items']
 if len(pods)!=2:raise ValueError('two worker pods required')
 tasks=[(p['spec']['nodeName'],next(c['containerID']for c in p['status']['containerStatuses']if c['name']=='ether').split('://',1)[1],None)for p in pods]
 if target:tasks.append((target['node'],target['containerID'].split('://',1)[1],target['ownedNodeContainerID']))
 inventory=[];count=0;seen=set()
 for node,cid,expected in tasks:
  if(node,cid)in seen:continue
  seen.add((node,cid));result=call(['docker','inspect',node],end);result.check_returncode();owned=json.loads(result.stdout)[0]
  if owned['Config'].get('Labels',{}).get('ether.lab')!=('ether035-multihost'if lab=='lab1'else'ether035-lab2')or(expected and expected!=owned['Id']):raise ValueError('owned node identity differs')
  result=call(['docker','exec',owned['Id'],'ctr','-n','k8s.io','tasks','ls'],end);result.check_returncode();active={x.split()[0]for x in result.stdout.splitlines()[1:]if x.split()}
  row={'node':node,'ownedNodeContainerID':owned['Id'],'containerID':cid,'taskAbsent':cid not in active,'processes':[]}
  if cid in active:
   result=call(['docker','exec',owned['Id'],'ctr','-n','k8s.io','tasks','ps',cid],end);result.check_returncode()
   for line in result.stdout.splitlines()[1:]:
    fields=line.split()
    if not fields or not fields[0].isdigit():continue
    result=call(['docker','exec',owned['Id'],'readlink','/proc/'+fields[0]+'/exe'],end)
    if result.returncode:raise ValueError('active task process inventory incomplete')
    exe=pathlib.PurePosixPath(result.stdout.strip()).name;row['processes'].append({'pid':int(fields[0]),'executable':exe});count+=int(exe.lower().startswith('ffmpeg'))
   if not row['processes']:raise ValueError('unverified active task inventory')
  inventory.append(row)
 return count,inventory

def retain_nonzero_rows(lab,session,out,end):
 """Reuse the bounded read-only full-row snapshot after a recorded failure."""
 if not re.fullmatch(r'[A-Za-z0-9._-]{1,128}',session):raise ValueError('invalid session')
 l=ops.LABS[lab]
 sql=f"BEGIN READ ONLY;SELECT json_build_object('observedAt',clock_timestamp(),'session','{session}','currentClaims',(SELECT json_agg(x)FROM(SELECT * FROM placement_claims WHERE session='{session}')x),'displacedClaims',(SELECT json_agg(x)FROM(SELECT * FROM placement_displaced_claims WHERE session='{session}')x),'displaced',(SELECT json_agg(x)FROM(SELECT * FROM placement_displaced WHERE session='{session}')x));COMMIT;"
 k=[l['state']+'/bin/kubectl','--kubeconfig',l['state']+'/kubeconfig','--request-timeout=1s','-n',l['ns'],'exec','-i',l['pg'],'-c','postgres','--','psql','-U','ether','-d','ether','-Atq','-v','ON_ERROR_STOP=1']
 result=call(k,end,input=sql);result.check_returncode();rows=json.loads(result.stdout)
 if rows.get('session')!=session:raise ValueError('row snapshot session differs')
 path=out/'cleanup-deadline-rows.private.json'
 # Set private permissions at creation, before writing any row content.
 import os
 fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
 with os.fdopen(fd,'w')as stream:json.dump(rows,stream,indent=2);stream.write('\n')

def main():
 p=argparse.ArgumentParser();p.add_argument('--lab',required=True);p.add_argument('--session',required=True);p.add_argument('--player',type=pathlib.Path,required=True);p.add_argument('--run-dir',type=pathlib.Path,required=True);p.add_argument('--backend',type=pathlib.Path,required=True);p.add_argument('--role-proof',type=pathlib.Path,required=True);a=p.parse_args();configure(a.backend,a.role_proof);started=now();wall_end=time.time()+18.5
 if not re.fullmatch(r'[A-Za-z0-9._-]{1,128}',a.session):raise ValueError('invalid session')
 context=json.loads((a.run_dir/'post-check-context.json').read_text());player=json.loads(a.player.read_text());anchor=policy.anchor(player,context);deadline=anchor+policy.BOUND_NS;target=None;frozen_at=None
 base={'session':a.session,'passed':True}
 if context['action']!='none':
  result=subprocess.run([sys.executable,str(role_proof),'--lab',a.lab,'--session',a.session,'--player',str(a.player),'--run-dir',str(a.run_dir)],capture_output=True,text=True,timeout=max(.05,wall_end-time.time()));(a.run_dir/'cleanup-base-role.stdout.json').write_text(result.stdout);(a.run_dir/'cleanup-base-role.stderr.log').write_text(result.stderr)
  # Command may need normal bounded RPC time; no process is left on timeout.
  if result.returncode:raise RuntimeError('base role proof failed: '+result.stdout[-500:])
  base=json.loads(result.stdout)
 proofpath=a.run_dir/'cleanup-contract-observed.json'
 while not proofpath.exists() and time.time()<wall_end:
  if(a.run_dir/'cleanup-observer-error.json').exists():raise ValueError('inline cleanup observation invalid')
  time.sleep(.05)
 if not proofpath.exists():raise ValueError('completed in-bound cleanup evidence unavailable')
 observed=json.loads(proofpath.read_text());evaluation=observed['evaluation']
 if observed['session']!=a.session:raise ValueError('cleanup proof session differs')
 proof={'session':a.session,'passed':base.get('passed')is True and evaluation.get('passed')is True,'cleanupContract':evaluation,'collectorStartedAt':observed['collectorStartedAt'],'target':observed['target'],'targetCapturedAt':observed['targetCapturedAt'],'samples':observed['samples'],'baseRoleProof':base,'prospectivePolicyVersion':policy.VERSION,'rawHistoricalArtifactsModified':False}
 for key in ['serverInterruptionUpperBoundSeconds','serverInterruptionUpperBoundVerified']:proof[key]=base.get(key)
 (a.run_dir/'cleanup-deadline-proof.json').write_text(json.dumps(proof,indent=2)+'\n');print(json.dumps({k:v for k,v in proof.items()if k not in ['samples','processInventories','baseRoleProof']}));return 0 if proof['passed']else 1
if __name__=='__main__':
 try:raise SystemExit(main())
 except Exception as error:print(json.dumps({'passed':False,'errorType':type(error).__name__,'error':str(error)}));raise SystemExit(1)
