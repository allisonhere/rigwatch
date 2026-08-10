FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY .git ./.git

ARG GIT_TAG=
ARG GIT_COMMIT=
ARG BUILD_DATE=

# Auto-derive version from .git when ARGs are empty (default compose build path);
# override manually with: docker compose build --build-arg GIT_TAG=v1.2.3
RUN GIT_TAG="${GIT_TAG:-$(git describe --tags --abbrev=0 2>/dev/null || echo v0.0.0)}" \
 && GIT_COMMIT="${GIT_COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}" \
 && BUILD_DATE="${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}" \
 && CGO_ENABLED=0 go build \
  -ldflags "-s -w -X 'github.com/allisonhere/rigwatch/internal.Version=${GIT_TAG#v}' -X 'github.com/allisonhere/rigwatch/internal.GitCommit=${GIT_COMMIT}' -X 'github.com/allisonhere/rigwatch/internal.BuildDate=${BUILD_DATE}' -X 'github.com/allisonhere/rigwatch/internal.GitTag=${GIT_TAG}'" \
  -o /rigwatch ./cmd/rigwatch

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata util-linux procps-ng wireguard-tools pciutils

COPY --from=builder /rigwatch /usr/local/bin/rigwatch

VOLUME ["/root/.ssh"]

EXPOSE 8081

ENTRYPOINT ["rigwatch"]
CMD ["--web", "--port", "8081"]
