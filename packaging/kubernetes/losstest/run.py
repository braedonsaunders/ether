#!/usr/bin/env python3
"""Bounded isolated-lab loss run. JSON config uses explicit argv, never a shell.

Required: url, tokenFile, ratingKey, hlsplay, role, action, phase.
Pod deletion additionally requires kubeconfig, namespace, pod. Other actions
require lossArgv and restoreArgv; use immutable IDs from the multihost inventory.
Optional producerEvidence names a JSON file from a trusted observer containing
{session, generation, segmentIndex, producedAt} rows (UTC RFC3339). Client receipt
latency is reported separately and never called server production interruption.
"""
import argparse
import datetime as dt
import hashlib
import json
import os
import signal
from pathlib import Path
import subprocess
import time


def stamp():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def parsed(s):
    return dt.datetime.fromisoformat(s.replace('Z', '+00:00')).timestamp()


def control_summary(controls):
    terminal_stop = bool(controls) and controls[-1].get('Kind') == 'stop'
    stops = [attempt for attempt in controls if attempt.get('Kind') == 'stop']
    status = stops[-1].get('Status') if stops else None
    failed = sum(not 200 <= attempt.get('Status', 0) < 300 for attempt in controls)
    clean = bool(controls) and failed == 0
    return {'stopStatus': status, 'failedRequests': failed, 'requests': len(controls),
            'stopPassed': terminal_stop and status is not None and 200 <= status < 300,
            'allRequestsPassed': clean,
            'passed': clean and terminal_stop and status is not None and 200 <= status < 300}


def media_control_passed(media_passed, action_succeeded, control):
    return bool(media_passed and action_succeeded and control['allRequestsPassed'] and control['stopPassed'])


def qualify_reference(path):
    report = json.loads(Path(path).read_text())
    required = ('IDRStart', 'ClosedGOP', 'PTSContinuity', 'CleanDecode', 'AudioContinuity', 'GridDurations')
    checks = report.get('Continuity', {}).get('Report', {})
    failed = [name for name in required if checks.get(name, {}).get('Pass') is not True]
    controls = [a for a in report.get('Attempts', []) if a.get('Kind') in ('timeline', 'stop')]
    if (report.get('PlaybackPassed') is not True or failed
            or not control_summary(controls)['passed']):
        raise ValueError('reference qualification failed before launch: '+(', '.join(failed) or 'playback/control'))
    return report


def boundary_ready(path, session, index, segments):
    if not path.exists():
        return None
    ready = json.loads(path.read_text())
    if (ready.get('session') != session or ready.get('index') != index
            or ready.get('lastReceivedIndex') != index-1
            or not isinstance(ready.get('playlistSHA256'), str)
            or len(ready['playlistSHA256']) != 64):
        raise RuntimeError('request barrier differs from session/index/initial playlist')
    timeline = json.loads((path.parent.parent/'player'/'initial-timeline.json').read_text())
    if timeline.get('session') != session or timeline.get('playlistSHA256') != ready['playlistSHA256']:
        raise RuntimeError('barrier differs from retained initial authored timeline')
    indices = [s['Index'] for s in segments]
    if len(indices) > index or indices != list(range(len(indices))):
        raise RuntimeError('request barrier has excess or noncontiguous received prefix')
    # ready.json can appear after this poll's events snapshot. A valid shorter
    # prefix is pending; the next iteration re-reads events after the marker.
    if len(indices) < index:
        return None
    return ready


