#!/usr/bin/env python3
"""Offline pinning invariants; no cluster calls, credentials or acceptance claim."""
import copy
import os
from pathlib import Path
import subprocess
import sys
import yaml

script = Path(__file__).with_name('fresh-baseline-postrender.py')
base = [
    {'kind': 'StatefulSet', 'metadata': {'name': 'ether035-roles-ether-server'},
     'spec': {'replicas': 2, 'podManagementPolicy': 'Parallel', 'updateStrategy': {'type': 'OnDelete'}}},
    {'kind': 'Service', 'metadata': {'name': 'ether035-roles-ether'},
     'spec': {'type': 'ClusterIP', 'selector': {'app.kubernetes.io/instance': 'ether035-roles',
       'app.kubernetes.io/name': 'ether', 'ether.plex/public': 'true'}}},
    {'kind': 'ConfigMap', 'metadata': {'name': 'ether035-roles-ether-server'},
     'data': {'deployment.json': '{"roles":["server"],"bootstrap":false}\n'}},
]
base.extend({'kind': 'StatefulSet', 'metadata': {'name': 'ether035-roles-ether-' + role},
             'spec': {'replicas': 1, 'podManagementPolicy': 'Parallel', 'updateStrategy': {'type': 'OnDelete'}}}
            for role in ('source', 'transcode', 'delivery'))
pin = 'statefulset.kubernetes.io/pod-name'
expected = copy.deepcopy(base)
expected[1]['spec']['selector'][pin] = 'ether035-roles-ether-server-1'


def run(items, optimize=False):
    return subprocess.run([sys.executable, *(['-O'] if optimize else []), str(script)],
        input=yaml.safe_dump_all(items), capture_output=True, text=True, timeout=10,
        env={**os.environ, 'PYTHONOPTIMIZE': '1' if optimize else '0'})


for optimize in (False, True):
    result = run(base, optimize)
    assert result.returncode == 0, result.stderr
    assert list(yaml.safe_load_all(result.stdout)) == expected
    assert list(yaml.safe_load_all(run(expected, optimize).stdout)) == expected
    cases = []
    for field, value in [('replicas', 1), ('podManagementPolicy', 'OrderedReady'),
                         ('updateStrategy', {'type': 'RollingUpdate'})]:
        bad = copy.deepcopy(base); bad[0]['spec'][field] = value; cases.append(bad)
    for field, value in [('type', 'NodePort'), ('publishNotReadyAddresses', True)]:
        bad = copy.deepcopy(base); bad[1]['spec'][field] = value; cases.append(bad)
    for field, value in [('ether.plex/public', 'false'), ('app.kubernetes.io/instance', 'other'),
                         (pin, 'ether035-roles-ether-server-0')]:
        bad = copy.deepcopy(base); bad[1]['spec']['selector'][field] = value; cases.append(bad)
    bad = copy.deepcopy(base); bad[0]['metadata']['namespace'] = 'other'; cases.append(bad)
    bad = copy.deepcopy(base); bad[0]['metadata']['annotations'] = {'helm.sh/hook': 'pre-upgrade'}; cases.append(bad)
    cases.extend([base + [copy.deepcopy(base[0])], base + [copy.deepcopy(base[1])], base[1:], []])
    bad = copy.deepcopy(base); bad[3]['spec']['replicas'] = 2; cases.append(bad)
    bad = copy.deepcopy(base); bad[1]['spec']['selector'] = 'invalid'; cases.append(bad)
    cases.append(base + [{'kind': 'Secret', 'metadata': {'name': 'sentinel'}, 'data': {'key': 'DO_NOT_ECHO'}}])
    for bad in cases:
        result = run(bad, optimize)
        assert result.returncode != 0 and result.stdout == '', result.stdout
        assert 'DO_NOT_ECHO' not in result.stderr
for raw in ('apiVersion: [', 'null\n', 'x' * (1024 * 1024 + 1)):
    result = subprocess.run([sys.executable, str(script)], input=raw, text=True,
                            capture_output=True, timeout=10)
    assert result.returncode != 0 and result.stdout == ''
print('offline fresh baseline pin checks passed, including optimized Python; no deployment performed')
