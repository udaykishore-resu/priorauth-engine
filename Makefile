SHELL := /bin/bash
MODULE := github.com/udaykishore-resu/priorauth-engine
BIN_DIR := bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w
IMAGE ?= ghcr.io/udaykishore-resu/priorauth-engine:$(VERSION)
GOFLAGS ?=

.PHONY: all build run run-sim run-full test test-short cover lint vet tidy gen docker helm-lint clean demo

all: build

## build: compile both binaries into ./bin
build:
	CGO_ENABLED=0 go build $(GOFLAGS) -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/priorauth ./cmd/priorauth
	CGO_ENABLED=0 go build $(GOFLAGS) -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/payer-sim ./cmd/payer-sim

## run: start the engine with in-memory storage, in-process payer simulator and no other dependencies
run: build
	PA_STORAGE=memory PA_EVENTS=memory PA_PAYER=sim PA_RULES_DIR=./rules PA_VERSION=$(VERSION) $(BIN_DIR)/priorauth

## run-sim: start the standalone payer simulator on :8081
run-sim: build
	SIM_HTTP_ADDR=:8081 $(BIN_DIR)/payer-sim

## run-full: bring up postgres, kafka, otel-collector and payer-sim with docker compose, then run the engine against them
run-full: build
	docker compose -f deploy/docker-compose.yaml up -d postgres kafka otel-collector payer-sim
	PA_STORAGE=postgres PA_POSTGRES_DSN='postgres://priorauth:priorauth@localhost:5432/priorauth?sslmode=disable' \
	PA_EVENTS=kafka PA_KAFKA_BROKERS=localhost:9092 \
	PA_PAYER=http PA_PAYER_BASE_URL=http://localhost:8081 \
	PA_OTLP_ENDPOINT=http://localhost:4318 PA_RULES_DIR=./rules PA_VERSION=$(VERSION) $(BIN_DIR)/priorauth

## test: race-enabled unit tests
test:
	go test -race -count=1 -p 1 ./...

## test-short: fast tests without race
test-short:
	go test -count=1 ./...

## cover: coverage report (domain packages are the ones that matter)
cover:
	go test -race -count=1 -p 1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -n 1
	@echo "domain packages:"; go tool cover -func=coverage.out | grep -E 'internal/domain' | awk '{s+=$$3; n++} END {if (n>0) printf "  avg %.1f%% over %d functions\n", s/n, n}'

## lint: golangci-lint (install: https://golangci-lint.run)
lint:
	golangci-lint run ./...

## vet: go vet
vet:
	go vet ./...

## tidy: go mod tidy and verify
tidy:
	go mod tidy
	go mod verify

## gen: nothing to generate today (kept for parity with the other repos)
gen:
	@echo "no generated code"

## docker: build the multi-stage image
docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

## helm-lint: lint and render the chart
helm-lint:
	helm lint deploy/helm/priorauth-engine
	helm template priorauth deploy/helm/priorauth-engine > /dev/null

## demo: run the README quick-start against a running instance (BASE_URL defaults to :8080)
demo:
	./examples/demo.sh

clean:
	rm -rf $(BIN_DIR) coverage.out

help:
	@grep -E '^## ' Makefile | sed 's/## //'
