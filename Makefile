BIN_DIR := bin
BIN := $(BIN_DIR)/imem
INSTALL_BIN := $(HOME)/.local/bin/imem

.PHONY: build install run up down init test itest itest-down search status hooks-json cypher backup backups restore

build:
	go build -o $(BIN) ./cmd/imem

# Build beside the target and rename: the agents (~/scratch) exec this binary
# every run, and must never catch a half-written file.
install:
	mkdir -p $(HOME)/.local/bin
	go build -o $(INSTALL_BIN).tmp ./cmd/imem
	mv -f $(INSTALL_BIN).tmp $(INSTALL_BIN)

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

# A fresh container per run: the tests never clean up, and fixtures piling up
# across runs made limit-bounded queries flaky.
itest:
	docker compose --profile test rm -sfv memgraph-test >/dev/null 2>&1 || true
	docker compose --profile test up -d memgraph-test
	go test -count=1 -tags=integration ./internal/graph/...

itest-down:
	docker compose --profile test rm -sfv memgraph-test

search: build
	$(BIN) search "$(Q)"

status: build
	$(BIN) status

hooks-json: build
	$(BIN) hooks-json

cypher:
	docker compose exec memgraph mgconsole

backup: build
	$(BIN) backup

backups: build
	$(BIN) backups

restore: build
	$(BIN) restore "$(F)" --yes
