#!/usr/bin/env python3
"""Opt-in state alignment for disposable MediaBrowser fixtures; credentials via stdin.

Input JSON: {"reference_url":...,"target_url":...,"reference_user":...,
"reference_password":...,"target_user":...,"target_password":...,"apply":true}.
Reference receives only GET and login/logout. Target must be loopback. No
credential or response body is printed. Reference duplicate-version conflicts,
unmatched titles, ambiguous episode mapping and incomplete cleanup fail closed.
"""
import json
import sys
import urllib.error
import urllib.parse
import urllib.request

class Failure(Exception):
    pass

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class Server:
    def __init__(self, base, device):
        self.base = base.rstrip('/')
        self.device = device
        self.token = ''
        self.user = ''
        self.opener = urllib.request.build_opener(NoRedirect())

    def call(self, method, path, payload=None):
        auth = 'MediaBrowser Client="ScorecardSync", Device="fixture", DeviceId="%s", Version="1"' % self.device
        if self.token:
            auth += ', Token="%s"' % self.token
        headers = {'Authorization': auth, 'Content-Type': 'application/json'}
        request = urllib.request.Request(self.base + path, data=None if payload is None else json.dumps(payload).encode(), headers=headers, method=method)
        try:
            with self.opener.open(request, timeout=30) as response:
                raw = response.read(16 * 1024 * 1024 + 1)
                if len(raw) > 16 * 1024 * 1024:
                    raise Failure('response too large')
                return json.loads(raw) if raw else {}
        except (urllib.error.URLError, ValueError, OSError):
            raise Failure('HTTP operation failed') from None

    def login(self, user, password):
        data = self.call('POST', '/Users/AuthenticateByName', {'Username': user, 'Pw': password})
        self.token = data.get('AccessToken', '')
        self.user = data.get('User', {}).get('Id', '')
        if not self.token or not self.user:
            raise Failure('login returned incomplete identity')

    def logout(self):
        if self.token:
            self.call('POST', '/Sessions/Logout', {})
            self.token = ''

    def items(self):
        q = urllib.parse.urlencode({'Recursive': 'true', 'IncludeItemTypes': 'Movie,Series,Episode', 'Fields': 'ProviderIds,UserData,SeriesId,IndexNumber,ParentIndexNumber', 'Limit': '10000'})
        data = self.call('GET', '/Users/' + urllib.parse.quote(self.user, safe='') + '/Items?' + q)
        items = data.get('Items')
        if not isinstance(items, list) or data.get('TotalRecordCount', len(items)) > len(items):
            raise Failure('incomplete item listing')
        return items


def key_states(items):
    shows = {x.get('Id'): str(x.get('ProviderIds', {}).get('Tmdb', '')) for x in items if x.get('Type') == 'Series'}
    result = {}
    for x in items:
        typ = x.get('Type')
        if typ == 'Movie':
            tmdb = str(x.get('ProviderIds', {}).get('Tmdb', ''))
            key = ('movie', tmdb)
        elif typ == 'Episode':
            tmdb = shows.get(x.get('SeriesId'), '')
            season, episode = x.get('ParentIndexNumber'), x.get('IndexNumber')
            if not isinstance(season, int) or not isinstance(episode, int):
                raise Failure('episode identity incomplete')
            key = ('episode', tmdb, season, episode)
        else:
            continue
        if not tmdb or not x.get('Id') or not isinstance(x.get('UserData'), dict):
            raise Failure('item identity or user state incomplete')
        u = x['UserData']
        position = u.get('PlaybackPositionTicks', 0)
        if not isinstance(position, int) or position < 0:
            raise Failure('invalid resume position')
        played, favorite = u.get('Played', False), u.get('IsFavorite', False)
        if not isinstance(played, bool) or not isinstance(favorite, bool):
            raise Failure('invalid played or favorite state')
        state = (played, favorite, position)
        if key in result and result[key][1] != state:
            raise Failure('duplicate versions have conflicting user state')
        if key in result:
            continue
        result[key] = (x['Id'], state)
    return result


def target_loopback(base):
    u = urllib.parse.urlsplit(base)
    return u.scheme == 'http' and u.hostname in ('127.0.0.1', '::1') and u.port and not u.username and not u.password and not u.query and not u.fragment


def align(reference, target, apply):
    refs, targets = key_states(reference.items()), key_states(target.items())
    if not refs or set(refs) != set(targets):
        raise Failure('reference and fixture item identities differ')
    changed = 0
    for key, (_, state) in refs.items():
        target_id, current = targets[key]
        if current == state:
            continue
        changed += 1
        if not apply:
            continue
        played, favorite, position = state
        path = urllib.parse.quote(target_id, safe='')
        if current[0] != played:
            target.call('POST' if played else 'DELETE', '/UserPlayedItems/' + path, {} if played else None)
        if current[1] != favorite:
            target.call('POST' if favorite else 'DELETE', '/UserFavoriteItems/' + path, {} if favorite else None)
        if current[2] != position:
            if position == 0:
                # A reset is supplied by the normal unplayed mutation.
                if played:
                    raise Failure('cannot clear progress while preserving played state')
                target.call('DELETE', '/UserPlayedItems/' + path)
            else:
                reply = target.call('POST', '/Items/' + path + '/PlaybackInfo?IsPlayback=true', {'UserId': target.user, 'IsPlayback': True})
                session = reply.get('PlaySessionId', '')
                if not session:
                    raise Failure('resume seed session missing')
                target.call('POST', '/Sessions/Playing/Stopped', {'PlaySessionId': session, 'ItemId': target_id, 'PositionTicks': position})
    if apply:
        checked = key_states(target.items())
        if set(checked) != set(refs) or any(checked[k][1] != refs[k][1] for k in refs):
            raise Failure('target user state verification failed')
    return {'items': len(refs), 'changed': changed, 'applied': bool(apply)}


def main():
    try:
        config = json.load(sys.stdin)
        if not target_loopback(config['target_url']):
            raise Failure('target must be loopback')
        reference, target = Server(config['reference_url'], 'scorecard-sync-ref'), Server(config['target_url'], 'scorecard-sync-target')
        result, errors = None, []
        try:
            reference.login(config['reference_user'], config['reference_password'])
            target.login(config['target_user'], config['target_password'])
            result = align(reference, target, config.get('apply') is True)
        except Failure as exc:
            errors.append(str(exc))
        except (KeyError, TypeError):
            errors.append('state alignment failed')
        finally:
            for server in (target, reference):
                try:
                    server.logout()
                except Failure:
                    errors.append('logout failed')
        if errors:
            raise Failure('; '.join(errors))
        print(json.dumps(result))
    except (Failure, KeyError, ValueError, TypeError) as exc:
        print(json.dumps({'error': str(exc) if isinstance(exc, Failure) else 'invalid configuration'}), file=sys.stderr)
        return 1
    return 0

if __name__ == '__main__':
    sys.exit(main())
