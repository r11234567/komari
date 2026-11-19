FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends nginx libnginx-mod-http-lua ca-certificates && rm -rf /var/lib/apt/lists/*
COPY crowdsec.lua /usr/lib/crowdsec/lua/crowdsec.lua
CMD ["nginx", "-g", "daemon off;", "-c", "/verify/nginx.conf"]
