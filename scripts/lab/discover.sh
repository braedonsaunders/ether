#!/usr/bin/env bash
# Authenticate as each adult viewer and write private conformance credentials/vars.
# Run on the Mac while tunnel.sh is in the foreground. Output directory is ignored.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
mkdir -p "$here/.conform"
chmod 700 "$here/.conform"
LAB_HOST="${LAB_HOST:-192.168.68.66}"
LAB_PORT_BASE="${LAB_PORT_BASE:-8096}"
python3 - "$here/lab.env" "$here/.conform" "$LAB_HOST" "$LAB_PORT_BASE" <<'PY'
import json, os, pathlib, sys, urllib.error, urllib.parse, urllib.request, uuid
env = dict(line.rstrip("\n").split("=", 1) for line in open(sys.argv[1]) if "=" in line)
out = pathlib.Path(sys.argv[2])
lab_host = sys.argv[3]
port_base = int(sys.argv[4])
os.umask(0o077)

def call(base, path, method="GET", body=None, headers=None):
    h = {"Accept": "application/json", "Content-Type": "application/json"}
    h.update(headers or {})
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(base + path, data=data, headers=h, method=method)
    try:
        with urllib.request.urlopen(req, timeout=30) as response:
            return json.loads(response.read())
    except urllib.error.HTTPError as error:
        raise RuntimeError(f"{method} {path}: HTTP {error.code}") from None

def save(protocol, token, variables):
    (out / f"{protocol}-reference.token").write_text(token + "\n")
    (out / f"{protocol}-reference-vars.json").write_text(json.dumps(variables, indent=2) + "\n")
    os.chmod(out / f"{protocol}-reference.token", 0o600)
    os.chmod(out / f"{protocol}-reference-vars.json", 0o600)
    print(f"{protocol}: token and variables written under {out}")

