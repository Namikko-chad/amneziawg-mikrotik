#!/bin/sh
set -e

# RouterOS containers do not always expose /dev/net/tun.
if [ ! -c /dev/net/tun ]; then
    mkdir -p /dev/net
    mknod /dev/net/tun c 10 200 || echo "warning: cannot create /dev/net/tun" >&2
fi
mkdir -p /var/run/amneziawg

nginx -g 'daemon off;' &
NGINX_PID=$!

awg-manager &
MGR_PID=$!

term() {
    kill -TERM "$MGR_PID" "$NGINX_PID" 2>/dev/null || true
}
trap term TERM INT

# Exit (and let RouterOS restart us) as soon as either process dies.
while kill -0 "$MGR_PID" 2>/dev/null && kill -0 "$NGINX_PID" 2>/dev/null; do
    sleep 2 &
    wait $! || true
done
term
wait "$MGR_PID" 2>/dev/null || true
wait "$NGINX_PID" 2>/dev/null || true
