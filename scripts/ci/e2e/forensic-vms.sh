#!/usr/bin/env bash
set -euo pipefail
umask 077
panel_source=$(cd "$(dirname "$0")/../../.." && pwd)
agent_source=$(realpath "${1:?Agent checkout required}")
image=$(realpath "${2:?verified Ubuntu cloud image required}")
(cd "$(dirname "$image")" && sha256sum --check image.sha256)
work=$(mktemp -d /tmp/komari-forensic-vms.XXXXXX)
panel_vm= agent_vm= proxy=
cleanup() {
    status=$?
    if (( status != 0 )) && [[ -s "$work/nginx-access.log" ]]; then
        grep -F 'SealedRecordService' "$work/nginx-access.log" | tail -n 12 || true
    fi
    for pid in "$panel_vm" "$agent_vm" "$proxy"; do
        if [[ -n "$pid" ]]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
    done
    if (( status != 0 )); then
        for log in "$work/panel-console.log" "$work/agent-console.log" "$work/nginx-error.log"; do
            if [[ -f "$log" ]]; then tail -n 18 "$log"; fi
        done
    fi
    case "$work" in /tmp/komari-forensic-vms.*) rm -rf -- "$work" ;; esac
}
trap cleanup EXIT
ssh-keygen -q -t ed25519 -N '' -f "$work/client"
openssl rand -hex 24 > "$work/root-password"
read -r password < "$work/root-password"
authorized_key=$(cat "$work/client.pub")
openssl req -x509 -newkey rsa:2048 -noenc -days 1 -subj '/CN=Komari forensic CI' \
    -addext 'subjectAltName=IP:127.0.0.1,IP:10.0.2.100' \
    -keyout "$work/tls.key" -out "$work/tls.crt" 2>/dev/null
for role in panel agent; do
    {
        printf '#cloud-config\ndisable_root: false\nssh_pwauth: false\n'
        printf 'users:\n  - default\n  - name: root\n    ssh_authorized_keys:\n      - %s\n' "$authorized_key"
        printf 'chpasswd:\n  expire: false\n  users:\n    - {name: root, password: "%s", type: text}\n' "$password"
        printf "write_files:\n  - path: /etc/ssh/sshd_config.d/01-komari-fixture.conf\n    content: |\n      PermitRootLogin prohibit-password\n      UsePAM yes\n"
    } > "$work/$role-user-data"
    printf 'instance-id: komari-forensic-%s\nlocal-hostname: komari-forensic-%s\n' "$role" "$role" > "$work/$role-meta-data"
    cloud-localds "$work/$role-seed.iso" "$work/$role-user-data" "$work/$role-meta-data"
    qemu-img create -q -f qcow2 -F qcow2 -b "$image" "$work/$role.qcow2"
done
unset password

mkdir "$work/bin" "$work/packages"
(cd "$panel_source" && CGO_ENABLED=1 go build -buildvcs=false -trimpath -o "$work/bin/komari" .)
(
    cd "$agent_source"
    CGO_ENABLED=0 go build -buildvcs=false -trimpath -o "$work/bin/komari-agent" .
    CGO_ENABLED=0 go build -buildvcs=false -trimpath -o "$work/bin/komari-host" ./cmd/komari-host
)
(cd "$work/packages" && apt-get download qemu-guest-agent liburing2 pamtester)

start_vm() {
    local role=$1 networking=$2
    shift 2
    qemu-system-x86_64 -machine q35 -accel kvm -accel tcg,thread=multi -m 1152 -smp 2 \
        -nodefaults -display none -serial "file:$work/$role-console.log" -no-reboot \
        -drive "file=$work/$role.qcow2,if=virtio,format=qcow2" \
        -drive "file=$work/$role-seed.iso,if=virtio,format=raw,readonly=on" \
        -netdev "$networking" -device virtio-net-pci,netdev=nic "$@" &
}
start_vm panel 'user,id=nic,restrict=on,hostfwd=tcp:127.0.0.1:29480-:22,hostfwd=tcp:127.0.0.1:29481-:25774'
panel_vm=$!
# Start a separate loopback proxy per guest TCP connection. A persistent
# character backend would mix enrollment, daemon, and forensic TLS sessions.
start_vm agent 'user,id=nic,restrict=on,hostfwd=tcp:127.0.0.1:29482-:22,guestfwd=tcp:10.0.2.100:8443-cmd:nc 127.0.0.1 29483' \
    -device virtio-serial-pci -chardev "socket,id=qga,path=$work/qga.sock,server=on,wait=off" \
    -device virtserialport,chardev=qga,name=org.qemu.guest_agent.0
