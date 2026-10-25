#!/usr/bin/env bash
source "$(dirname "$0")/guard.sh"
umask 077
mkdir -p "$E2E_ROOT/tls" "$E2E_ROOT/panel" "$E2E_ROOT/files" "$E2E_ARTIFACTS"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=Komari-CI-only-CA \
  -keyout "$E2E_ROOT/tls/ca.key" -out "$E2E_ROOT/tls/ca.crt"
openssl req -newkey rsa:2048 -nodes -subj /CN=127.0.0.1 \
  -keyout "$E2E_ROOT/tls/server.key" -out "$E2E_ROOT/tls/server.csr"
printf 'subjectAltName=IP:127.0.0.1,DNS:localhost\nextendedKeyUsage=serverAuth\n' > "$E2E_ROOT/tls/extensions"
openssl x509 -req -in "$E2E_ROOT/tls/server.csr" -CA "$E2E_ROOT/tls/ca.crt" \
  -CAkey "$E2E_ROOT/tls/ca.key" -CAcreateserial -days 1 \
  -extfile "$E2E_ROOT/tls/extensions" -out "$E2E_ROOT/tls/server.crt"
sudo install -m 0644 "$E2E_ROOT/tls/ca.crt" /usr/local/share/ca-certificates/komari-e2e.crt
sudo update-ca-certificates
mkdir -p "$HOME/.pki/nssdb"
if [[ ! -f "$HOME/.pki/nssdb/cert9.db" ]]; then
  certutil -N --empty-password -d "sql:$HOME/.pki/nssdb"
fi
certutil -A -n komari-e2e -t 'C,,' -d "sql:$HOME/.pki/nssdb" -i "$E2E_ROOT/tls/ca.crt"

# H2 is required for Agent bidirectional Connect streams. The grpc module
# proxies those paths to the server's h2c listener; ordinary browser requests
# and the raw file data plane use HTTP/1.1 without request/response buffering.
runner_user=$(id -un)
cat > "$E2E_ROOT/nginx.conf" <<NGINX
user $runner_user;
pid $E2E_ROOT/nginx.pid;
error_log $E2E_ARTIFACTS/nginx-error.log info;
events { worker_connections 256; }
http {
  log_format e2e '\$time_iso8601 \$request_method \$uri \$status upstream=\$upstream_status rt=\$request_time';
  access_log $E2E_ARTIFACTS/nginx-access.log e2e;
  client_body_temp_path $E2E_ROOT/nginx-body;
  server {
    listen 127.0.0.1:29443 ssl http2;
    server_name localhost;
    ssl_certificate $E2E_ROOT/tls/server.crt;
    ssl_certificate_key $E2E_ROOT/tls/server.key;
    client_max_body_size 8m;
    location ~ ^/komari\\..*Service/(AttachSession|AttachFileOperations)\$ {
      grpc_pass grpc://127.0.0.1:29444;
      grpc_set_header Host \$http_host;
      grpc_set_header X-Forwarded-Proto https;
      grpc_set_header X-Forwarded-Host \$http_host;
      grpc_read_timeout 180s;
    }
    location / {
      proxy_pass http://127.0.0.1:29444;
      proxy_http_version 1.1;
      proxy_set_header Host \$http_host;
      proxy_set_header X-Forwarded-Proto https;
      proxy_set_header X-Forwarded-Host \$http_host;
      proxy_set_header Connection '';
      proxy_buffering off;
      proxy_request_buffering off;
      proxy_read_timeout 180s;
    }
  }
}
NGINX
cp "$E2E_ROOT/nginx.conf" "$E2E_ARTIFACTS/nginx.conf"
sudo nginx -t -c "$E2E_ROOT/nginx.conf" -p "$E2E_ROOT/"
sudo nginx -c "$E2E_ROOT/nginx.conf" -p "$E2E_ROOT/"
(
  cd "$E2E_ROOT/panel"
  nohup "$E2E_ROOT/bin/komari" server --listen 127.0.0.1:29444 \
    > "$E2E_ARTIFACTS/server.log" 2>&1 &
  echo "$!" > "$E2E_ROOT/server.pid"
)
for attempt in $(seq 1 60); do
  if curl --silent --fail "$E2E_URL/api/install/status" > "$E2E_ARTIFACTS/initial-install-status.json"; then break; fi
  if [[ "$attempt" = 60 ]]; then exit 1; fi
  sleep 1
done
sudo install -d /usr/lib/komari/komari-e2e
sudo install -m0755 "$E2E_ROOT/bin/komari-agent" /usr/lib/komari/komari-e2e/komari-agent
sudo install -m0755 "$E2E_ROOT/bin/komari-host" /usr/lib/komari/komari-e2e/komari-host
sudo /usr/lib/komari/komari-e2e/komari-host install \
  --service komari-e2e --user "$runner_user" --binary-dir /usr/lib/komari/komari-e2e \
  -- --endpoint "$E2E_URL" --disable-auto-update
printf 'editor fixture\n' > "$E2E_ROOT/files/editor.txt"
printf 'destination must survive\n' > "$E2E_ROOT/files/existing.txt"
mkdir "$E2E_ROOT/files/nonempty"
printf 'keep\n' > "$E2E_ROOT/files/nonempty/keep.txt"