def release_boundary(directory, ready, target, proof):
    node = target['barrierNodeInventory']
    old = [c['containerID'] for c in target['containers'] if c['name'] == 'ether']
    if (not old or proof.get('session') != ready['session']
            or proof.get('index') != ready['index']
            or proof.get('oldPodUID') != target['uid']
            or proof.get('oldContainerIDs') != old
            or proof.get('originalTasksAbsent') is not True
            or proof.get('ownedNodeContainerID') != node['ownedNodeContainerID']
            or proof.get('kubernetesNode') != target['node']
            or proof.get('kubernetesNode') != node['kubernetesNode']
            or proof.get('kubernetesNodeUID') != node['kubernetesNodeUID']):
        raise RuntimeError('barrier requires exact original-task absence proof')
    data = (json.dumps(proof, sort_keys=True)+'\n').encode()
    digest = hashlib.sha256(data).hexdigest()
    (directory/'fault-proof.json').write_bytes(data)
    release = dict(session=ready['session'], index=ready['index'],
                   playlistSHA256=ready['playlistSHA256'], proofSHA256=digest,
                   faultVerified=True, releasedAt=stamp())
    temp = directory/'release.tmp'
    temp.write_text(json.dumps(release)+'\n')
    temp.rename(directory/'release.json')
    return release


def read_events(path):
    rows = []
    if path.exists():
        for line in path.read_text().splitlines():
            try:
                rows.append(json.loads(line))
            except json.JSONDecodeError:
                pass  # Writer may be in the middle of its last row.
    return rows


def bind_boundary_node(target, selection):
    node = selection.get('barrierNodeInventory', {})
    if (node.get('kubernetesNode') != target['node']
            or not node.get('kubernetesNodeUID')
            or not isinstance(node.get('ownedNodeContainerID'), str)
            or len(node['ownedNodeContainerID']) != 64):
        raise RuntimeError('boundary requires trusted original-node incarnation inventory')
    target['barrierNodeInventory'] = dict(node)


def sample_host_load(out):
    begin = time.monotonic()
    row = dict(observedAt=stamp(), logicalCPUs=os.cpu_count())
    for name, path in [('loadavg', '/proc/loadavg'),
                       ('cpuPressure', '/proc/pressure/cpu'),
                       ('ioPressure', '/proc/pressure/io'),
                       ('memoryPressure', '/proc/pressure/memory')]:
        try:
            row[name] = Path(path).read_text().strip()
        except OSError:
            row[name] = None
    row['captureSeconds'] = time.monotonic()-begin
    with (out/'host-load.jsonl').open('a') as log:
        log.write(json.dumps(row)+'\n')


def command(argv, **kw):
    child = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                             start_new_session=True, **kw)
    try:
        stdout, stderr = child.communicate(timeout=20)
    except subprocess.TimeoutExpired:
        # The process group was created by this call and contains only this
        # explicit adapter and its children; no shared daemon is signalled.
        os.killpg(child.pid, signal.SIGKILL)
        child.communicate()
        return subprocess.CompletedProcess(argv, 124, b'', b'command exceeded bound')
    except BaseException:
        os.killpg(child.pid, signal.SIGKILL)
        child.communicate()
        raise
    return subprocess.CompletedProcess(argv, child.returncode, stdout, stderr)


