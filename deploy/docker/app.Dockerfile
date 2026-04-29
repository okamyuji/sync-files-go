# syntax=docker/dockerfile:1.7
#
# sync-files-go アプリケーションコンテナ
#
# - ビルド: golang:1.25-bookworm
# - 実行: gcr.io/distroless/static-debian12:nonroot
# - arm64 / static binary / read-only root filesystem 想定
#
# /var/data は ECS タスクの volume mount で attach される (S3 Files NFS)。

FROM golang:1.25-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0 GOOS=linux GOARCH=arm64

RUN go build -tags=netgo,osusergo \
    -ldflags="-s -w -buildid=" \
    -trimpath \
    -o /out/sync-files-go \
    ./cmd/server \
 && go build -tags=netgo,osusergo \
    -ldflags="-s -w -buildid=" \
    -trimpath \
    -o /out/sync-files-batch \
    ./cmd/batch \
 && go build -tags=netgo,osusergo \
    -ldflags="-s -w -buildid=" \
    -trimpath \
    -o /out/sync-files-admin \
    ./cmd/sync-files-admin

# templates / static (Phase 4 以降で実体が増えていく)
RUN mkdir -p /out/templates /out/static \
    && cp -r internal/ui/templates /out/templates 2>/dev/null || true \
    && cp -r internal/ui/static    /out/static    2>/dev/null || true

# /var/data の雛形（distroless にはシェルが無いので build stage で nonroot UID/GID(65532) 所有のディレクトリを作成）。
# 名前付き volume が初回マウント時にこの所有権をコピーする。
RUN mkdir -p /out/data && chown -R 65532:65532 /out/data

# ----- Runtime -----
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/sync-files-go    /sync-files-go
COPY --from=build /out/sync-files-batch /sync-files-batch
COPY --from=build /out/sync-files-admin /sync-files-admin
COPY --from=build /out/templates /templates
COPY --from=build /out/static    /static
COPY --from=build --chown=nonroot:nonroot /out/data /var/data

ENV PORT=8080 \
    DATA_DIR=/var/data \
    APP_ENV=prod

EXPOSE 8080

USER nonroot:nonroot

ENTRYPOINT ["/sync-files-go"]
