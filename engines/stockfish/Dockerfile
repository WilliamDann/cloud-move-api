# --- build stage ---
FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/sf-api .

# --- stockfish stage ---
# the debian package is compiled for baseline x86-64 (sse2 only); the official
# avx2 build roughly doubles nnue evaluation speed. avx2 is universal on cloud
# run hardware, while bmi2/avx512 variants are not safe on every machine type.
FROM debian:trixie-slim AS stockfish

ADD https://github.com/official-stockfish/Stockfish/releases/download/sf_17.1/stockfish-ubuntu-x86-64-avx2.tar /tmp/stockfish.tar
RUN tar -xf /tmp/stockfish.tar -C /tmp \
    && install -m 0755 /tmp/stockfish/stockfish-ubuntu-x86-64-avx2 /usr/local/bin/stockfish

# --- runtime stage ---
FROM debian:trixie-slim

COPY --from=stockfish /usr/local/bin/stockfish /usr/local/bin/stockfish
COPY --from=build /out/sf-api /usr/local/bin/sf-api

ENV STOCKFISH_PATH=/usr/local/bin/stockfish

USER nobody
EXPOSE 8080

ENTRYPOINT ["sf-api"]
