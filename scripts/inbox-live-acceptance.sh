#!/usr/bin/env bash
# Live acceptance for the inbox redesign (spec §9.5.6). Drives a RUNNING
# factory with curl and checks the four operator-visible behaviors:
#   (a) operator chat opens a task
#   (b) a planner reply lands in that task's thread
#   (c) a job-attached foreman.report is filed under its task and hidden by default
#   (d) a foreman raise_alert shows in GET /v1/alerts and acks
#
# Usage:  FOREMAN_ID=<bare id> FOREMAN_BEARER=<foreman credential> \
#           scripts/inbox-live-acceptance.sh [factory-url]
#   factory-url defaults to $FACTORY_URL, then http://127.0.0.1:8080.
#   FOREMAN_BEARER is the paired foreman's bearer (its paired.json), used only
#   for (c) and (d): it can post to factory:main, exactly as the foreman
#   brain's post_to_factory / raise_alert tools do. Without it (c)/(d) are
#   skipped. PLANNER_WAIT (seconds, default 180) bounds step (b): a real LLM
#   planner session answers the chat.
# Exit 0 = every run step passed; the script creates a few test tasks and
# alerts (title prefix "acceptance-<ts>"); nothing is deleted.
set -u
BASE="${1:-${FACTORY_URL:-http://127.0.0.1:8080}}"
FID="${FOREMAN_ID:-}"
FB="${FOREMAN_BEARER:-}"
WAIT="${PLANNER_WAIT:-180}"
TAG="acceptance-$(date +%s)"
fail=0

jget() { python3 -I -c 'import json,sys
d=json.load(sys.stdin)
for k in sys.argv[1].split("."):
    d=d[int(k)] if k.isdigit() else d[k]
print(d if not isinstance(d,(dict,list)) else json.dumps(d))' "$1"; }
ok()  { echo "PASS  $*"; }
bad() { echo "FAIL  $*"; fail=1; }
step() { echo; echo "== $*"; }

# thread <task> [internal]  -> JSON on stdout
thread() { curl -fsS "$BASE/v1/tasks/$1${2:+?internal=1}"; }
types() { python3 -I -c 'import json,sys
print(" ".join(m["type"] for m in json.load(sys.stdin)["messages"]))'; }

# real_reply: stdin is a thread JSON; true if it holds a brain.reply that is
# NOT the factory's "planner could not run" failure note.
real_reply() { python3 -I -c 'import json,sys
ms=json.load(sys.stdin)["messages"]
sys.exit(0 if any(m["type"]=="brain.reply" and "could not run" not in json.dumps(m["payload"]) for m in ms) else 1)'; }

step "(a) operator chat opens a task"
R=$(curl -sS -w '\n%{http_code}' -X POST "$BASE/v1/chat" -H 'Content-Type: application/json' \
  -d "{\"text\":\"$TAG: please reply 'pong' and do nothing else\"}")
CODE=${R##*$'\n'}; BODY=${R%$'\n'*}
TASK=$(printf '%s' "$BODY" | jget task_id 2>/dev/null || true)
if [ "$CODE" = 201 ] && [[ "$TASK" == task_* ]]; then ok "POST /v1/chat -> 201, task $TASK"; else bad "chat: HTTP $CODE $BODY"; echo "cannot continue"; exit 1; fi
if curl -fsS "$BASE/v1/tasks?status=open&q=$TAG" | grep -q "$TASK"; then ok "task listed in GET /v1/tasks?status=open"; else bad "task not in open list"; fi

step "(b) planner reply lands in the thread (waiting up to ${WAIT}s)"
got=""
for ((i = 0; i < WAIT; i += 3)); do
  if thread "$TASK" | real_reply; then got=1; break; fi
  sleep 3
done
if [ -n "$got" ]; then ok "brain.reply present in GET /v1/tasks/$TASK"; else bad "no brain.reply in $TASK after ${WAIT}s (journalctl -u factory; a planner failure raises a P0 alert)"; fi

step "(c) job-attached foreman.report is filed under its task and hidden by default"
if [ -z "$FB" ] || [ -z "$FID" ]; then
  echo "SKIP  set FOREMAN_ID and FOREMAN_BEARER to run (c) and (d)"
else
  J=$(curl -sS -X POST "$BASE/v1/jobs" -H 'Content-Type: application/json' \
    -d "{\"task\":\"$TAG: acceptance job (never runs anything important)\",\"task_id\":\"$TASK\"}")
  JOB=$(printf '%s' "$J" | jget job_id 2>/dev/null || true)
  if [[ "$JOB" == job_* ]]; then ok "POST /v1/jobs task_id=$TASK -> $JOB"; else bad "create job: $J"; fi
  CODE=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/inboxes/factory:main/messages" \
    -H "Authorization: Bearer $FB" -H 'Content-Type: application/json' \
    -d "{\"type\":\"foreman.report\",\"job_id\":\"$JOB\",\"from\":\"foreman:$FID\",\"payload\":{\"text\":\"$TAG report\"}}")
  [ "$CODE" = 201 ] && ok "foreman.report posted (201)" || bad "post foreman.report: HTTP $CODE"
  sleep 2
  if thread "$TASK" 1 | types | grep -qw foreman.report; then ok "report present with ?internal=1"; else bad "report missing from internal view"; fi
  if thread "$TASK" | types | grep -qw foreman.report; then bad "report VISIBLE in the default view"; else ok "report hidden in the default view"; fi
  if curl -fsS "$BASE/v1/alerts" | grep -q "Unattributed"; then bad "attributed report raised an 'Unattributed' alert"; else ok "no unattributed alert"; fi

  step "(d) foreman raise_alert shows in GET /v1/alerts and acks"
  CODE=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/inboxes/factory:main/messages" \
    -H "Authorization: Bearer $FB" -H 'Content-Type: application/json' \
    -d "{\"type\":\"alert.raise\",\"from\":\"foreman:$FID\",\"payload\":{\"priority\":3,\"title\":\"$TAG alert\",\"body\":\"live acceptance\",\"ttl_s\":600}}")
  [ "$CODE" = 201 ] && ok "alert.raise posted (201)" || bad "post alert.raise: HTTP $CODE"
  AID=""
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    AID=$(curl -fsS "$BASE/v1/alerts" | python3 -I -c 'import json,sys
for a in json.load(sys.stdin)["alerts"]:
    if a["title"].endswith(sys.argv[1]+" alert"): print(a["id"]); break' "$TAG")
    [ -n "$AID" ] && break; sleep 1
  done
  if [ -n "$AID" ]; then ok "alert $AID listed (open, source foreman:$FID)"; else bad "alert not in GET /v1/alerts"; fi
  if [ -n "$AID" ]; then
    CODE=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/alerts/$AID/ack")
    [ "$CODE" = 200 ] && ok "ack -> 200" || bad "ack: HTTP $CODE"
    CODE=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/alerts/$AID/ack")
    [ "$CODE" = 409 ] && ok "second ack -> 409 already_terminal" || bad "second ack: HTTP $CODE (want 409)"
    if curl -fsS "$BASE/v1/alerts" | grep -q "$AID"; then bad "acked alert still listed"; else ok "acked alert left GET /v1/alerts"; fi
  fi
fi

echo
[ "$fail" = 0 ] && echo "ALL PASSED" || echo "FAILURES (see above)"
exit "$fail"