def run(c, out):
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    role, action, phase = c['role'], c['action'], c['phase']
    if role not in ('controller', 'source', 'transcode', 'delivery'):
        raise ValueError('invalid role')
    if action not in ('none', 'force-delete', 'container-kill', 'pause', 'partition', 'host-stop'):
        raise ValueError('invalid action')
    if phase not in ('early', 'steady', 'state', 'boundary'):
        raise ValueError('invalid phase')
    if phase == 'state' and not c.get('faultTriggerArgv'):
        raise ValueError('state phase requires an observed-state trigger')
    if action not in ('none', 'force-delete') and not (c.get('targetArgv') or c.get('lossWrapperArgv') or (c.get('lossArgv') and c.get('restoreArgv'))):
        raise ValueError('loss and restoration argv required')
    if phase == 'boundary':
        index = c.get('pauseBeforeIndex')
        if (role != 'transcode' or action != 'force-delete' or type(index) is not int or index < 1
                or not isinstance(c.get('barrierFaultProofArgv'), list) or not c['barrierFaultProofArgv']
                or not all(isinstance(x, str) and x for x in c['barrierFaultProofArgv'])
                or not c.get('reference') or not c.get('targetArgv') or c.get('faultTriggerArgv')):
            raise ValueError('boundary phase requires transcode deletion, positive index, independent reference and task-absence adapter')
    barrier_dir = out/'segment-barrier'
    window = c.get('windowSeconds', 90)
    if not 15 <= window <= 3600:
        raise ValueError('window must be 15..3600 seconds')
    session = c.get('session') or 'loss-' + os.urandom(12).hex()
    argv = [c['hlsplay'], '-url', c['url'],
            '-rating-key', str(c['ratingKey']), '-output', str(out/'player'),
            '-session', session, '-window', f'{window}s',
            '-buffer', f"{c.get('bufferSeconds', 30)}s"]
    argv += ['-tries', str(c.get('tries', 8)),
             '-timeout', f"{c.get('requestTimeoutSeconds', 5)}s",
             '-backoff', f"{c.get('backoffSeconds', 0.2)}s"]
    for key, flag in [('decisionOrigin', '-decision-origin'), ('startOrigin', '-start-origin'), ('stopOrigin', '-stop-origin'), ('tokenFile', '-token-file'), ('credentialsFile', '-credentials-file'), ('protocol', '-protocol'), ('prefix', '-prefix'), ('deviceID', '-device-id'), ('host', '-host'), ('ffmpeg', '-ffmpeg'), ('ffprobe', '-ffprobe'),
                      ('reference', '-reference'), ('producerEvidence', '-producer-evidence'), ('maxVideoBitrate', '-max-video-bitrate'), ('videoResolution', '-video-resolution')]:
        if c.get(key):
            argv += [flag, str(c[key])]
    for key, flag in [('reencodedAudio', '-reencoded-audio'), ('skipDecision', '-skip-decision'), ('directStream', '-direct-stream'), ('directPlay', '-direct-play')]:
        if c.get(key):
            argv += [flag]
    if phase == 'boundary':
        argv += ['-segment-barrier-dir', str(barrier_dir), '-pause-before-index', str(c['pauseBeforeIndex'])]
    k = ['kubectl', '--kubeconfig', c.get('kubeconfig', ''), '--request-timeout=10s',
         '-n', c.get('namespace', '')]
    target = None
    if action == 'force-delete' and not c.get('targetArgv'):
        result = command(k+['get', 'pod', c['pod'], '-o', 'json'])
        if result.returncode:
            raise RuntimeError('target inspection failed')
        pod = json.loads(result.stdout)
        target = {'name': pod['metadata']['name'], 'uid': pod['metadata']['uid'],
                  'node': pod['spec']['nodeName'], 'containers': pod['status'].get('containerStatuses', [])}
    report = dict(session=session, role=role, action=action, phase=phase,
                  startedAt=stamp(), target=target, killAt=None,
                  hostLoadEvidence=dict(file='host-load.jsonl', nominalIntervalSeconds=1),
                  retryPolicy=dict(maxAttempts=c.get('tries', 8),
                                   requestTimeoutSeconds=c.get('requestTimeoutSeconds', 5),
                                   backoffSeconds=c.get('backoffSeconds', 0.2)),
                  serverInterruptionSeconds=None, serverInterruptionVerified=False,
                  clientFirstPostLossReceiptSeconds=None, actionSucceeded=action == 'none')
    process = None
    fault_process = None
    fault_handles = []
    restoration_needed = False
    next_trigger_poll = 0.0
    next_load_sample = 0.0
    try:
        if c.get('reference'):
            qualify_reference(c['reference'])
        with (out/'player.log').open('w') as log:
            process = subprocess.Popen(argv, stdout=log, stderr=log)
            deadline = time.monotonic()+window+150
            injected = action == 'none'
            while process.poll() is None and time.monotonic() < deadline:
                if time.monotonic() >= next_load_sample:
                    sample_host_load(out)
                    next_load_sample = time.monotonic()+1
                if fault_process is not None and fault_process.poll() is not None:
                    raise RuntimeError('fault wrapper exited before observation completed')
                events = out/'player'/'events.jsonl'
                rows = read_events(events)
                boundary_path = barrier_dir/'ready.json'
                # Observe the marker first, then the events it follows. This also
                # observes a MediaBrowser's admitted session before binding it.
                boundary_visible = phase == 'boundary' and boundary_path.exists()
                if boundary_visible:
                    rows = read_events(events)
                server_sessions = [r['session'] for r in rows if r.get('kind') == 'session']
                if server_sessions:
                    session = server_sessions[-1]
                    report['session'] = session
                segments = [r for r in rows if 'ReceivedAt' in r]
                playing = [r for r in rows if r.get('kind') == 'playing']
                # Early = 2s after first received segment, before 10s of media.
                # Steady = buffered playback established plus 10s of play time.
                ready = (phase == 'early' and segments and time.time()-parsed(segments[0]['ReceivedAt']) >= 2)
                ready = ready or (phase == 'steady' and playing and time.time()-parsed(playing[0]['wallTime']) >= c.get('steadySeconds', 10))
                if phase == 'state':
                    ready = bool(playing)
                if phase == 'boundary' and not injected:
                    barrier = boundary_ready(boundary_path, session, c['pauseBeforeIndex'], segments) if boundary_visible else None
                    ready = barrier is not None
                    if ready:
                        report['requestBarrier'] = barrier
                if ready and not injected and c.get('faultTriggerArgv'):
                    if time.monotonic() < next_trigger_poll:
                        ready = False
                    else:
                        next_trigger_poll = time.monotonic()+max(0.2, c.get('triggerPollSeconds', 1))
                        trigger = command([x.replace('{session}', session) for x in c['faultTriggerArgv']])
                        if trigger.returncode:
                            raise RuntimeError('observed-state trigger failed')
                        observed = json.loads(trigger.stdout)
                        if not isinstance(observed.get('ready'), bool):
                            raise RuntimeError('trigger must return a boolean ready field')
                        with (out/'trigger-events.jsonl').open('a') as trigger_log:
                            trigger_log.write(json.dumps(dict(at=stamp(), observation=observed))+'\n')
                        ready = observed['ready']
                        if ready:
                            report['faultTriggerEvidence'] = observed
                if ready and not injected:
                    injected = True
                    if c.get('targetArgv'):
                        selected = command([x.replace('{session}', session) for x in c['targetArgv']])
                        if selected.returncode:
                            raise RuntimeError('active target lookup failed')
                        selection = json.loads(selected.stdout)
                        report['targetSelection'] = selection
                        for key in ('pod', 'lossArgv', 'restoreArgv', 'lossWrapperArgv'):
                            if key in selection:
                                c[key] = selection[key]
                        if action == 'force-delete':
                            inspected = command(k+['get', 'pod', c['pod'], '-o', 'json'])
                            if inspected.returncode:
                                raise RuntimeError('active pod inspection failed')
                            pod = json.loads(inspected.stdout)
                            target = {'name':pod['metadata']['name'], 'uid':pod['metadata']['uid'],
                                      'node':pod['spec']['nodeName'], 'containers':pod['status'].get('containerStatuses', [])}
                            if selection.get('uid', target['uid']) != target['uid']:
                                raise RuntimeError('active pod incarnation changed')
                            if phase == 'boundary':
                                bind_boundary_node(target, selection)
                                inventory = target['barrierNodeInventory']
                                checked_node = command(k+['get', 'node', target['node'], '-o', 'json'])
                                if checked_node.returncode or json.loads(checked_node.stdout)['metadata']['uid'] != inventory['kubernetesNodeUID']:
                                    raise RuntimeError('original Kubernetes node incarnation differs')
                            report['target'] = target
                    report['killAt'] = stamp()
                    report['lastPreLossIndex'] = segments[-1]['Index'] if segments else None
                    # Local monotonic ordering: this timestamp precedes dispatch,
                    # so a same-host production timestamp yields an upper bound.
                    (out/'fault-dispatch.json').write_text(json.dumps({
                        'session': session, 'dispatchedAt': report['killAt'],
                        'role': role, 'action': action,
                        'target': target, 'targetSelection': report.get('targetSelection'),
                        'lastClientIndex': report['lastPreLossIndex']})+'\n')
                    if action == 'force-delete':
                        options = {'apiVersion':'v1', 'kind':'DeleteOptions', 'gracePeriodSeconds':0,
                                   'preconditions':{'uid':target['uid']}}
                        deletion = out/'delete-options.json'
                        deletion.write_text(json.dumps(options))
                        result = command(k+['delete', '--raw', f"/api/v1/namespaces/{c['namespace']}/pods/{target['name']}", '-f', str(deletion)])
                    elif c.get('lossWrapperArgv'):
                        ready_file = out/'fault-ready'
                        fault_argv = [x.replace('{readyFile}', str(ready_file)).replace('{session}', session).replace('{runDir}', str(out)) for x in c['lossWrapperArgv']]
                        fault_handles = [(out/'action.stdout').open('wb'), (out/'action.stderr').open('wb')]
                        fault_process = subprocess.Popen(fault_argv, stdin=subprocess.PIPE,
                            stdout=fault_handles[0], stderr=fault_handles[1], start_new_session=True)
                        ready_deadline = time.monotonic()+min(150, max(1, c.get('faultReadyTimeoutSeconds', 20)))
                        while not ready_file.exists() and fault_process.poll() is None and time.monotonic()<ready_deadline:
                            time.sleep(0.05)
                        if not ready_file.exists() or fault_process.poll() is not None:
                            raise RuntimeError('fault wrapper did not become ready')
                        report['faultReadyAt'] = stamp()
                        try:
                            report['faultReadyEvidence'] = json.loads(ready_file.read_text())
                        except ValueError:
                            if c.get('requireFaultReadyJSON'):
                                raise RuntimeError('native fault requires JSON kill evidence')
                        if c.get('requireFaultReadyJSON') and (report['faultReadyEvidence'].get('verified') is not True or report['faultReadyEvidence'].get('session') != session):
                            raise RuntimeError('fault proof is unverified or belongs to another session')
                        result = subprocess.CompletedProcess(fault_argv, 0, b'fault wrapper ready', b'')
                    else:
                        restoration_needed = True
                        result = command(c['lossArgv'])
                    report['actionSucceeded'] = result.returncode == 0
                    # Output may contain private host inventory; retained privately.
                    if fault_process is None:
                        (out/'action.stdout').write_bytes(result.stdout)
                        (out/'action.stderr').write_bytes(result.stderr)
                    if result.returncode:
                        raise RuntimeError('loss action failed')
                    if phase == 'boundary':
                        proof_argv = [x.replace('{session}', session).replace('{index}', str(c['pauseBeforeIndex'])).replace('{runDir}', str(out)) for x in c['barrierFaultProofArgv']]
                        checked = command(proof_argv)
                        (out/'barrier-proof.stderr').write_bytes(checked.stderr)
                        (out/'barrier-proof.stdout').write_bytes(checked.stdout)
                        if checked.returncode:
                            raise RuntimeError('original encoder task absence was not verified')
                        proof = json.loads(checked.stdout)
                        report['barrierRelease'] = release_boundary(barrier_dir, barrier, target, proof)
                        report['barrierFaultProof'] = proof
                time.sleep(0.1)
            if process.poll() is None:
                raise RuntimeError('player exceeded bounded window')
            report['playerExitCode'] = process.returncode
    except (Exception, KeyboardInterrupt) as error:
        report['error'] = type(error).__name__ + ': ' + str(error)
    finally:
        if process and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill(); process.wait()
        if fault_process is not None:
            try:
                fault_process.communicate(input=b'', timeout=min(180, max(1, c.get('restoreTimeoutSeconds', 150))))
            except subprocess.TimeoutExpired:
                os.killpg(fault_process.pid, signal.SIGTERM)
                try:
                    fault_process.wait(timeout=20)
                except subprocess.TimeoutExpired:
                    os.killpg(fault_process.pid, signal.SIGKILL)
                    fault_process.wait()
            report['restored'] = fault_process.returncode == 0
            report['faultRestoredAt'] = stamp()
            (out/'fault-restoration.json').write_text(json.dumps({key: report.get(key) for key in ('session', 'role', 'action', 'restored', 'faultRestoredAt')}, indent=2)+'\n')
            for handle in fault_handles:
                handle.close()
        if restoration_needed:
            result = command(c['restoreArgv'])
            report['restored'] = result.returncode == 0
            report['faultRestoredAt'] = stamp()
            (out/'fault-restoration.json').write_text(json.dumps({key: report.get(key) for key in ('session', 'role', 'action', 'restored', 'faultRestoredAt')}, indent=2)+'\n')
            (out/'restore.stderr').write_bytes(result.stderr)
        player = out/'player'/'report.json'
        if player.exists():
            try:
                p = json.loads(player.read_text())
            except (OSError, ValueError):
                p = {}
                report['error'] = 'player report is unreadable'
            report['playback'] = p
            controls = [a for a in p.get('Attempts', []) if a.get('Kind') in ('timeline', 'stop')]
            # The client sends Stop last, after its bounded playback window.
            report['control'] = control_summary(controls)
            report['outcomes'] = {'mediaPassed': p.get('Outcome') == 'passed',
                                  'controlPassed': report['control']['allRequestsPassed'],
                                  'stopPassed': report['control']['stopPassed'],
                                  'actionSucceeded': report['actionSucceeded']}
            kill = parsed(report['killAt']) if report['killAt'] else None
            if kill:
                report['postLossObservationSeconds'] = max(0, parsed(p['StartedAt'])+p.get('ElapsedSeconds', 0)-kill)
                receipts = [parsed(s['ReceivedAt']) for s in p.get('Segments') or [] if parsed(s['ReceivedAt']) >= kill]
                if receipts:
                    report['clientFirstPostLossReceiptSeconds'] = min(receipts)-kill
                if c.get('producerEvidence') and Path(c['producerEvidence']).exists():
                    evidence = Path(c['producerEvidence'])
                    try:
                        rows = json.loads(evidence.read_text())
                        if not isinstance(rows, list):
                            raise ValueError('observer rows must be an array')
                        for row in rows:
                            parsed(row['producedAt'])
                            if not isinstance(row['generation'], int) or not isinstance(row['session'], str):
                                raise ValueError('invalid observer row')
                    except (OSError, ValueError, TypeError, KeyError):
                        rows = []
                        report['producerEvidenceError'] = 'unreadable observer evidence'
                    before = [r for r in rows if r['session'] == session and parsed(r['producedAt']) < kill]
                    after = [r for r in rows if r['session'] == session and parsed(r['producedAt']) >= kill]
                    # Only a trusted observer can associate a new encoder generation
                    # with server production. Unchanged generations don't prove recovery.
                    recovered = [r for r in after if before and r['generation'] > max(x['generation'] for x in before)]
                    if recovered:
                        report['serverInterruptionSeconds'] = min(parsed(r['producedAt']) for r in recovered)-kill
                        report['serverInterruptionVerified'] = True
                    report['producerEvidenceSHA256'] = hashlib.sha256(evidence.read_bytes()).hexdigest()
            report['passed'] = (media_control_passed(p.get('Outcome') == 'passed', report['actionSucceeded'], report['control'])
                                and (action == 'none' or report['killAt'] is not None)
                                and report.get('restored', True) and 'error' not in report
                                and report.get('playerExitCode') == 0
                                and report.get('postLossObservationSeconds', 0) >= c.get('minimumPostLossSeconds', 0))
        else:
            report['passed'] = False
        if c.get('postCheckArgv') and player.exists() and (report['killAt'] or action == 'none'):
            # These are the original event timestamps, not a fresh cleanup clock.
            # The observer must retain pre-fault identities independently.
            context = {key: report.get(key) for key in ('session', 'role', 'action', 'phase',
                       'killAt', 'faultRestoredAt', 'restored', 'target', 'targetSelection')}
            context['postCheckStartedAt'] = stamp()
            (out/'post-check-context.json').write_text(json.dumps(context, indent=2)+'\n')
            replacements = {'{session}':session, '{playerReport}':str(player), '{faultReadyFile}':str(out/'fault-ready'), '{runDir}':str(out)}
            argv = []
            for value in c['postCheckArgv']:
                for key, replacement in replacements.items():
                    value = value.replace(key, replacement)
                argv.append(value)
            checked = command(argv)
            (out/'post-check.stderr').write_bytes(checked.stderr)
            try:
                proof = json.loads(checked.stdout)
                if not isinstance(proof, dict):
                    raise ValueError('post-check proof must be an object')
            except ValueError:
                proof = {'passed':False, 'error':'post-check returned invalid JSON'}
            report['postCheck'] = proof
            report['passed'] = (report['passed'] and checked.returncode == 0 and proof.get('passed') is True and proof.get('session') == session)
            upper = proof.get('serverInterruptionUpperBoundSeconds')
            verified = proof.get('serverInterruptionUpperBoundVerified') is True and proof.get('session') == session and isinstance(upper, (int,float)) and not isinstance(upper, bool) and 0 <= upper < c.get('bufferSeconds', 30)
            report['serverInterruptionUpperBoundSeconds'] = upper
            report['serverInterruptionUpperBoundVerified'] = verified
        if role == 'transcode' and action != 'none' and not (report['serverInterruptionVerified'] or report.get('serverInterruptionUpperBoundVerified', False)):
            report['passed'] = False
        report['finishedAt'] = stamp()
        (out/'report.json').write_text(json.dumps(report, indent=2)+'\n')
        p = report.get('playback', {})
        text = (f"# {role} / {action} / {phase}\n\nPassed: **{report['passed']}**\n\n"
                f"Client outcome: {p.get('Outcome', 'missing')}; stalls: {len(p.get('Stalls') or [])}; retries: {p.get('Retries', 0)}.\n\n"
                f"Lifecycle control: {report.get('control', {})}.\n\n"
                f"Server interruption: {report['serverInterruptionSeconds']} seconds (verified: {report['serverInterruptionVerified']}).\n\n"
                f"First post-loss receipt: {report['clientFirstPostLossReceiptSeconds']} seconds.\n")
        (out/'report.md').write_text(text)
    return report['passed']


