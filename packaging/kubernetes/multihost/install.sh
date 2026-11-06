#!/usr/bin/env bash
# Install fixtures and chart into the explicitly isolated multihost context.
set -euo pipefail
umask 077
state=${1:?usage: install.sh PRIVATE_STATE IMAGE_TAG}
tag=${2:?immutable image tag required}
root=$(cd "$(dirname "$0")/../../.." && pwd)
export KUBECONFIG="$state/kubeconfig"
export PATH="$state/bin:$PATH"
[[ $(kubectl config current-context) == ether035-multihost ]] || exit 64
kubectl create namespace ether035-ha --dry-run=client -o yaml | kubectl apply -f -
kubectl label node ether035-mh-agent1 ether035-mh-agent2 ether.lab/workload=true --overwrite
mkdir -p "$state/private"
chmod 700 "$state/private"
if [[ ! -f $state/private/database-url ]]; then
  python3 - "$state/private" <<'PY'
import pathlib,secrets,sys
p=pathlib.Path(sys.argv[1]); password=secrets.token_urlsafe(24)
(p/'password').write_text(password)
(p/'database-url').write_text('postgres://ether:'+password+'@ether035-ha-pg.ether035-ha.svc.cluster.local:5432/ether?sslmode=disable')
(p/'cluster-key').write_bytes(secrets.token_bytes(32))
PY
  openssl genpkey -algorithm ED25519 -out "$state/private/ca.key"
  openssl req -new -x509 -key "$state/private/ca.key" -out "$state/private/ca.crt" -days 30 \
    -subj /CN=ether035-multihost-lab -addext basicConstraints=critical,CA:TRUE \
    -addext keyUsage=critical,keyCertSign,cRLSign
  cp "$state/private/ca.crt" "$state/private/roots.pem"
fi
for pair in 'ether-db database-url' 'ether-cluster cluster-key' 'ether035-ha-pg password'; do
  read -r name key <<< "$pair"
  if ! kubectl -n ether035-ha get secret "$name" >/dev/null 2>&1; then
    kubectl -n ether035-ha create secret generic "$name" --from-file="$key=$state/private/$key"
  fi
done
if ! kubectl -n ether035-ha get secret ether-authority >/dev/null 2>&1; then
 kubectl -n ether035-ha create secret generic ether-authority \
  --from-file="ca.key=$state/private/ca.key" --from-file="ca.crt=$state/private/ca.crt" --from-file="roots.pem=$state/private/roots.pem"
fi
fixture_flags=()
if kubectl -n ether035-ha get deployment ether035-ha-source >/dev/null 2>&1; then
 fixture_flags=(--preserve-source)
else
 kubectl -n ether035-ha create configmap ether035-ha-source-code --from-file="$root/packaging/kubernetes/source-fixture.py" --dry-run=client -o yaml | kubectl -n ether035-ha apply -f -
fi
python3 "$root/packaging/kubernetes/multihost/fixtures.py" "$root" "$tag" "${fixture_flags[@]}" > "$state/fixtures.yaml"
kubectl -n ether035-ha apply -f "$state/fixtures.yaml"
kubectl -n ether035-ha rollout status statefulset/ether035-ha-pg --timeout=180s
postrender=()
if [[ -s $state/external-private-ip ]]; then
 export MULTIHOST_IP=$(cat "$state/external-private-ip")
 postrender=(--post-renderer "$root/packaging/kubernetes/multihost/external-private.py")
fi
extra_values=()
if [[ -s $state/physical-host.values.yaml ]]; then
 extra_values=(-f "$state/physical-host.values.yaml")
fi
helm upgrade --install ether035-mh "$root/packaging/helm/ether" -n ether035-ha \
 -f "$root/packaging/kubernetes/multihost/values.yaml" "${extra_values[@]}" --set-string "image.tag=$tag" "${postrender[@]}" --wait --timeout 10m
