BIN_DIR := bin
BIN := $(BIN_DIR)/imem
INSTALL_BIN := $(HOME)/.local/bin/imem

.PHONY: build install run up down init test itest search status hooks-json cypher

build:
	go build -o $(BIN) ./cmd/imem

install:
	mkdir -p $(HOME)/.local/bin
	go build -o $(INSTALL_BIN) ./cmd/imem

run: build
	$(BIN) daemon

up:
	docker compose up -d

down:
	docker compose down

init: build
	$(BIN) init

test:
	go test ./...

itest:
	go test -tags=integration ./internal/graph/...

search: build
	$(BIN) search "$(Q)"

status: build
	$(BIN) status

hooks-json: build
	$(BIN) hooks-json

cypher:
	docker compose exec memgraph mgconsole
