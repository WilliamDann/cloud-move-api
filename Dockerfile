# --- build stage ---
FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/cloud-move .

# --- runtime stage ---
FROM debian:trixie-slim

# ca certificates so the gateway can reach google's token endpoint and
# the https engine urls; slim images don't ship them
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/cloud-move /usr/local/bin/cloud-move

USER nobody
EXPOSE 8080

ENTRYPOINT ["cloud-move"]
