#!/usr/bin/env sh
# Requires a running Docker daemon. Exit 2 from the validator is expected.
set -eu
mkdir -p tmp reports bin
go build -o bin/datagen ./cmd/datagen
docker build -t streamcheck .
for format in csv jsonl; do
    bin/datagen --format "$format" --bytes 268435456 --output "tmp/large.$format"
    code=0
    docker run --rm --memory=64m --memory-swap=64m -e GOMEMLIMIT=24MiB \
        -v "$(pwd):/data:ro" streamcheck validate --schema /data/examples/schema.json \
        --input "/data/tmp/large.$format" --format "$format" --samples 0 \
        > "reports/docker-memory-$format.json" 2> "reports/docker-memory-$format.txt" || code=$?
    if [ "$code" -ne 2 ]; then echo "Expected exit 2; got $code" >&2; exit 1; fi
    echo "$format: 256 MiB file processed under 64 MiB container RAM limit"
done
