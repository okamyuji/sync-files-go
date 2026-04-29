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

# UI ベンダー JS （サードパーティ JS のためリポジトリ未コミット、make ui-vendor で取得）
HTMX_VERSION          ?= 2.0.4
HTMX_SSE_VERSION      ?= 2.2.2
HTMX_SHA256           ?= 64f81e5dee6e4b9d56d6a6bb710c47f4f5a39f9d63ccd64a99ad4c63b7e5d7e8
UI_VENDOR_DIR         := internal/ui/static/js

.PHONY: help build test test-integration test-e2e e2e-install lint fmt vet staticcheck golangci-lint \
        tools tools-install tools-check ui-vendor ui-vendor-check \
        docker-build docker-build-app docker-build-nginx \
        compose-up compose-down compose-logs \
        db-migrate db-shell smoke-test clean \
        tf-fmt tf-validate tf-plan-dev tf-plan-prod release-smoke release-critical-path

help:
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | sort | awk -F'[: ]+##[ ]?' '{printf "  %-22s %s\n", $$1, $$2}'

build: ui-vendor-check ## Go の本体バイナリをビルド
	mkdir -p dist
	$(GO) build -trimpath -ldflags="-s -w" -o dist/sync-files-go ./cmd/server
	$(GO) build -trimpath -ldflags="-s -w" -o dist/sync-files-batch ./cmd/batch || true
	$(GO) build -trimpath -ldflags="-s -w" -o dist/sync-files-admin ./cmd/sync-files-admin || true

ui-vendor: ## サードパーティ JS (HTMX + sse 拡張) を $(UI_VENDOR_DIR) に取得
	mkdir -p $(UI_VENDOR_DIR)
	@if [ ! -f "$(UI_VENDOR_DIR)/htmx.min.js" ]; then \
		echo "[ui-vendor] downloading htmx.org@$(HTMX_VERSION)"; \
		curl -fsSL -o $(UI_VENDOR_DIR)/htmx.min.js \
			https://unpkg.com/htmx.org@$(HTMX_VERSION)/dist/htmx.min.js; \
	fi
	@if [ ! -f "$(UI_VENDOR_DIR)/htmx-ext-sse.js" ]; then \
		echo "[ui-vendor] downloading htmx-ext-sse@$(HTMX_SSE_VERSION)"; \
		curl -fsSL -o $(UI_VENDOR_DIR)/htmx-ext-sse.js \
			https://unpkg.com/htmx-ext-sse@$(HTMX_SSE_VERSION)/sse.js; \
	fi
	@echo "[ui-vendor] sha256:"
	@shasum -a 256 $(UI_VENDOR_DIR)/htmx.min.js $(UI_VENDOR_DIR)/htmx-ext-sse.js

ui-vendor-check: ## ベンダー JS が存在することを確認（無ければ案内）
	@for f in htmx.min.js htmx-ext-sse.js; do \
		if [ ! -f "$(UI_VENDOR_DIR)/$$f" ]; then \
			echo "✗ missing $(UI_VENDOR_DIR)/$$f. Run: make ui-vendor" >&2; \
			exit 1; \
		fi; \
	done
	@echo "[ui-vendor-check] ok"

test: ## 単体テスト
	$(GO) test -race -count=1 ./...

test-integration: ## 統合テスト (testcontainers-go)
	$(GO) test -race -count=1 -tags=integration ./tests/integration/...

e2e-install: ## Playwright 依存（初回のみ）。Node.js 20+ 必須
	cd tests/e2e && npm install && npx playwright install chromium

test-e2e: ## Playwright E2E 実行。事前に make compose-up && make db-migrate を実行しておくこと
	cd tests/e2e && npm test

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

# ===== Terraform =====

tf-fmt: ## deploy/terraform 配下を terraform fmt -recursive
	terraform -chdir=deploy/terraform fmt -recursive -check
	@echo "[tf-fmt] ok"

tf-validate: ## dev / prod 両方を validate (バックエンドなしで init → validate)
	cd deploy/terraform/envs/dev  && terraform init -backend=false -upgrade && terraform validate
	cd deploy/terraform/envs/prod && terraform init -backend=false -upgrade && terraform validate
	@echo "[tf-validate] ok"

tf-plan-dev: ## dev 環境 plan (要 backend S3 + AWS credentials)
	cd deploy/terraform/envs/dev && terraform init && terraform plan -var-file=terraform.tfvars

tf-plan-prod: ## prod 環境 plan (要 backend S3 + AWS credentials、apply は人間承認)
	cd deploy/terraform/envs/prod && terraform init && terraform plan -var-file=terraform.tfvars

# ===== Release E2E =====

release-smoke: ## Phase 6 リリースゲート: smoke spec のみ実行
	cd tests/e2e/release && BASE_URL=$(BASE_URL) npx playwright test smoke.spec.ts

release-critical-path: ## Phase 6 リリースゲート: critical-path spec (要 E2E_TEST_EMAIL/PASSWORD)
	cd tests/e2e/release && BASE_URL=$(BASE_URL) npx playwright test critical-path.spec.ts

clean:
	rm -rf dist coverage.out coverage.html
