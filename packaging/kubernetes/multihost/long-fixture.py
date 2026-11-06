#!/usr/bin/env python3
"""Render an isolated600s generated fixture; does not alter the180s fixture."""
import sys
from pathlib import Path
import yaml

root, image = Path(sys.argv[1]), sys.argv[2]
for obj in yaml.safe_load_all((root/'packaging/kubernetes/source-fixture.yaml').read_text()):
    obj['metadata']['name'] = 'ether035-ha-long-source'
    if obj['kind'] == 'Service':
        obj['spec']['selector']['app'] = 'ether035-ha-long-source'
    if obj['kind'] == 'Deployment':
        obj['spec']['selector']['matchLabels']['app'] = 'ether035-ha-long-source'
        pod = obj['spec']['template']
        pod['metadata']['labels']['app'] = 'ether035-ha-long-source'
        spec = pod['spec']
        spec['nodeSelector'] = {'kubernetes.io/hostname':'ether035-mh-server'}
        gen = spec['initContainers'][0]
        gen['image'],gen['imagePullPolicy'] = image,'Never'
        gen['args'] = ['600' if arg == '180' else '/media/long-fixture.mp4' if arg == '/media/fixture.mp4' else arg for arg in gen['args']]
        spec['containers'][0]['args'] = [
            '--media','/media/long-fixture.mp4',
            '--origin','http://ether035-ha-long-source.ether035-ha.svc.cluster.local:8080',
            '--tmdb','900002','--release','Ether.Long.Cluster.Fixture.2020.360p.WEB-DL.MPEG4.AAC',
            '--fixture-id','ether035-long-fixture','--article-prefix','ether035-long-part']
        # Source data/chunks/yEnc stay bounded even for the longer generated clip.
        spec['containers'][0]['resources']['limits']['memory'] = '512Mi'
        for volume in spec['volumes']:
            if volume['name'] == 'media': volume['emptyDir']['sizeLimit'] = '256Mi'
            if volume['name'] == 'code': volume['configMap']['name'] = 'ether035-ha-long-source-code'
    yaml.safe_dump(obj,sys.stdout)
    print('---')