def item_vars(base, token, auth, protocol, uid, username, password):
    if protocol == "jellyfin":
        auth["Authorization"] += ', Token="' + token + '"'
        listing = call(base, "/Items?Recursive=true&Fields=ProviderIds,ParentId,RunTimeTicks,Path,Type,Name", headers=auth)
    else:
        auth["X-Emby-Token"] = token
        listing = call(base, "/Items?Recursive=true&Fields=ProviderIds,ParentId,RunTimeTicks,Path,Type,Name", headers=auth)
    wanted = None
    for item in listing.get("Items", []):
        ids = {str(k).lower(): str(v) for k, v in item.get("ProviderIds", {}).items()}
        if ids.get("tmdb") == "10378":
            wanted = (item, ids)
            break
    if wanted is None:
        raise RuntimeError(f"{protocol}: TMDB 10378 (Big Buck Bunny) not found; run seed.sh and wait for library scan")
    item, ids = wanted
    episode = next((x for x in listing.get("Items", []) if x.get("Type") == "Episode" and "S01E01" in x.get("Path", "")), None)
    playback = call(base, f"/Items/{item['Id']}/PlaybackInfo", "POST",
                    {"UserId": uid, "IsPlayback": True, "AutoOpenLiveStream": True, "StartTimeTicks": 0}, auth)
    media_source = next(iter(playback.get("MediaSources", [])), {})
    play_session = playback.get("PlaySessionId") or uuid.uuid4().hex
    values = {"username": username, "password": password, "user_id": uid,
              "access_token": token,
              "item_id": str(item["Id"]), "itemId": str(item["Id"]),
              "parent_id": str((episode or {}).get("ParentId", "")),
              "media_source_id": str(media_source.get("Id", "")),
              "play_session_id": str(play_session),
              "session_id": str(play_session),
              "position_ticks": str((item.get("RunTimeTicks") or 600000000) // 20),
              "device_id": "prism-conform-adult", "server_id": "prism-reference-lab"}
    for provider, value in ids.items():
        values[f"provider:{provider}:{value}"] = str(item["Id"])
    return values

jf_base = f"http://{lab_host}:{port_base}"
jf_user, jf_password = env["JELLYFIN_ADULT_USER"], env["JELLYFIN_ADULT_PASSWORD"]
jf_auth = {"Authorization": 'MediaBrowser Client="conformdiff", Device="conformdiff", DeviceId="prism-lab", Version="1"'}
jf = call(jf_base, "/Users/AuthenticateByName", "POST", {"Username": jf_user, "Pw": jf_password}, jf_auth)
jf_token = jf["AccessToken"]
save("jellyfin", jf_token, item_vars(jf_base, jf_token, jf_auth, "jellyfin", jf["User"]["Id"], jf_user, jf_password))

emby_base = f"http://{lab_host}:{port_base + 1}/emby"
emby_user, emby_password = env["EMBY_ADULT_USER"], env["EMBY_ADULT_PASSWORD"]
emby_auth = {"X-Emby-Authorization": 'MediaBrowser Client="conformdiff", Device="conformdiff", DeviceId="prism-lab", Version="1"'}
emby = call(emby_base, "/Users/AuthenticateByName", "POST", {"Username": emby_user, "Pw": emby_password}, emby_auth)
emby_token = emby["AccessToken"]
save("emby", emby_token, item_vars(emby_base, emby_token, emby_auth, "emby", emby["User"]["Id"], emby_user, emby_password))

silo_base = f"http://{lab_host}:{port_base + 2}"
silo_user, silo_password = env["SILO_ADULT_USER"], env["SILO_ADULT_PASSWORD"]
silo_device = "prism-conform-adult"
silo = call(silo_base, "/api/v2/auth/login", "POST", {"username": silo_user, "password": silo_password}, {"X-Silo-Device-Id": silo_device})
silo_token = silo["access_token"]
silo_auth = {"Authorization": "Bearer " + silo_token, "X-Silo-Device-Id": silo_device}
profiles = call(silo_base, "/api/v2/profiles", headers=silo_auth)
profile_id = next((x["id"] for x in profiles.get("items", []) if x.get("is_primary")), profiles["items"][0]["id"])
silo_auth["X-Profile-Id"] = profile_id
libraries = call(silo_base, "/api/v2/user/libraries", headers=silo_auth).get("items", [])
movie_library = next(x for x in libraries if x.get("type") == "movies")
sections = call(silo_base, f"/api/v2/library/{movie_library['id']}/sections", headers=silo_auth).get("sections", [])
item = next(x for section in sections for x in section.get("items", []) if x.get("content_id") == "movie-tmdb-10378")
versions = call(silo_base, f"/api/v2/catalog/items/{item['content_id']}/versions", headers=silo_auth).get("items", [])
version = next((x for x in versions if x.get("resolution") == "1080p" and x.get("codec_video") == "h264"), versions[0])
capabilities = call(silo_base, "/api/v2/playback/capabilities", headers=silo_auth)
duration = int(version.get("duration") or 600)
base_silo_values = {"username": silo_user, "password": silo_password, "access_token": silo_token,
                    "device_id": silo_device,
                    "profile_id": profile_id, "library_id": str(movie_library["id"]),
                    "item_id": item["content_id"], "file_id": str(version["file_id"]),
                    "installation_id": capabilities["installation_id"], "duration_seconds": str(duration),
                    "position_seconds": str(max(1, duration // 10)), "sequence": "1",
                    "quality_preference": "auto", "app_version": "1.0.0", "app_build": "seed"}
for slug in ("silo-apple", "silo-android"):
    values = dict(base_silo_values)
    values.update({"playback_attempt_id_direct": str(uuid.uuid4()),
                   "playback_attempt_id_transcode": str(uuid.uuid4()),
                   "play_session_id": "", "stop_id": str(uuid.uuid4())})
    repo = pathlib.Path(sys.argv[1]).resolve().parents[2]
    profile = json.loads((repo / "internal" / "conform" / "profiles" / "silo" / f"{slug}.json").read_text())
    direct = profile["flows"]["play_direct"][0]["body"]
    request_text = json.dumps(direct)
    for key, value in values.items():
        request_text = request_text.replace("${" + key + "}", json.dumps(str(value))[1:-1])
    request_body = json.loads(request_text)
    client_headers = dict(silo_auth)
    client_headers.update({"X-Silo-Client": profile["client"], "X-Silo-Client-Version": values["app_version"], "X-Silo-Client-Build": values["app_build"]})
    opened = call(silo_base, "/api/v2/playback/start", "POST", request_body, client_headers)
    if opened.get("outcome") != "playable" or not opened.get("session_id"):
        raise RuntimeError(f"{slug}: direct playback preflight did not return a playable session")
    values["play_session_id"] = opened["session_id"]
    save(slug, silo_token, values)
save("silo", silo_token, base_silo_values)
PY
