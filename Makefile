# sync-files-go Makefile
#
# 主要ターゲット:
#   make build              -- Go ビルド (dist/sync-files-go)
#   make test               -- 単体テスト
#   make test-integration   -- testcontainers-go 統合テスト
#   make lint               -- staticcheck + go vet
#   make docker-build       -- app と nginx のイメージを arm64 でビルド
#   make compose-up         -- ローカル一式 (mysql + app + nginx) を起動
#   make compose-down       -- 停止
#   make db-migrate         -- マイグレーション適用 (ローカル)
#   make smoke-test         -- /healthz への curl 確認
#
# 環境変数:
#   TAG                     -- Docker イメージタグ (default: dev)
#   BASE_URL                -- smoke-test の対象 (default: http://localhost:8080)

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

GO            ?= go
TAG           ?= dev
BASE_URL      ?= http://localhost:8080
COMPOSE_FILE  := deploy/docker/docker-compose.yml

.PHONY: help build test test-integration lint fmt vet \
        docker-build docker-build-app docker-build-nginx \
        compose-up compose-down compose-logs \
        db-migrate db-shell smoke-test clean

help:
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | sort | awk -F'[: ]+##[ ]?' '{printf "  %-22s %s\n", $$1, $$2}'

build: ## Go の本体バイナリをビルド
	mkdir -p dist
	$(GO) build -trimpath -ldflags="-s -w" -o dist/sync-files-go ./cmd/server
	$(GO) build -trimpath -ldflags="-s -w" -o dist/sync-files-batch ./cmd/batch || true
	$(GO) build -trimpath -ldflags="-s -w" -o dist/sync-files-admin ./cmd/sync-files-admin || true

test: ## 単体テスト
	$(GO) test -race -count=1 ./...

test-integration: ## 統合テスト (testcontainers-go)
	$(GO) test -race -count=1 -tags=integration ./tests/integration/...

fmt: ## go fmt
	$(GO) fmt ./...

vet: ## go vet
	$(GO) vet ./...

lint: vet ## staticcheck + vet
	@if command -v staticcheck >/dev/null 2>&1; then \
		staticcheck ./...; \
	else \
		echo "[lint] staticcheck not installed, skip (install: go install honnef.co/go/tools/cmd/staticcheck@latest)"; \
	fi

docker-build: docker-build-app docker-build-nginx ## app と nginx の両方をビルド

docker-build-app:
	docker buildx build --platform=linux/arm64 \
		-f deploy/docker/app.Dockerfile \
		-t sync-files-go/app:$(TAG) \
		--load .

docker-build-nginx:
	docker buildx build --platform=linux/arm64 \
		-f deploy/docker/nginx.Dockerfile \
		-t sync-files-go/nginx:$(TAG) \
		--load .

compose-up: ## ローカル一式起動
	docker compose -f $(COMPOSE_FILE) up -d --build
	@echo "→ Logs:        make compose-logs"
	@echo "→ Smoke test:  make smoke-test"
	@echo "→ Web UI:      http://localhost:8080  (nginx 経由は https://localhost:8443)"

compose-down: ## 停止
	docker compose -f $(COMPOSE_FILE) down -v

compose-logs:
	docker compose -f $(COMPOSE_FILE) logs -f --tail=200

db-migrate: ## migrations/ を MySQL コンテナに適用
	docker compose -f $(COMPOSE_FILE) exec -T mysql mysql \
		-uroot -prootdev sync < <(cat migrations/*.sql)

db-shell: ## ローカル MySQL に CLI 接続
	docker compose -f $(COMPOSE_FILE) exec mysql mysql -usync_app -pdev sync

smoke-test: ## healthz へ疎通
	@curl -fsSL $(BASE_URL)/healthz && echo " OK"

clean:
	rm -rf dist coverage.out coverage.html
