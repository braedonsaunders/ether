#!/usr/bin/env bash
# Operator invokes only after receiving the exact green integration image tag.
# heavy-io.sh owns serialization and reports queue+run wall time.
set -euo pipefail
state=${1:?usage: roll-gated.sh PRIVATE_STATE GATED_IMAGE_TAG}
tag=${2:?gated image tag required}
[[ ${ETHER_HEAVY_IO_LOCKED:-0} == 1 ]] || { echo 'invoke through heavy-io.sh' >&2; exit 64; }
root=$(cd "$(dirname "$0")/../../.." && pwd)
chart=${ETHER_LAB_CHART:-$root/packaging/helm/ether}
[[ -f $chart/Chart.yaml ]] || { echo 'Helm chart not found' >&2; exit 64; }
export KUBECONFIG="$state/kubeconfig" PATH="$state/bin:$PATH" MULTIHOST_IP
[[ $(kubectl config current-context) == ether035-multihost ]] || exit 64
if [[ ${ETHER_LAB_IMAGES_IMPORTED:-0} != 1 ]]; then "$root/packaging/kubernetes/multihost/import.sh" "$state" "ether035-multihost:$tag"; fi
extra=(); post=()
if [[ -s $state/physical-host.values.yaml ]]; then extra=(-f "$state/physical-host.values.yaml"); fi
if [[ -s $state/external-private-ip ]]; then
 MULTIHOST_IP=$(cat "$state/external-private-ip")
 post=(--post-renderer "$root/packaging/kubernetes/multihost/external-private.py")
fi
kubectl -n ether035-ha get pods -o json > "$state/pods-before-gated-roll.json"
tmpfs=$(kubectl -n ether035-ha get statefulset ether035-ha-pg -o 'jsonpath={.spec.template.spec.volumes[?(@.name=="pg-memory")].emptyDir.medium}')
if [[ $tmpfs != Memory ]]; then
 # Quiesce all writers before a complete PG snapshot; old PVCs/identities remain.
 for role in server source transcode delivery; do
  for ordinal in 0 1; do
   kubectl -n ether035-ha exec "ether035-mh-ether-$role-$ordinal" -c ether -- \
    /usr/lib/ether/ether cluster drain --data-dir /config/ether --wait 120s
  done
 done
 kubectl -n ether035-ha scale statefulset -l app.kubernetes.io/instance=ether035-mh --replicas=0
 for ((attempt=0; attempt<240; attempt++)); do
  count=$(kubectl -n ether035-ha get pods -l app.kubernetes.io/instance=ether035-mh -o json | python3 -c 'import json,sys;print(len(json.load(sys.stdin)["items"]))')
  [[ $count != 0 ]] || break
  sleep 1
 done
 [[ $count == 0 ]] || { echo 'old role pods did not retire; PG retained' >&2; exit 1; }
 "$root/packaging/kubernetes/multihost/migrate-pg-tmpfs.sh" "$state"
fi
helm upgrade ether035-mh "$chart" -n ether035-ha \
 -f "$root/packaging/kubernetes/multihost/values.yaml" "${extra[@]}" \
 --set-string "image.tag=$tag" "${post[@]}"
# OnDelete preserves healthy workers; replace only ordinals still on old image.
for role in server source transcode delivery; do
 for ordinal in 0 1; do
  pod="ether035-mh-ether-$role-$ordinal"
  if kubectl -n ether035-ha get pod "$pod" >/dev/null 2>&1; then
   actual=$(kubectl -n ether035-ha get pod "$pod" -o 'jsonpath={.spec.containers[0].image}')
   if [[ $actual != "ether035-multihost:$tag" ]]; then "$root/packaging/kubernetes/rollout.sh" ether035-ha "$pod"; fi
  fi
 done
done
for ((attempt=0; attempt<180; attempt++)); do
 count=$(kubectl -n ether035-ha get pods -l app.kubernetes.io/instance=ether035-mh -o json | python3 -c 'import json,sys;print(len(json.load(sys.stdin)["items"]))')
 [[ $count != 8 ]] || break
 sleep 1
done
[[ $count == 8 ]] || { echo 'expected eight role pods did not appear' >&2; exit 1; }
kubectl -n ether035-ha wait --for=condition=Ready pod -l app.kubernetes.io/instance=ether035-mh --timeout=300s
kubectl -n ether035-ha get pods -o json > "$state/pods-after-gated-roll.json"
echo 'gated image ready; verify exact image IDs, legacy retirement and free capacity before playback'
