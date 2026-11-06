#!/usr/bin/env bash
# LAB ONLY: preserve a consistent DB dump, retain the disk PVC, move PG to tmpfs.
# Run only with playback stopped and under the shared heavy-I/O lock.
set -euo pipefail
umask 077
state=${1:?usage: migrate-pg-tmpfs.sh PRIVATE_STATE}
[[ ${ETHER_HEAVY_IO_LOCKED:-0} == 1 ]] || { echo 'invoke through heavy-io.sh' >&2; exit 64; }
root=$(cd "$(dirname "$0")/../../.." && pwd)
export KUBECONFIG="$state/kubeconfig" PATH="$state/bin:$PATH"
[[ $(kubectl config current-context) == ether035-multihost ]] || exit 64
[[ ! -e /data/bb/thread-storage/batch-gate.active ]] || { echo 'batch gate has priority' >&2; exit 75; }
kubectl -n ether035-ha get statefulset ether035-ha-pg -o json > "$state/pg-before-tmpfs.json"
# Refuse a repeat migration: a tmpfs pod replacement would destroy its live DB.
python3 - "$state/pg-before-tmpfs.json" <<'PY'
import json,sys
s=json.load(open(sys.argv[1]))
assert s['metadata']['name']=='ether035-ha-pg'
assert not any(v.get('emptyDir',{}).get('medium')=='Memory' for v in s['spec']['template']['spec'].get('volumes',[])), 'already tmpfs: retain current pod'
PY
# The caller drains/stops the eight role pods first, so the snapshot cannot
# roll back a concurrently issued grant or generation floor.
roles=$(kubectl -n ether035-ha get pods -l app.kubernetes.io/instance=ether035-mh -o json | python3 -c 'import json,sys;print(len(json.load(sys.stdin)["items"]))')
[[ $roles == 0 ]] || { echo 'drain and stop all Ether roles before DB migration' >&2; exit 64; }
kubectl -n ether035-ha get service ether035-ha-pg -o json > "$state/pg-service-before-tmpfs.json"
# Block fresh TCP clients and close any leftover client transaction before dump;
# all migration commands use the pod's local Unix socket.
kubectl -n ether035-ha patch service ether035-ha-pg --type merge -p '{"spec":{"selector":{"app":"ether035-ha-pg-restore-hold"}}}'
kubectl -n ether035-ha exec ether035-ha-pg-0 -- psql -U ether -d ether -Atq -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=current_database() AND client_addr IS NOT NULL AND pid<>pg_backend_pid();" >/dev/null
backup="$state/private/pg-before-tmpfs-$(date -u +%Y%m%dT%H%M%SZ).dump"
kubectl -n ether035-ha exec ether035-ha-pg-0 -- pg_dump -U ether -d ether -Fc --no-owner > "$backup"
[[ -s $backup ]]
printf '%s\n' "$backup" > "$state/private/pg-tmpfs-backup-path"
sha256sum "$backup" > "$backup.sha256"
# Render only PG: do not touch the original synthetic-source deployment.
python3 "$root/packaging/kubernetes/multihost/fixtures.py" "$root" unused > "$state/tmpfs-fixtures.rendered.yaml"
python3 - "$state/tmpfs-fixtures.rendered.yaml" "$state/pg-tmpfs.yaml" <<'PY'
import yaml,sys
objects=[x for x in yaml.safe_load_all(open(sys.argv[1])) if x.get('metadata',{}).get('name')=='ether035-ha-pg' and x['kind']=='StatefulSet']
assert len(objects)==1
yaml.safe_dump_all(objects,open(sys.argv[2],'w'))
PY
# StatefulSet claim templates are immutable. Remove only its controller, retain
# the old PVC and stop its old pod normally before creating the tmpfs controller.
uid=$(kubectl -n ether035-ha get pod ether035-ha-pg-0 -o 'jsonpath={.metadata.uid}')
kubectl -n ether035-ha delete statefulset ether035-ha-pg --cascade=orphan --wait=true
printf '{"apiVersion":"v1","kind":"DeleteOptions","preconditions":{"uid":"%s"}}' "$uid" |
 kubectl delete --raw /api/v1/namespaces/ether035-ha/pods/ether035-ha-pg-0 -f - >/dev/null
kubectl -n ether035-ha wait --for=delete pod/ether035-ha-pg-0 --timeout=120s
kubectl -n ether035-ha apply -f "$state/pg-tmpfs.yaml"
kubectl -n ether035-ha rollout status statefulset/ether035-ha-pg --timeout=180s
# Restore every table and sequence. No ownership/claim/fencing rows are edited.
kubectl -n ether035-ha exec -i ether035-ha-pg-0 -- pg_restore -U ether -d ether --clean --if-exists --no-owner --exit-on-error < "$backup"
kubectl -n ether035-ha exec ether035-ha-pg-0 -- psql -U ether -d ether -Atq -c 'SHOW fsync; SHOW synchronous_commit; SHOW full_page_writes;' > "$state/pg-tmpfs-settings.txt"
[[ $(tr '\n' ' ' < "$state/pg-tmpfs-settings.txt") == 'off off off ' ]]
kubectl -n ether035-ha patch service ether035-ha-pg --type merge -p '{"spec":{"selector":{"app":"ether035-ha-pg"}}}'
echo 'lab PG restored on tmpfs; private backup and original PVC retained'
