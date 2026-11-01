"""Offline lifecycle checks for opt-in lab drivers."""
import contextlib
import io
import json
import pathlib
import runpy
import sys
import types
import unittest
import unittest.mock
import urllib.error

ROOT=pathlib.Path(__file__).parent
class Response:
    def __init__(self,value):self.value=value
    def __enter__(self):return self
    def __exit__(self,*_):return False
    def read(self):return json.dumps(self.value).encode()

class CleanupTests(unittest.TestCase):
    def test_drivers_fail_closed_on_logout_error(self):
        credentials={'jellyfin':{'user':'viewer','password':'private-password'},'emby':{'user':'viewer','password':'private-password'},'fixture':{'user':'fixture','password':'fixture-password'},'username':'viewer','password':'private-password'}
        for name in ('lab-browse.py','lab-profile-browse.py','probe-source.py'):
            with self.subTest(script=name):
                logouts=[]
                def fake_open(req,timeout=None):
                    url=req.full_url
                    if '/Users/AuthenticateByName' in url:return Response({'AccessToken':'secret-token','User':{'Id':'id'}})
                    if '/Sessions/Logout' in url:
                        logouts.append(url)
                        if ':8096' in url:raise urllib.error.HTTPError(url,500,'secret-token',None,None)
                        return Response({})
                    if '/PlaybackInfo' in url:return Response({'MediaSources':[]})
                    if '/Items?' in url:return Response({'Items':[{'Name':'Big Buck Bunny','Path':'1080p','Id':'item'}]})
                    return Response({'Items':[],'TotalRecordCount':0})
                output,error=io.StringIO(),io.StringIO()
                with unittest.mock.patch.object(sys,'stdin',io.StringIO(json.dumps(credentials))),unittest.mock.patch('urllib.request.urlopen',side_effect=fake_open),unittest.mock.patch('subprocess.run',return_value=types.SimpleNamespace(stdout='[]')),contextlib.redirect_stdout(output),contextlib.redirect_stderr(error):
                    with self.assertRaises(SystemExit) as raised:runpy.run_path(str(ROOT/name),run_name='__main__')
                self.assertEqual(raised.exception.code,1)
                self.assertGreaterEqual(len(logouts),2)
                self.assertIn('logout failed',error.getvalue())
                self.assertNotIn('secret-token',output.getvalue()+error.getvalue())
                self.assertNotIn('private-password',output.getvalue()+error.getvalue())
if __name__=='__main__':unittest.main()
