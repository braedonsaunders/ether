"""Inline cleanup collection; backend supplies bounded SQL/process inventory calls."""
import json
import pathlib
import re
import time

class InlineCleanup:
 def __init__(self,config,out,backend):
  self.helper=backend;self.policy=backend.policy
  self.config=config;self.out=out;self.started=None;self.next=0;self.samples=[];self.done=False;self.target=None;self.frozen=None;self.error=None
 def step(self):
  helper=self.helper;policy=self.policy
  if self.done or time.monotonic()<self.next:return
  self.next=time.monotonic()+.4
  eventpath=self.out/'player/events.jsonl'
  if not eventpath.exists():return
  events=[]
  for line in eventpath.read_text().splitlines():
   try:events.append(json.loads(line))
   except ValueError:continue
  controls=[x for x in events if x.get('Kind')in('timeline','stop')]
  if not controls or controls[-1].get('Kind')!='stop':return
  timeline=self.out/'player/initial-timeline.json'
  if not timeline.exists():return
  session=json.loads(timeline.read_text())['session'];
  if not re.fullmatch(r'[A-Za-z0-9._-]{1,128}',session):raise ValueError('invalid session')
  player={'Session':session,'Attempts':controls}
  if self.started is None:self.started=helper.now()
  context={'session':session,'role':self.config['role'],'action':self.config['action']}
  pause=context['action']not in('none','force-delete')
  restore=self.out/'fault-restoration.json'
  if pause and restore.exists():context.update(json.loads(restore.read_text()))
  try:
   if context['action']!='none' and self.target is None:
    dispatch=json.loads((self.out/'fault-dispatch.json').read_text());selection=dispatch.get('targetSelection')or dispatch.get('target');raw=json.loads(pathlib.Path(selection['preFaultTargetRecord']).read_text());target=policy.exact_target({'role':context['role'],'target':raw['target']})
    if raw['session']!=session or policy.timestamp_ns(raw['capturedAt'])>policy.timestamp_ns(dispatch['dispatchedAt']):raise ValueError('pre-fault target was not retained before dispatch')
    inspected=policy.exact_target({'role':context['role'],'target':selection})
    if inspected!=target:raise ValueError('dispatch differs from frozen target')
    direct=dispatch.get('target')
    if direct and (direct['uid']!=target['uid']or direct['node']!=target['node']or [x['containerID']for x in direct['containers']if x['name']=='ether']!=[target['containerID']]):raise ValueError('actual deleted target differs')
    self.target=dict(target,ownedNodeContainerID=raw['target']['ownedNodeContainerID']);self.frozen=raw['capturedAt']
   self.observe(session,player,context,pause)
  except(Exception)as error:
   self.error=type(error).__name__+': '+str(error)
   (self.out/'cleanup-observer-error.json').write_text(json.dumps({'session':session,'error':self.error,'at':helper.now(),'passed':None},indent=2)+'\n')
   self.done=True
 def observe(self,session,player,context,pause):
  helper=self.helper;policy=self.policy
  l=helper.ops.LABS[self.config['_lab']];k=[l['state']+'/bin/kubectl','--kubeconfig',l['state']+'/kubeconfig','--request-timeout=2s','-n',l['ns'],'exec','-i',l['pg'],'-c','postgres','--','psql','-U','ether','-d','ether','-Atq','-v','ON_ERROR_STOP=1']
  budget=time.time()+2;sql=f"BEGIN READ ONLY;SELECT json_build_object('session',session,'observedAt',clock_timestamp(),'generation',generation,'retired',retired,'current',(SELECT count(*)FROM placement_claims WHERE session='{session}'),'displaced',(SELECT count(*)FROM placement_displaced_claims WHERE session='{session}'))FROM placement_sessions WHERE session='{session}';COMMIT;"
  query_started=helper.now();result=helper.call(k,budget,input=sql);result.check_returncode();sample=json.loads(result.stdout);sample.update(ffmpegCount=None,processInventoryVerified=False,queryStartedAt=query_started);deadline=None
  if not pause or context.get('restored')is True:deadline=policy.anchor(player,context)+policy.BOUND_NS
  if sample['retired']==1 and sample['current']==sample['displaced']==0 and (deadline is None or policy.timestamp_ns(sample['observedAt'])<=deadline):
   try:
    count,inventory=helper.processes(self.config['_lab'],self.target,min(budget,deadline/1e9)if deadline else budget);sample.update(ffmpegCount=count,processInventoryVerified=True);(self.out/('cleanup-processes-'+str(len(self.samples))+'.json')).write_text(json.dumps({'completedAt':helper.now(),'inventory':inventory},indent=2)+'\n')
   except(Exception)as error:sample['processInventoryError']=type(error).__name__
  sample['completedAt']=helper.now();self.samples.append(sample)
  (self.out/'cleanup-observations.json').write_text(json.dumps({'session':session,'startedAt':self.started,'target':self.target,'targetCapturedAt':self.frozen,'samples':self.samples},indent=2)+'\n')
  if deadline is None:return
  evaluation=policy.evaluate(session,player,context,self.target,self.frozen,self.samples,self.started)
  if evaluation['passed']is not None or(policy.timestamp_ns(sample['queryStartedAt'])>=deadline) or evaluation['status'].startswith('INVALID_LATE'):
   (self.out/'cleanup-contract-observed.json').write_text(json.dumps({'session':session,'context':context,'playerStop':player['Attempts'][-1],'target':self.target,'targetCapturedAt':self.frozen,'collectorStartedAt':self.started,'samples':self.samples,'evaluation':evaluation},indent=2)+'\n');self.done=True
   # Publish the immutable verdict first; capture causal rows within a separate
   # short read-only budget before the later case publisher can lose them.
   if evaluation.get('passed')is False:
    try:helper.retain_nonzero_rows(self.config['_lab'],session,self.out,time.time()+1.5)
    except Exception as error:
     (self.out/'cleanup-deadline-row-snapshot-error.json').write_text(json.dumps({'session':session,'at':helper.now(),'errorType':type(error).__name__,'verdictUnchanged':True},indent=2)+'\n')

