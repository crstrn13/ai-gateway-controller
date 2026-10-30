#!/usr/bin/env bash
#
# ext-proc idle-connection keepalive repro (client-level A/B) on the Kind env.
#
# BACKGROUND
#   The gateway holds a long-lived, pooled HTTP/2 connection to ext-proc. When a
#   middlebox (cloud LB / OVN) silently evicts that idle flow, Envoy keeps reusing
#   the dead connection and client requests hang/fail -- even though ext-proc is
#   perfectly healthy. HTTP/2 connection_keepalive makes Envoy ping the idle
#   connection, notice it is dead, and open a fresh one instead.
#
#   Kind has no such middlebox, so we reproduce the eviction with the REAL
#   mechanism: a per-FLOW packet DROP via iptables on the node (exactly what an LB
#   does when it blackholes an evicted flow), matched by the connection's source
#   port so that NEW connections are left untouched.
#
# RESULT (same cluster, same healthy ext-proc; only keepalive differs)
#   OFF (no keepalive): client hangs -> 000/timeout  (Envoy reuses the dead flow)
#   ON  (keepalive):    client 200/401 fast          (Envoy recycles to a fresh flow)
#
# PREREQS
#   AIGC Kind env provisioned:  test/kind-env/run.sh --provision
#   Runs conntrack/iptables inside the Kind node via `docker exec`.
#
# USAGE
#   ./chaos-keepalive-repro.sh
#
set -uo pipefail

# ---------------------------------------------------------------------------
# Configuration (override via environment)
# ---------------------------------------------------------------------------
CTX=${CTX:-kind-external-model-two-plane}          # kube context
NS=${NS:-maas-system}                              # gateway + ext-proc namespace
AIGC_NS=${AIGC_NS:-opendatahub}                    # ai-gateway-controller namespace
EF=${EF:-payload-processing}                       # EnvoyFilter holding the ext-proc clusters
NODE=${NODE:-external-model-two-plane-control-plane}  # Kind node container
MODEL_PATH=${MODEL_PATH:-/models-as-a-service/demo/v1/chat/completions}

GW=$(kubectl --context "$CTX" -n "$NS" get pod \
      -l gateway.networking.k8s.io/gateway-name=maas-default-gateway \
      -o name | grep -v Terminating | head -1)
GWIP=$(kubectl --context "$CTX" -n "$NS" get "$GW" -o jsonpath='{.status.podIP}')

DROPPED=/tmp/kr-dropped-sports   # source ports we blackholed, for cleanup
PF=""                            # current port-forward pid

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Sleep without relying on the `sleep` binary.
nap() { python3 -c "import time,sys; time.sleep(float(sys.argv[1]))" "$1"; }

# Envoy's live connection count for the ext-proc cluster.
cx_active() {
  kubectl --context "$CTX" -n "$NS" exec "$GW" -c istio-proxy -- \
    pilot-agent request GET /clusters 2>/dev/null \
    | grep -E "payload-pre-processing-extproc::.*cx_active::" \
    | sed -E 's#.*::##' | head -1
}

# One client request through the gateway; prints "HTTP <code> in <secs>s".
# A 401 is fine -- it still traverses ipp-pre (which runs before auth).
client_request() {
  kubectl --context "$CTX" -n "$NS" port-forward "$GW" 18080:80 >/tmp/kr-pf.log 2>&1 &
  PF=$!
  nap 3
  local result
  result=$(curl -s -o /dev/null -w 'HTTP %{http_code} in %{time_total}s' --max-time 20 \
    "http://127.0.0.1:18080${MODEL_PATH}" \
    -H 'content-type: application/json' \
    -d '{"model":"demo","messages":[{"role":"user","content":"hi"}]}' 2>/dev/null)
  kill "$PF" 2>/dev/null
  PF=""
  echo "$result"
}

