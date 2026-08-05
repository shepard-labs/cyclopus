#!/bin/sh
# Idempotently register a node as a Beszel "system" so the hub starts polling
# it for metrics — no Beszel UI visit required. Staged by scripts/install.sh to
# /opt/cyclopus/beszel/register_node.sh at install time (so operators can rerun
# it for nodes added later); safe to rerun.
#
# Reads the beszel-auth Secret install.sh created and reaches the hub via its
# in-cluster ClusterIP. This assumes it runs on a cluster node (as install.sh
# and its integration-test guest both do): kube-proxy routes ClusterIP traffic
# in the host's network namespace on any node, so no port-forward is needed.
#
# Usage: register_beszel_node.sh <node-name> <node-ip> [namespace]
set -eu
NODE_NAME="${1:?usage: register_beszel_node.sh <node-name> <node-ip> [namespace]}"
NODE_IP="${2:?usage: register_beszel_node.sh <node-name> <node-ip> [namespace]}"
NAMESPACE="${3:-cyclopus}"
KCTL="k3s kubectl"

CREDENTIAL=$($KCTL -n "$NAMESPACE" get secret beszel-auth -o jsonpath='{.data.credential}' | base64 -d)
IDENTITY=${CREDENTIAL%%:*}
PASSWORD=${CREDENTIAL#*:}
HUB_IP=$($KCTL -n "$NAMESPACE" get svc beszel -o jsonpath='{.spec.clusterIP}')
HUB="http://${HUB_IP}:8090"

TOKEN=$(curl -fsS -X POST "$HUB/api/collections/_superusers/auth-with-password" \
  -H 'Content-Type: application/json' \
  -d "{\"identity\":\"${IDENTITY}\",\"password\":\"${PASSWORD}\"}" \
  | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$TOKEN" ] || { echo "register_beszel_node.sh: Beszel authentication failed" >&2; exit 1; }

EXISTING=$(curl -fsS -G "$HUB/api/collections/systems/records" \
  -H "Authorization: $TOKEN" \
  --data-urlencode "filter=(name='${NODE_NAME}')" \
  | sed -n 's/.*"totalItems":\([0-9]*\).*/\1/p')
if [ "${EXISTING:-0}" != "0" ]; then
  echo "register_beszel_node.sh: ${NODE_NAME} already registered with Beszel"
  exit 0
fi

curl -fsS -X POST "$HUB/api/collections/systems/records" \
  -H "Authorization: $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"name\":\"${NODE_NAME}\",\"host\":\"${NODE_IP}\",\"port\":\"45876\"}" >/dev/null
echo "register_beszel_node.sh: registered ${NODE_NAME} (${NODE_IP}:45876) with Beszel"
