#!/usr/bin/env python3
"""LAB ONLY: pin fresh baseline API traffic to server1; never operate a cluster.

Reads a Helm manifest on stdin and writes the transformed manifest on stdout.
Requires PyYAML (the chart checks pin its version). Deployment still needs the
coordinator's exact candidate, schema and final-render approval.
"""
import sys
import yaml

LIMIT = 1024 * 1024
API = 'ether035-roles-ether'
POD = API + '-server-1'
PIN = 'statefulset.kubernetes.io/pod-name'


def require(ok, reason):
    if not ok:
        raise ValueError(reason)


def unique(items, kind, name):
    matches = [x for x in items if x.get('kind') == kind and x['metadata'].get('name') == name]
    require(len(matches) == 1, 'required LAB resource missing or duplicated')
    return matches[0]


def transform(items):
    require(bool(items), 'empty manifest')
    for item in items:
        require(isinstance(item, dict) and isinstance(item.get('metadata'), dict), 'invalid resource')
        require(item.get('kind') != 'Secret', 'credential resources are forbidden')
        require(item['metadata'].get('namespace') in (None, 'ether035-ha'), 'unexpected namespace')
        require('helm.sh/hook' not in item['metadata'].get('annotations', {}), 'hooks are forbidden')
    sets = [x for x in items if x.get('kind') == 'StatefulSet']
    names = {API + '-' + role for role in ('server', 'source', 'transcode', 'delivery')}
    require(len(sets) == 4 and {x['metadata'].get('name') for x in sets} == names,
            'exact four LAB role StatefulSets required')
    for item in sets:
        spec = item['spec']
        count = 2 if item['metadata']['name'] == API + '-server' else 1
        require(type(spec['replicas']) is int and spec['replicas'] == count, 'unexpected role replicas')
        require(spec['podManagementPolicy'] == 'Parallel', 'Parallel pod management required')
        require(spec['updateStrategy'] == {'type': 'OnDelete'}, 'OnDelete without automatic rollout required')
    api = unique(items, 'Service', API)['spec']
    require(api['type'] == 'ClusterIP', 'API Service must remain ClusterIP')
    require(not api.get('publishNotReadyAddresses', False), 'API must exclude unready addresses')
    selector = api['selector']
    require(isinstance(selector, dict), 'invalid API selector')
    require(selector.get('app.kubernetes.io/instance') == 'ether035-roles'
            and selector.get('app.kubernetes.io/name') == 'ether'
            and selector.get('ether.plex/public') == 'true', 'unexpected API selector')
    require(selector.get(PIN) in (None, POD), 'conflicting API pod pin')
    selector[PIN] = POD
    return items


def main():
    raw = sys.stdin.read(LIMIT + 1)
    try:
        require(len(raw) <= LIMIT, 'manifest exceeds LAB bound')
        items = list(yaml.safe_load_all(raw))
        result = transform(items)
        # Serialize fully before emitting: failures must never leave partial YAML.
        rendered = yaml.safe_dump_all(result, sort_keys=False)
    except (ValueError, KeyError, TypeError, yaml.YAMLError):
        # Manifest content and parser exception text can contain credentials.
        print('fresh baseline LAB postrenderer: manifest rejected; no output', file=sys.stderr)
        return 1
    sys.stdout.write(rendered)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
