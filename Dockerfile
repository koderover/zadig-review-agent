ARG GO_VERSION=1.25.12
ARG ALPINE_VERSION=3.23

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build \
      -trimpath \
      -ldflags="-s -w \
        -X github.com/koderover/zadig-review-agent/internal/version.Version=${VERSION} \
        -X github.com/koderover/zadig-review-agent/internal/version.Commit=${COMMIT} \
        -X github.com/koderover/zadig-review-agent/internal/version.Date=${BUILD_DATE}" \
      -o /out/zadig-review-agent .

FROM alpine:${ALPINE_VERSION}

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="zadig-review-agent" \
      org.opencontainers.image.description="LLM-powered code review agent for local development and CI" \
      org.opencontainers.image.source="https://github.com/koderover/zadig-review-agent" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"

RUN apk add --no-cache bash ca-certificates git \
    && mkdir -p /root/.zadig-review-agent /root/.ssh /workspace /tmp \
    && chmod 0700 /root/.ssh \
    && chmod 1777 /tmp \
    && git config --system --add safe.directory '/workspace/*'

COPY --from=build /out/zadig-review-agent /usr/local/bin/zadig-review-agent

ENV HOME=/root

USER root
WORKDIR /workspace

ENTRYPOINT ["zadig-review-agent"]
