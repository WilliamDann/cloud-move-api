# --- build stage ---
FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/cql-api .

# --- cql build stage ---
# reference cql 6.2 (build 9.493) from source — no linux binary is
# published. build recipe per the zip's readme: uncomment the gcc flags
# in makefile-flags, then `make cql`. verified working on trixie's gcc 14.
FROM debian:trixie-slim AS cql

ARG CQL_SOURCE_URL=https://www.gadycosteff.com/cql/cql6-source-9-493.zip
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl unzip build-essential \
    && rm -rf /var/lib/apt/lists/* \
    && curl -fsSL "$CQL_SOURCE_URL" -o /tmp/cql.zip \
    && unzip -q /tmp/cql.zip -d /tmp/cql-src \
    && cd /tmp/cql-src/cqlsourcedist \
    && sed -i \
        -e 's|^# OSFLAGS= -Wno-format-overflow|OSFLAGS= -Wno-format-overflow|' \
        -e 's|^ OSFLAGS=  -Wno-deprecated-register|# OSFLAGS=  -Wno-deprecated-register|' \
        -e 's|^#LINUXPTHREADOPTION = -pthread|LINUXPTHREADOPTION = -pthread|' \
        makefile-flags \
    && make -j"$(nproc)" cql \
    && strip cql \
    && install -D -m 0755 cql /out/cql

# --- runtime stage ---
FROM debian:trixie-slim

COPY --from=cql /out/cql /usr/local/bin/cql
COPY --from=build /out/cql-api /usr/local/bin/cql-api

# queries run with cwd = fresh temp dir (readfile/writefile land there);
# nothing else is writable to the process user
USER nobody
EXPOSE 8080

ENTRYPOINT ["cql-api"]
