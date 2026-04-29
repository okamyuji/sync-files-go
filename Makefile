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

# 品質ゲート用ツールのバージョン（reproducible に固定）
STATICCHECK_VERSION    ?= 2025.1.1
GOLANGCI_LINT_VERSION  ?= v1.62.0

.PHONY: help build test test-integration lint fmt vet staticcheck golangci-lint \
        tools tools-install tools-check \
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

staticcheck: tools-check ## staticcheck（fail で 1 を返す）
	staticcheck -checks=all,-ST1000 ./...

golangci-lint: tools-check ## golangci-lint run（fail で 1 を返す）
	golangci-lint run ./...

lint: vet staticcheck golangci-lint ## 品質ゲート: vet + staticcheck + golangci-lint を全部走らせる
	@echo "[lint] all gates passed"

tools-install: ## ツールをローカルに固定バージョンでインストール
	$(GO) install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
	@if ! command -v golangci-lint >/dev/null 2>&1 || \
	    [ "$$(golangci-lint --version 2>/dev/null | awk '{print $$4}')" != "$(GOLANGCI_LINT_VERSION:v%=%)" ]; then \
		curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
			| sh -s -- -b $$($(GO) env GOPATH)/bin $(GOLANGCI_LINT_VERSION); \
	fi
	@echo "[tools] installed staticcheck@$(STATICCHECK_VERSION) and golangci-lint@$(GOLANGCI_LINT_VERSION)"

tools-check: ## 必要ツールが PATH に揃っているか確認
	@command -v staticcheck >/dev/null 2>&1 || (echo "✗ staticcheck not found. Run: make tools-install" >&2 && exit 1)
	@command -v golangci-lint >/dev/null 2>&1 || (echo "✗ golangci-lint not found. Run: make tools-install" >&2 && exit 1)

tools: tools-check ## tools-check のエイリアス

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
		-uroot -p"$$MYSQL_ROOT_PASSWORD" sync < <(cat migrations/*.sql)

db-shell: ## ローカル MySQL に CLI 接続
	docker compose -f $(COMPOSE_FILE) exec mysql mysql -usync_app -p"$$MYSQL_PASSWORD" sync

smoke-test: ## healthz へ疎通
	@curl -fsSL $(BASE_URL)/healthz && echo " OK"

clean:
	rm -rf dist coverage.out coverage.html
