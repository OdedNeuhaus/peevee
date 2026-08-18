#!/usr/bin/env bash
# Installs peevee with every kubeconfig in a directory.
#
#   ./install-from-dir.sh ./kubeconfigs [namespace] [extra helm args…]
#
# Each file becomes one cluster, named after the file. The Secret is created
# directly rather than passed through values.yaml, so credentials never end up
# in Helm's release history or in your shell history.

set -euo pipefail

DIR="${1:?usage: $0 <kubeconfig-dir> [namespace] [helm args…]}"
NAMESPACE="${2:-peevee}"
shift 2 2>/dev/null || shift 1
SECRET="peevee-kubeconfigs"
CHART="$(cd "$(dirname "${BASH_SOURCE[0]}")/../charts/peevee" && pwd)"

[ -d "$DIR" ] || { echo "no such directory: $DIR" >&2; exit 1; }

COUNT=$(find "$DIR" -maxdepth 1 -type f | wc -l)
[ "$COUNT" -gt 0 ] || { echo "no kubeconfig files in $DIR" >&2; exit 1; }

echo "Found $COUNT kubeconfig(s):"
find "$DIR" -maxdepth 1 -type f -printf '  %f\n'

kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

kubectl -n "$NAMESPACE" create secret generic "$SECRET" \
  --from-file="$DIR" --dry-run=client -o yaml | kubectl apply -f -

helm upgrade --install peevee "$CHART" \
  --namespace "$NAMESPACE" \
  --set kubeconfigs.existingSecret="$SECRET" \
  "$@"
