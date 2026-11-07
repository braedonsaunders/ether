"""Prospective root cleanup policy v1; historical reports stay unchanged."""
import calendar
import datetime as dt
import decimal
import re

VERSION = 'root-20261107-v1'
BOUND_NS = 15_000_000_000


def timestamp_ns(value):
    match = re.fullmatch(r'(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d{1,9}))?Z', value)
    if not match:
        # Python's authoritative restore stamps have an explicit UTC offset.
        if value.endswith('+00:00'):
            return timestamp_ns(value[:-6]+'Z')
        raise ValueError('UTC timestamp required')
    whole = dt.datetime.strptime(match[1], '%Y-%m-%dT%H:%M:%S')
    return calendar.timegm(whole.timetuple())*1_000_000_000+int((match[2] or '').ljust(9, '0'))


def utc(ns):
    seconds, fraction = divmod(ns, 1_000_000_000)
    return dt.datetime.fromtimestamp(seconds, dt.timezone.utc).strftime('%Y-%m-%dT%H:%M:%S')+f'.{fraction:09d}Z'


def exact_target(dispatch):
    target = dispatch.get('target') or dispatch.get('targetSelection')
    if not isinstance(target, dict):
        raise ValueError('exact pre-fault target absent')
    name = target.get('name') or target.get('pod')
    if not name or not target.get('uid') or not target.get('node'):
        raise ValueError('target name, UID and node required')
    containers = [x.get('containerID') for x in target.get('containers', []) if x.get('name') == 'ether']
    if len(containers) != 1 or not re.fullmatch(r'containerd://[a-f0-9]{64}', containers[0] or ''):
        raise ValueError('immutable Ether container ID required')
    if dispatch.get('role') == 'source':
        source = target.get('sourceIdentity')
        if not isinstance(source, dict) or not source.get('NodeID') or not source.get('Boot'):
            raise ValueError('immutable source node/boot required')
    return dict(name=name, uid=target['uid'], node=target['node'], containerID=containers[0],
                sourceIdentity=target.get('sourceIdentity'))


def anchor(player, context):
    controls = [x for x in player.get('Attempts', []) if x.get('Kind') in ('timeline', 'stop')]
    if not controls or controls[-1].get('Kind') != 'stop' or not 200 <= controls[-1].get('Status', 0) < 300:
        raise ValueError('successful terminal Stop required')
    stop = controls[-1]
    duration = decimal.Decimal(str(stop['TotalSeconds']))
    if not duration.is_finite() or duration < 0:
        raise ValueError('invalid Stop duration')
    response = timestamp_ns(stop['RequestedAt'])+int((duration*1_000_000_000).to_integral_value())
    if context.get('action') not in ('none', 'force-delete'):
        if context.get('restored') is not True or not context.get('faultRestoredAt'):
            raise ValueError('successful restoration required')
        return max(response, timestamp_ns(context['faultRestoredAt']))
    return response


def evaluate(session, player, context, target, frozen_at, samples, started_at):
    if context.get('session') != session or player.get('Session') != session:
        raise ValueError('cleanup session differs')
    start = anchor(player, context)
    deadline = start+BOUND_NS
    if context.get('action') != 'none':
        if (not isinstance(target, dict) or not all(target.get(key) for key in ('name', 'uid', 'node', 'containerID'))
                or not re.fullmatch(r'containerd://[a-f0-9]{64}', target.get('containerID', ''))
                or not frozen_at or timestamp_ns(frozen_at) > start):
            return dict(passed=None, status='INVALID_PREFAULT_TARGET')
    if timestamp_ns(started_at) > deadline:
        return dict(passed=None, status='INVALID_LATE_COLLECTOR', anchorUTC=utc(start), deadlineUTC=utc(deadline))
    if any(x.get('session') != session for x in samples):
        raise ValueError('cross-session sample')
    valid = [x for x in samples if start <= timestamp_ns(x['completedAt']) <= deadline]
    good = [x for x in valid if x.get('retired') == 1 and x.get('current') == x.get('displaced') == 0
            and x.get('ffmpegCount') == 0 and x.get('processInventoryVerified') is True]
    nonzero_after = [x for x in samples if x.get('queryStartedAt') is not None and timestamp_ns(x['queryStartedAt']) >= deadline
                     and (x.get('retired') != 1 or x.get('current') != 0 or x.get('displaced') != 0 or (x.get('ffmpegCount') or 0) > 0)]
    passed = True if good else (False if nonzero_after else None)
    return dict(version=VERSION, passed=passed,
                status='PASS_IN_BOUND' if good else ('FAIL_NONZERO_AT_DEADLINE' if nonzero_after else 'INVALID_NO_IN_BOUND_ZERO'),
                anchorUTC=utc(start), deadlineUTC=utc(deadline), boundSeconds=15,
                qualifyingSample=good[0] if good else None,
                lateSamplesIgnored=sum(timestamp_ns(x['completedAt']) > deadline for x in samples))
