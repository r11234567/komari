# Include inside the HTTPS Komari server, or its trusted local Xray fallback.
# Remove the former per-IP metric and generic request/connection limit includes.
access_log /var/log/nginx/komari-access.log komari_security;
client_max_body_size 100m;
client_body_timeout 35m;
send_timeout 35m;

location ~ ^/komari\.(webssh\.v1\.WebSSHService/(AttachSession|OpenSession)|file\.v1\.FileService/AttachFileOperations|metrics\.v1\.MetricsService/(UploadMetrics|StreamMetrics))$ {
    grpc_pass grpc://komari_backend;
    grpc_set_header Host $host;
    grpc_set_header X-Real-IP $remote_addr;
    grpc_set_header X-Forwarded-For $remote_addr;
    grpc_set_header X-Forwarded-Proto https;
    grpc_connect_timeout 5s;
    grpc_read_timeout 35m;
    grpc_send_timeout 35m;
}
location / {
    proxy_pass http://komari_backend;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $remote_addr;
    proxy_set_header X-Forwarded-Proto https;
    proxy_set_header Connection "";
    proxy_buffering off;
    proxy_request_buffering off;
    proxy_connect_timeout 5s;
    proxy_read_timeout 35m;
    proxy_send_timeout 35m;
}
