FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags='-s -w' -o /out/desk-tunnel .

FROM node:24-bookworm-slim
ARG CODEX_VERSION=0.170.0
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/* \
 && npm install -g --ignore-scripts --no-audit --no-fund "@openai/codex@${CODEX_VERSION}" \
 && codex --version && npm cache clean --force \
 && mkdir -p /srv/data /srv/codex /srv/cli /srv/work \
 && chown -R node:node /srv/data /srv/codex /srv/cli /srv/work
COPY --from=build /out/desk-tunnel /usr/local/bin/desk-tunnel
ENV DATA_DIR=/srv/data CODEX_HOME=/srv/codex CLI_INSTALL_DIR=/srv/cli LISTEN_ADDR=0.0.0.0:8080
WORKDIR /srv/work
USER node
EXPOSE 8080
ENTRYPOINT ["desk-tunnel"]
