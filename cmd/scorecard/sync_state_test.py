#!/usr/bin/env python3
import importlib.util
import pathlib
import unittest

p = pathlib.Path(__file__).with_name('sync-state.py')
spec = importlib.util.spec_from_file_location('scorecard_sync', p)
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


def item(ident, played=False, favorite=False, pos=0):
    return {'Id': ident, 'Type': 'Movie', 'ProviderIds': {'Tmdb': '7'}, 'UserData': {'Played': played, 'IsFavorite': favorite, 'PlaybackPositionTicks': pos}}

class Fake:
    def __init__(self, state):
        self.rows = [item('item', *state)]
        self.user = 'viewer'
        self.calls = []
    def items(self): return self.rows
    def call(self, method, path, payload=None):
        self.calls.append((method, path))
        data = self.rows[0]['UserData']
        if path.startswith('/UserFavoriteItems/'):
            data['IsFavorite'] = method == 'POST'
        if path.startswith('/UserPlayedItems/'):
            data['Played'] = method == 'POST'
            data['PlaybackPositionTicks'] = 0
        if path.startswith('/Items/'):
            return {'PlaySessionId': 'session'}
        if path == '/Sessions/Playing/Stopped':
            data['PlaybackPositionTicks'] = payload['PositionTicks']
        return {}

class Tests(unittest.TestCase):
    def test_sync_authored_state_and_resume(self):
        ref, tgt = Fake((False, True, 900000000)), Fake((False, False, 0))
        result = m.align(ref, tgt, True)
        self.assertEqual(result, {'items': 1, 'changed': 1, 'applied': True})
        self.assertEqual(ref.calls, [])
        self.assertEqual(tgt.rows[0]['UserData'], ref.rows[0]['UserData'])
        self.assertIn(('POST', '/Sessions/Playing/Stopped'), tgt.calls)
    def test_dry_run_and_conflicts(self):
        ref, tgt = Fake((True, True, 0)), Fake((False, False, 0))
        self.assertEqual(m.align(ref, tgt, False)['changed'], 1)
        self.assertEqual(tgt.calls, [])
        ref.rows.append(item('other', False, False, 0))
        with self.assertRaises(m.Failure): m.align(ref, tgt, True)
        self.assertEqual(tgt.calls, [])
    def test_explicit_hash_selects_only_matching_reference_version(self):
        ref, tgt = Fake((True, True, 0)), Fake((False, False, 0))
        ref.rows[0]['Path'] = '/media/one.mp4'
        other = item('other', True, False, 0)
        other['Path'] = '/media/two.mp4'
        ref.rows.append(other)
        sha = 'a' * 64
        manifest = [{'tmdb': 7, 'sha256': sha, 'path': '/media/one.mp4'}]
        result = m.align(ref, tgt, True, {'7': sha}, manifest)
        self.assertTrue(result['applied'])
        self.assertTrue(tgt.rows[0]['UserData']['IsFavorite'])
        with self.assertRaises(m.Failure): m.align(ref, tgt, True, {'7': 'b' * 64}, manifest)

    def test_target_is_loopback_only(self):
        self.assertTrue(m.target_loopback('http://127.0.0.1:18096'))
        self.assertFalse(m.target_loopback('http://10.0.0.43:32450'))
        self.assertFalse(m.target_loopback('http://127.0.0.1:18096/?secret=x'))

if __name__ == '__main__': unittest.main()
