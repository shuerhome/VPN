# ---- mihomo：从 Go 模块源码编译（不依赖 GitHub Releases 下载）----
FROM golang:1.24-bookworm AS mihomo
ARG MIHOMO_VERSION=v1.19.31
WORKDIR /tmp
# 下载失败时错误只写在 m.json 里，打印出来才看得到原因
RUN (go mod download -json github.com/metacubex/mihomo@${MIHOMO_VERSION} > m.json || { cat m.json; exit 1; }) \
 && cp -r "$(sed -n 's/.*"Dir": "\([^"]*\)".*/\1/p' m.json)" /src \
 && chmod -R u+w /src
WORKDIR /src
RUN CGO_ENABLED=0 go build -trimpath -tags with_gvisor \
      -ldflags "-s -w -X github.com/metacubex/mihomo/constant.Version=${MIHOMO_VERSION}" \
      -o /out/mihomo .

# ---- 面板 ----
FROM golang:1.24-bookworm AS panel
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/panel ./cmd/panel

# ---- 运行 ----
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -r -u 10001 -d /data panel \
 && mkdir -p /data && chown panel /data
COPY --from=mihomo /out/mihomo /usr/local/bin/mihomo
COPY --from=panel /out/panel /usr/local/bin/panel
USER panel
ENV DATA_DIR=/data MIHOMO_BIN=/usr/local/bin/mihomo LISTEN=:8080 RELAY_LISTEN=8443 TZ=Asia/Shanghai
VOLUME /data
EXPOSE 8080 8443
CMD ["panel"]