agent_vm=$!

cat > "$work/nginx.conf" <<NGINX
pid $work/nginx.pid;
error_log $work/nginx-error.log;
events { worker_connections 64; }
http {
  log_format forensic '\$request_method \$uri \$status upstream=\$upstream_status';
  access_log $work/nginx-access.log forensic;
  client_body_temp_path $work/nginx-client;
  proxy_temp_path $work/proxy;
  server {
    listen 127.0.0.1:29483 ssl;
    ssl_certificate $work/tls.crt;
    ssl_certificate_key $work/tls.key;
    location / {
      proxy_pass http://127.0.0.1:29481;
      proxy_http_version 1.1;
      proxy_set_header Host \$http_host;
      proxy_set_header X-Forwarded-Proto https;
      proxy_set_header X-Forwarded-Host \$http_host;
      proxy_set_header Connection '';
      proxy_buffering off;
      proxy_request_buffering off;
    }
  }
}
NGINX
nginx -p "$work/" -c "$work/nginx.conf" -g 'daemon off;' &
proxy=$!

ssh_flags=(-i "$work/client" -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new
           -o "UserKnownHostsFile=$work/known_hosts" -o ConnectTimeout=3 -o LogLevel=ERROR)
for port in 29480 29482; do
    for attempt in {1..70}; do
        if ssh "${ssh_flags[@]}" -p "$port" root@127.0.0.1 'true' 2>/dev/null; then break; fi
        if (( attempt == 70 )); then exit 1; fi
        sleep 1
    done
done

scp "${ssh_flags[@]}" -P 29480 "$work/bin/komari" root@127.0.0.1:/root/
ssh "${ssh_flags[@]}" -p 29480 root@127.0.0.1 'bash -se' > "$work/panel-public-keys" <<'PANEL'
install -d -m 0700 /root/fixture /root/fixture/archives /root/fixture/keys
/root/komari init-forensic-keys /root/fixture/keys
cat > /etc/systemd/system/komari-forensic-panel.service <<'UNIT'
[Unit]
Description=Komari synthetic forensic panel
[Service]
WorkingDirectory=/root/fixture
Environment=KOMARI_TRUSTED_PROXIES=10.0.2.2
Environment=KOMARI_PANEL_DIAGNOSTIC_ARCHIVE=/root/fixture/archives
Environment=KOMARI_PANEL_DIAGNOSTIC_SIGNING_KEY=/root/fixture/keys/forensic-receipt.key
EnvironmentFile=-/root/fixture/node.env
ExecStart=/root/komari server --listen 0.0.0.0:25774
UNIT
systemctl daemon-reload
systemctl start komari-forensic-panel.service
PANEL
scp "${ssh_flags[@]}" -P 29482 "$work/bin/komari-agent" "$work/bin/komari-host" "$work/tls.crt" "$work/packages/"*.deb root@127.0.0.1:/root/
ssh "${ssh_flags[@]}" -p 29482 root@127.0.0.1 'bash -se' <<'AGENT'
dpkg -i /root/*.deb
install -m 0644 /root/tls.crt /usr/local/share/ca-certificates/komari-forensic-fixture.crt
update-ca-certificates
install -d -m 0755 /usr/lib/komari/komari-agent
install -m 0755 /root/komari-{agent,host} /usr/lib/komari/komari-agent/
/usr/lib/komari/komari-agent/komari-host install --user root -- --disable-auto-update --disable-remote-control
useradd --system --shell /usr/sbin/nologin forensic-fixture
systemctl start qemu-guest-agent.service
AGENT
openssl rand -hex 32 > "$work/requested"
openssl rand -hex 32 > "$work/unrequested"
scp "${ssh_flags[@]}" -P 29482 "$work/requested" "$work/unrequested" root@127.0.0.1:/root/
export NODE_EXTRA_CA_CERTS="$work/tls.crt"
node "$panel_source/scripts/ci/e2e/forensic-vms.cjs" "$work"
