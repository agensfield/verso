set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

default:
    @just --list

test:
    go test -race ./...

vet:
    go vet ./...

check: test vet

build:
    go build -trimpath -o .build/verso ./cmd/verso