# Turn keepalive on/off on the ext-proc clusters in the live EnvoyFilter.
set_keepalive() {
  kubectl --context "$CTX" -n "$NS" get envoyfilter "$EF" -o yaml > /tmp/kr-ef.yaml
  MODE="$1" python3 - <<'PY'
import os, yaml
mode = os.environ["MODE"]
d = yaml.safe_load(open("/tmp/kr-ef.yaml"))

# strip server-managed fields so the doc re-applies cleanly
for k in ("creationTimestamp", "resourceVersion", "uid", "generation", "managedFields"):
    d["metadata"].pop(k, None)
d["metadata"].get("annotations", {}).pop("kubectl.kubernetes.io/last-applied-configuration", None)
d.pop("status", None)

for patch in d["spec"].get("configPatches", []):
    if patch.get("applyTo") != "CLUSTER":
        continue
    value = patch["patch"]["value"]
    http = (value.get("typed_extension_protocol_options", {})
                 .get("envoy.extensions.upstreams.http.v3.HttpProtocolOptions", {})
                 .get("explicit_http_config", {}))
    if "http2_protocol_options" not in http:
        continue
    if mode == "on":
        http["http2_protocol_options"] = {
            "connection_keepalive": {"interval": "5s", "timeout": "5s"}}
        value["upstream_connection_options"] = {
            "tcp_keepalive": {"keepalive_time": 120, "keepalive_interval": 30, "keepalive_probes": 3}}
    else:
        http["http2_protocol_options"] = {}
        value.pop("upstream_connection_options", None)

yaml.safe_dump(d, open("/tmp/kr-ef-patched.yaml", "w"), sort_keys=False)
PY
  kubectl --context "$CTX" -n "$NS" apply -f /tmp/kr-ef-patched.yaml >/dev/null
  nap 12   # let istiod push the updated cluster
}

# ---------------------------------------------------------------------------
# Fault injection: per-flow packet DROP on the node (the real "middlebox" blackhole)
# ---------------------------------------------------------------------------

# Source ports of ESTABLISHED gateway->ext-proc flows, from the node's conntrack.
# Anchored to "sport=<n> dport=9004" so we take the ORIGINAL direction only
# (not the reply tuple, whose sport is 9004).
established_sports() {
  docker exec "$NODE" conntrack -L -p tcp --dport 9004 2>/dev/null \
    | grep ESTABLISHED | grep "src=$GWIP " \
    | sed -nE 's/.* sport=([0-9]+) dport=9004 .*/\1/p' | sort -u
}

# Blackhole each established flow by dropping its packets on the node.
blackhole_flows() {
  : > "$DROPPED"
  local sport
  for sport in $(established_sports); do
    if docker exec "$NODE" iptables -I FORWARD -p tcp -s "$GWIP" --sport "$sport" -j DROP 2>/dev/null; then
      echo "$sport" >> "$DROPPED"
    fi
  done
  tr '\n' ' ' < "$DROPPED"
}

# Remove the DROP rules we added.
heal_flows() {
  [ -f "$DROPPED" ] || return 0
  local sport
  for sport in $(cat "$DROPPED"); do
    docker exec "$NODE" iptables -D FORWARD -p tcp -s "$GWIP" --sport "$sport" -j DROP 2>/dev/null || true
  done
  : > "$DROPPED"
}

# ---------------------------------------------------------------------------
# One arm of the experiment
# ---------------------------------------------------------------------------
run_arm() {
  local mode=$1 label
  [ "$mode" = on ] && label="KEEPALIVE ON  (fix)" || label="KEEPALIVE OFF (bug)"
  echo "== $label =="

  set_keepalive "$mode"

  # Warm a pooled gateway->ext-proc connection.
  client_request >/dev/null
  nap 1
  echo "  warmed:          cx_active=$(cx_active)  client=[$(client_request)]"

  # Blackhole that flow (simulate the middlebox evicting an idle flow).
  echo "  blackholed flow sport(s): $(blackhole_flows)(per-flow DROP; new conns unaffected)"

  # Keepalive needs a moment to ping, detect the dead flow, and recycle.
  if [ "$mode" = on ]; then
    echo "  waiting 10s for keepalive to detect + recycle..."
    nap 10
  fi

  echo "  after blackhole: cx_active=$(cx_active)  client=[$(client_request)]"
  heal_flows
  echo
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
cleanup() {
  [ -n "$PF" ] && kill "$PF" 2>/dev/null || true
  heal_flows
  # restore the controller we paused so it does not revert our EnvoyFilter edits
  kubectl --context "$CTX" -n "$AIGC_NS" scale deploy ai-gateway-controller --replicas=1 >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "gateway=${GW##*/} gw-ip=$GWIP node=$NODE"

# Pause the AIGC controller so it does not overwrite our keepalive edits mid-test.
kubectl --context "$CTX" -n "$AIGC_NS" scale deploy ai-gateway-controller --replicas=0 >/dev/null 2>&1 || true
nap 2

run_arm off
run_arm on

echo "Expected:"
echo "  OFF -> client 000/timeout (Envoy reused the blackholed flow)"
echo "  ON  -> client 200/401     (Envoy recycled to a fresh flow; same healthy ext-proc)"
