# A2DP relay bridge. Build:   docker build -t a2dp-relay-bridge .
# Run with host networking (the nodes and Home Assistant connect directly):
#   docker run -d --network host -v ./data:/data a2dp-relay-bridge

FROM golang:1.27-trixie AS build
# fdk-aac lives in non-free.
RUN sed -i 's/^Components: main$/Components: main non-free/' /etc/apt/sources.list.d/debian.sources \
 && apt-get update \
 && apt-get install -y --no-install-recommends libfdk-aac-dev libldacbt-enc-dev libfreeaptx-dev \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=1 go build -trimpath \
    -ldflags="-s -w -X github.com/sl1288/a2dp-relay-bridge/internal/bridge.Version=${VERSION}" \
    -o /out/a2dp-bridge ./cmd/a2dp-bridge

FROM debian:trixie-slim
RUN sed -i 's/^Components: main$/Components: main non-free/' /etc/apt/sources.list.d/debian.sources \
 && apt-get update \
 && apt-get install -y --no-install-recommends libfdk-aac2 libldacbt-enc2 libfreeaptx0 ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/a2dp-bridge /usr/local/bin/a2dp-bridge
VOLUME /data
WORKDIR /data
# Configuration and state live in /data (bridge.yaml, state.json).
ENTRYPOINT ["/usr/local/bin/a2dp-bridge", "-config", "/data/bridge.yaml"]
