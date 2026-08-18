#!/usr/bin/env bash
# Creates a least-privilege kubeconfig for peevee to observe one cluster.
#
# Run this against each cluster you want observed, with admin credentials for
# that cluster. It creates a ServiceAccount with read-only access plus the
# nodes/proxy permission that kubelet statistics require, then writes a
# kubeconfig you can hand to peevee.
#
#   ./create-remote-kubeconfig.sh prod-eu > kubeconfigs/prod-eu.yaml
#
# The API server URL is taken from your current context. If that URL is not
# reachable from wherever peevee runs — a kubeconfig saying 127.0.0.1, or a
# private address behind a bastion — override it:
#
#   SERVER=https://prod-eu.k8s.example.com:6443 \
#     ./create-remote-kubeconfig.sh prod-eu > kubeconfigs/prod-eu.yaml
#
# The output file's name becomes the cluster name in the UI and in the
# `cluster` metric label, so name it deliberately and keep it stable.

set -euo pipefail

CLUSTER_NAME="${1:?usage: $0 <cluster-name> [namespace] [token-duration]}"
NAMESPACE="${2:-peevee}"
DURATION="${3:-8760h}"   # one year; shorten it if your cluster caps token lifetime
SA="peevee-reader"

>&2 echo "Creating ServiceAccount ${NAMESPACE}/${SA} in the current context…"
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >&2

kubectl apply -f - >&2 <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${SA}
  namespace: ${NAMESPACE}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: peevee-reader
rules:
  # Inventory: which claims exist, how big they were asked to be, and which
  # pods mount them.
  - apiGroups: [""]
    resources: [persistentvolumeclaims, persistentvolumes, pods, nodes, namespaces]
    verbs: [get, list]
  # The one that matters: reading kubelet's /stats/summary endpoint through the
  # API server proxy is where filesystem usage actually comes from.
  - apiGroups: [""]
    resources: [nodes/proxy]
    verbs: [get]
  - apiGroups: [storage.k8s.io]
    resources: [storageclasses]
    verbs: [get, list]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: peevee-reader
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: peevee-reader
subjects:
  - kind: ServiceAccount
    name: ${SA}
    namespace: ${NAMESPACE}
EOF

>&2 echo "Minting a ${DURATION} token…"
TOKEN="$(kubectl -n "$NAMESPACE" create token "$SA" --duration="$DURATION")"

SERVER="${SERVER:-$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.server}')}"

case "$SERVER" in
  *127.0.0.1*|*localhost*)
    >&2 echo
    >&2 echo "WARNING: the API server URL is ${SERVER}."
    >&2 echo "         That address means 'this pod' once peevee is running, not this cluster."
    >&2 echo "         Re-run with SERVER=https://<reachable-address>:6443 set, or edit the"
    >&2 echo "         generated file. If peevee runs inside this same cluster, use"
    >&2 echo "         SERVER=https://kubernetes.default.svc:443"
    >&2 echo
    ;;
esac
CA="$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"

if [ -z "$CA" ]; then
  # Some kubeconfigs reference a CA file on disk rather than inlining it.
  CA_FILE="$(kubectl config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority}')"
  if [ -n "$CA_FILE" ] && [ -r "$CA_FILE" ]; then
    CA="$(base64 -w0 < "$CA_FILE")"
  else
    >&2 echo "WARNING: no CA certificate found. peevee will not be able to verify this API server."
    >&2 echo "         Set clusters[].insecureSkipTlsVerify for ${CLUSTER_NAME}, or add the CA by hand."
  fi
fi

cat <<EOF
apiVersion: v1
kind: Config
current-context: ${CLUSTER_NAME}
clusters:
  - name: ${CLUSTER_NAME}
    cluster:
      server: ${SERVER}
$([ -n "$CA" ] && echo "      certificate-authority-data: ${CA}")
contexts:
  - name: ${CLUSTER_NAME}
    context:
      cluster: ${CLUSTER_NAME}
      user: peevee
users:
  - name: peevee
    user:
      token: ${TOKEN}
EOF

>&2 echo "Done. Save this as kubeconfigs/${CLUSTER_NAME}.yaml"