def main():
    os.umask(0o077)
    def interrupted(_signal, _frame):
        # Route service-manager termination through run()'s bounded cleanup.
        raise KeyboardInterrupt()
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--config', type=Path, required=True)
    ap.add_argument('--output', type=Path, required=True)
    ap.add_argument('--matrix', action='store_true', help='run 16 Plex role/action/phase cases plus Jellyfin controller/transcode loss; config needs targets[role] and jellyfin')
    a = ap.parse_args()
    c = json.loads(a.config.read_text())
    if a.matrix:
        if not c.get('jellyfin'):
            ap.error('--matrix requires jellyfin client config for controller/transcode cases')
        ok = True
        for role in ('controller', 'source', 'transcode', 'delivery'):
            for action in ('force-delete', 'pause'):
                for phase in ('early', 'steady'):
                    case = dict(c, **c['targets'][role]); case.update(protocol='plex', role=role, action=action, phase=phase)
                    ok = run(case, a.output/f'{role}-{action}-{phase}') and ok
        if c.get('jellyfin'):
            for role in ('controller', 'transcode'):
                case = dict(c, **c['jellyfin']); case.update(c['targets'][role])
                case.update(protocol='jellyfin', role=role, action='force-delete', phase='steady')
                ok = run(case, a.output/f'jellyfin-{role}-force-delete-steady') and ok
    else:
        ok = run(c, a.output)
    return 0 if ok else 1


if __name__ == '__main__':
    raise SystemExit(main())
