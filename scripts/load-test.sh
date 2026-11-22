#!/usr/bin/env sh
set -eu
BASE_URL=${BASE_URL:-http://localhost:10000}
DURATION=${DURATION:-10s}
FORMAT=${FORMAT:-csv}
HEY=${HEY:-hey}
case "$FORMAT" in csv|jsonl) ;; *) echo 'FORMAT must be csv or jsonl' >&2; exit 1;; esac
mkdir -p tmp reports bin
go build -o bin/datagen ./cmd/datagen
bin/datagen --format "$FORMAT" --bytes 1048576 --invalid-every 0 --output "tmp/load.$FORMAT"
SCHEMA=$(tr -d '\r\n' < examples/schema.json)
curl --fail --silent "$BASE_URL/healthz" >/dev/null
run() {
    concurrency=$1
    shift
    if [ -n "${API_TOKEN:-}" ]; then
        "$HEY" -z "$DURATION" -c "$concurrency" -t 60 -m POST -T application/octet-stream -D "tmp/load.$FORMAT" -H "X-Schema: $SCHEMA" -H "Authorization: Bearer $API_TOKEN" "$@" "$BASE_URL/api/validate?format=$FORMAT"
    else
        "$HEY" -z "$DURATION" -c "$concurrency" -t 60 -m POST -T application/octet-stream -D "tmp/load.$FORMAT" -H "X-Schema: $SCHEMA" "$@" "$BASE_URL/api/validate?format=$FORMAT"
    fi
}
for c in 1 2 8; do
    if [ "$c" = 8 ]; then run "$c" -q 25 > "reports/hey-$FORMAT-c$c.txt"; else run "$c" > "reports/hey-$FORMAT-c$c.txt"; fi
    cat "reports/hey-$FORMAT-c$c.txt"
done
curl --fail --silent "$BASE_URL/metrics" > "reports/metrics-after-$FORMAT.txt"
echo 'Count 429 separately: total RPS includes fast rejections.'
