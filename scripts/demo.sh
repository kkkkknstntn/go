#!/usr/bin/env bash
# Walks through the scenarios of ТЗ §55 against a running server.
#
# Start the service with short timeouts first, for example:
#   RESERVATION_TTL=5s PAYMENT_TIMEOUT=10s WORKER_INTERVAL=1s go run ./cmd/tickets
# then:
#   ./scripts/demo.sh                    # one instance on :8080
#   ./scripts/demo.sh :8081 :8082        # two instances on one database
#
# Needs curl and python3. Uses the seeded event-1 and the development
# passwords of the admin and provider accounts.
set -euo pipefail

A="localhost${1:-:8080}"
B="localhost${2:-${1:-:8080}}"
RES_TTL=${RES_TTL:-5}  # seconds, must match RESERVATION_TTL
PAY_TTL=${PAY_TTL:-10} # seconds, must match PAYMENT_TIMEOUT
EVENT=event-1
ORG=concert-company
RUN=$RANDOM

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
field() { # field a.b reads d["a"]["b"] from the JSON on stdin
	python3 -c "
import sys,json
d=json.load(sys.stdin)
for k in sys.argv[1].split('.'): d=d[k]
print(d)" "$1"
}
api() { # api METHOD HOST PATH TOKEN [BODY] -> prints "status body"
	local args=(-s -w '\n%{http_code}' -X "$1" "$2$3")
	[ -n "$4" ] && args+=(-H "Authorization: Bearer $4")
	[ $# -ge 5 ] && args+=(-H 'Content-Type: application/json' -d "$5")
	local out; out=$(curl "${args[@]}")
	echo "$(tail -n1 <<<"$out") $(sed '$d' <<<"$out")"
}
body() { cut -d' ' -f2- <<<"$1"; }
status() { cut -d' ' -f1 <<<"$1"; }
login() {
	# JSON is built outside "$(...)": bash 3.2 (macOS) brace-expands it there.
	local payload="{\"login\":\"$1\",\"password\":\"$2\"}"
	body "$(api POST "$A" /auth/login "" "$payload")" | field accessToken
}
buyer() { # buyer LOGIN BALANCE -> access token
	api POST "$A" /users "" "{\"name\":\"$1\",\"login\":\"$1\",\"password\":\"password-1\",\"balance\":$2}" >/dev/null
	login "$1" password-1
}
balance() { body "$(api GET "$A" /users/me "$1")" | field balance; }
org_balance() { body "$(api GET "$A" /organizers/$ORG "$ADMIN")" | field balance; }
reserve() { api POST "$A" /reservations "$1" "{\"eventId\":\"$EVENT\",\"seatId\":\"$2\"}"; }
free_seat() { # first available seat after the given index
	body "$(api GET "$A" /events/$EVENT/seats "")" | python3 -c "
import sys,json
seats=[s['seat'] for s in json.load(sys.stdin) if s['status']=='available']
print(seats[int(sys.argv[1]) % len(seats)])" "$1"
}
purchase() { body "$(api POST "$B" "/reservations/$1/purchase" "$2")"; }

ADMIN=$(login admin admin-dev-password)
PROVIDER=$(login provider provider-dev-password)

step "1. A buyer with a test balance (POST /users, then POST /auth/login)"
IVAN=$(buyer "ivan-$RUN" 10000)
body "$(api GET "$A" /users/me "$IVAN")"

step "2-3. Events and seats with prices (public)"
body "$(api GET "$A" /events "")" | python3 -c "import sys,json; print([e['id'] for e in json.load(sys.stdin)])"
body "$(api GET "$A" /events/$EVENT/seats "")" | python3 -c "import sys,json; print(json.load(sys.stdin)[:3], '...')"

step "Authorization: no token -> 401, buyer calling the payment callback -> 403"
echo "GET /users/me without token: $(status "$(api GET "$A" /users/me "")")"
echo "POST /payments/x/confirm as buyer: $(status "$(api POST "$A" /payments/x/confirm "$IVAN")")"

step "4-5. 100 buyers reserve the same seat at once (both instances)"
SEAT=$(free_seat 0)
TOKENS=()
for i in $(seq 1 100); do TOKENS+=("$(buyer "racer-$RUN-$i" 0)"); done
TMP=$(mktemp -d)
for i in $(seq 0 99); do
	host=$A; [ $((i % 2)) -eq 1 ] && host=$B
	curl -s -o /dev/null -w '%{http_code}\n' -X POST "$host/reservations" -H "Authorization: Bearer ${TOKENS[$i]}" \
		-d "{\"eventId\":\"$EVENT\",\"seatId\":\"$SEAT\"}" >"$TMP/$i" &
done
wait
echo "seat $SEAT:"; cat "$TMP"/* | sort | uniq -c; rm -rf "$TMP"

step "6. Expiration: the reservation TTL ($RES_TTL s) passes, the seat is free again"
SEAT=$(free_seat 1)
R=$(body "$(reserve "$IVAN" "$SEAT")"); echo "$R"
sleep $((RES_TTL + 1))
echo "reservation now: $(body "$(api GET "$A" "/reservations/$(field reservationId <<<"$R")" "$IVAN")" | field status)"

step "7. Cancel"
SEAT=$(free_seat 2)
RID=$(body "$(reserve "$IVAN" "$SEAT")" | field reservationId)
body "$(api POST "$A" "/reservations/$RID/cancel" "$IVAN")" | field status

step "8-11. Start purchase: payment_pending; balances before"
SEAT=$(free_seat 3)
RID=$(body "$(reserve "$IVAN" "$SEAT")" | field reservationId)
P=$(purchase "$RID" "$IVAN"); echo "$P"
PID=$(field paymentId <<<"$P")
echo "user before: $(balance "$IVAN"), organizer before: $(org_balance)"

step "18. Delayed confirmation: wait past the reservation TTL, still within the payment timeout"
sleep $((RES_TTL + 1))
step "12-15. Confirm (as the payment provider): money moves, purchased"
body "$(api POST "$A" "/payments/$PID/confirm" "$PROVIDER")" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['status'], d['purchaseStatus'])"
echo "user after: $(balance "$IVAN"), organizer after: $(org_balance)"

step "21-22. Duplicate confirm: 200, no second debit"
echo "status: $(status "$(api POST "$B" "/payments/$PID/confirm" "$PROVIDER")"), user: $(balance "$IVAN"), organizer: $(org_balance)"

step "16-17. Insufficient funds: 402, payment failed, nothing debited, seat free"
POOR=$(buyer "poor-$RUN" 500)
SEAT=$(free_seat 4)
RID=$(body "$(reserve "$POOR" "$SEAT")" | field reservationId)
PID=$(purchase "$RID" "$POOR" | field paymentId)
OUT=$(api POST "$A" "/payments/$PID/confirm" "$PROVIDER")
echo "status $(status "$OUT"): $(body "$OUT" | field error), payment $(body "$OUT" | field payment.status), balance $(balance "$POOR")"

step "19-20. Payment timeout, then a late confirmation is rejected"
SEAT=$(free_seat 5)
RID=$(body "$(reserve "$IVAN" "$SEAT")" | field reservationId)
PID=$(purchase "$RID" "$IVAN" | field paymentId)
BEFORE=$(balance "$IVAN")
sleep $((PAY_TTL + 1))
OUT=$(api POST "$A" "/payments/$PID/confirm" "$PROVIDER")
echo "status $(status "$OUT"): $(body "$OUT" | field error); balance $BEFORE -> $(balance "$IVAN")"

step "23-24. Two concurrent confirms, balance enough for one"
PETR=$(buyer "petr-$RUN" 2000)
S1=$(free_seat 6); R1=$(body "$(reserve "$PETR" "$S1")" | field reservationId)
S2=$(free_seat 7); R2=$(body "$(reserve "$PETR" "$S2")" | field reservationId)
P1=$(purchase "$R1" "$PETR" | field paymentId)
P2=$(purchase "$R2" "$PETR" | field paymentId)
ORG_BEFORE=$(org_balance)
TMP=$(mktemp -d)
curl -s -o /dev/null -w '%{http_code}\n' -X POST "$A/payments/$P1/confirm" -H "Authorization: Bearer $PROVIDER" >"$TMP/1" &
curl -s -o /dev/null -w '%{http_code}\n' -X POST "$B/payments/$P2/confirm" -H "Authorization: Bearer $PROVIDER" >"$TMP/2" &
wait
echo "statuses: $(cat "$TMP"/* | sort | tr '\n' ' ')(200 = succeeded, 402 = insufficient_funds)"; rm -rf "$TMP"
echo "balance: $(balance "$PETR"), organizer +$(($(org_balance) - ORG_BEFORE))"

step "History of a reservation"
body "$(api GET "$A" "/reservations/$R1/history" "$PETR")" | python3 -c "
import sys,json
for t in json.load(sys.stdin): print(' ', t['at'][11:23], t['entity'].ljust(11), (t.get('from') or '-').rjust(15), '->', t['to'].ljust(15), t.get('reason',''))"

step "25-27. Races (expire/cancel/timeout vs purchase/confirm): see the tests"
echo "go test -race -run 'TestExpireVsPurchase|TestCancelVsPurchase|TestTimeoutVsConfirm' -v ./internal/booking/"
step "28. Restart: stop the server (Ctrl+C), start it again and query the ids above - nothing is lost"
