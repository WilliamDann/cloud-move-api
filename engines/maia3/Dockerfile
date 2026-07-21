# --- build stage ---
FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/maia3-api .

# --- runtime stage ---
FROM python:3.12-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends git \
    && rm -rf /var/lib/apt/lists/*

# cpu-only torch first so maia3 doesn't pull the cuda wheels
RUN pip install --no-cache-dir torch --index-url https://download.pytorch.org/whl/cpu \
    && pip install --no-cache-dir git+https://github.com/CSSLab/maia3.git

# bake all model weights into the image so cold starts don't hit hugging face,
# in a location the nobody user can read
ENV HF_HOME=/opt/maia3/hf \
    XDG_CACHE_HOME=/opt/maia3/cache
RUN maia3-cache --model maia3-5m \
    && maia3-cache --model maia3-23m \
    && maia3-cache --model maia3-79m \
    && chmod -R a+rX /opt/maia3

COPY --from=build /out/maia3-api /usr/local/bin/maia3-api

# the api appends --model per request; --use-uci-history conditions maia on
# the line played, not just the position
ENV ENGINE_CMD="maia3-uci --device cpu --no-use-amp --use-uci-history"

USER nobody
EXPOSE 8080

ENTRYPOINT ["maia3-api"]
