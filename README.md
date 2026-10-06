# amneziawg-mikrotik

[![CI](https://github.com/Namikko-chad/amneziawg-mikrotik/actions/workflows/ci.yml/badge.svg)](https://github.com/Namikko-chad/amneziawg-mikrotik/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

An [AmneziaWG](https://docs.amnezia.org/documentation/amnezia-wg/) client packaged as a RouterOS 7 container,
with a small web UI to manage it. Run it on your MikroTik router and send your whole LAN (or selected
devices or sites) through an obfuscated WireGuard tunnel. No VPN client is needed on each device.

- **Small**: about 10 MB per image, about 8 MB RAM at runtime.
- **Three ways to configure it**:
  - paste a `vpn://` key from the Amnezia app;
  - paste or upload a `.conf` file;
  - enter the SSH credentials of your Amnezia server and let the container create its own client there.
- **Router setup from the web UI**: choose whether the whole LAN, selected devices or selected sites go
  through the tunnel, with a kill switch and DNS through the tunnel. The UI applies the rules over the
  RouterOS REST API. The router login is asked for in the browser and never stored by the container.
- **Country exceptions**: pick countries (e.g. `ru`) and their subnets bypass the tunnel. The container
  downloads the lists daily and serves them to the router as a `.rsc` script.
- **Architectures**: `arm64`, `arm/v7` and `amd64` (CHR/x86).
- **Several servers**: save as many configs as you like and switch between them in one click, like the
  server list in the Amnezia app.
- **Automatic failover**: mark a second server as the backup. It stays connected in the background,
  traffic moves to it within ~15 s when the selected server stops responding, and returns once the
  selected server has been working again for a minute.
- **Survives restarts**: configs are stored on a mounted disk and the tunnel comes back up on boot.

> **Router setup guide: [docs/mikrotik-setup.md](docs/mikrotik-setup.md)**

## What's inside

| Component | Role |
|---|---|
| [`amneziawg-go`](https://github.com/amnezia-vpn/amneziawg-go) | userspace AmneziaWG implementation (no kernel module needed) |
| `awg` from [`amneziawg-tools`](https://github.com/amnezia-vpn/amneziawg-tools) | configures the interface |
| `awg-manager` ([`backend/`](backend/)) | Go backend: config parsing, `vpn://` decoding, SSH provisioning, bringing the tunnel up and down, routing and NAT |
| nginx | serves the web UI ([`web/`](web/)) on `:80` and proxies `/api/` to the backend |

### Configuration methods

1. **`vpn://` key.** Open the Amnezia app, go to *Share*, choose *AmneziaWG*, and copy the key.
   Plain base64 of a `.conf` file also works. If the key grants full access to the server (it contains
   SSH credentials), the backend provisions a new client automatically.
2. **`.conf` file.** Any AmneziaWG or WireGuard config: `Jc`, `Jmin`, `Jmax`, `S1`–`S4`, `H1`–`H4`,
   `I1`–`I5` and the other AmneziaWG 2.0 keys are passed to `awg` unchanged.
3. **SSH credentials** of a server set up by the Amnezia app (container `amnezia-awg2`, or legacy `amnezia-awg`).
   The backend does what the Amnezia app does when you add a client:
   - generates a key pair;
   - takes the next free IP in the server's subnet;
   - appends a `[Peer]` to the server config and runs `syncconf`;
   - adds the client to `clientsTable`, so it shows up in the Amnezia app under the name you choose.

   Users other than `root` get `sudo`, using the same password. Credentials are never stored.

### How traffic flows

```
LAN device ──► RouterOS ──(routing rule)──► veth 172.18.0.2 ──► container
                                                                 │ ip rule: iif <veth> → table 51820 → awg0
                                                                 │ MASQUERADE + TCP MSS clamp on awg0
                                                                 ▼
                                                               awg0 ──UDP──► AmneziaWG server
```

The container's own traffic (the tunnel's UDP packets, SSH, the web UI) goes out through the veth via the
main routing table. Only packets *forwarded to* the container by the router enter the tunnel, so there are no
routing loops and no special route to the endpoint is needed.

## Quick start

1. Download the `.tar` for your router's architecture from
   [Releases](https://github.com/Namikko-chad/amneziawg-mikrotik/releases), or [build it yourself](#building).
2. Follow the [router setup guide](docs/mikrotik-setup.md).
3. Open `http://172.18.0.2/`, paste your key, and click **Apply**.

## Building

Requirements: Docker with buildx. To cross-build ARM images on an x86 host you also need QEMU binfmt
(once per boot: `make qemu`).

```sh
make test          # go vet + backend unit tests
make build-arm64   # hAP ax², hAP ax³, RB5009, CCR2004, Chateau ax …
make build-armv7   # hAP ac², hAP ac³, RB4011, cAP ac …
make build-amd64   # CHR / x86
make all           # all of the above
```

Output: `dist/amneziawg-mikrotik-<arch>.tar` (docker-archive format, which RouterOS can import).

The AmneziaWG versions are pinned with the build args `AWG_GO_VERSION` and `AWG_TOOLS_VERSION` in the [`Dockerfile`](Dockerfile).

### Running locally with Docker

```sh
docker load -i dist/amneziawg-mikrotik-amd64.tar
docker run -d --name awg --cap-add NET_ADMIN --device /dev/net/tun \
    -p 8080:80 -v awg-conf:/etc/amnezia amneziawg-mikrotik:latest
# open http://localhost:8080/
```

## Configuration

Environment variables:

| Variable | Default | Description |
|---|---|---|
| `AWG_DATA_DIR` | `/etc/amnezia` | where `profiles.json` (all saved servers) is stored; mount persistent storage here. An `awg0.conf` left by an older version is imported automatically |
| `LAN_IFACE` | auto | interface that receives traffic from the router. Detected from the default route: on RouterOS it is the veth name, in Docker it is `eth0` |
| `LISTEN` | `127.0.0.1:8080` | backend listen address (nginx proxies to it) |
| `CHECK_HOSTS` | `1.1.1.1 8.8.8.8` | hosts pinged through each link to detect failures when a backup server is set; a link is up if any of them answers |

The tunnel comes up automatically on start with the selected server, unless it was stopped with
**Disconnect** in the UI.

## HTTP API

| Method | Path | Body |
|---|---|---|
| GET | `/api/status` | — |
| GET | `/api/config` | — (config of the selected server, keys are masked) |
| GET | `/api/logs` | — |
| POST | `/api/config/text` | `{"config": "[Interface]…", "name": "…"}` |
| POST | `/api/config/vpnkey` | `{"key": "vpn://…", "client_name": "…", "name": "…"}` |
| POST | `/api/config/ssh` | `{"host", "port", "user", "password", "private_key", "passphrase", "client_name", "name"}` |
| GET | `/api/profiles` | — (list of saved servers and the selected one) |
| GET | `/api/profiles/{id}/config` | — (keys are masked) |
| POST | `/api/profiles/{id}/activate` | `{}` |
| PATCH | `/api/profiles/{id}` | `{"name": "…"}` |
| DELETE | `/api/profiles/{id}` | — |
| POST | `/api/backup` | `{"id": "…"}` (`""` removes the backup) |
| POST | `/api/up`, `/api/down` | `{}` |
| GET | `/api/router` | — (saved routing plan, default router URL) |
| POST | `/api/router/info` | `{"auth": {"url", "user", "password", "insecure"}}` |
| POST | `/api/router/apply` | `{"auth": {…}, "plan": {"mode": "off\|all\|devices\|sites", "lan", "exclude", "exclude_dst", "bypass_geo", "devices", "sites", "kill_switch", "paused", "dns"}}` |
| POST | `/api/router/geo` | `{"auth": {…}}` (make the router reload the country list now) |
| GET | `/api/geo` | — (country list: countries, source, per-country status, script URL) |
| POST | `/api/geo` | `{"countries": ["ru", "by"], "source": "…"}` (`source` is optional, `{cc}` is the country code) |
| POST | `/api/geo/update` | `{}` (download all country lists again) |
| GET | `/lists/geo.rsc` | — (RouterOS script that fills the address list `awg-geo`; 503 while there is no list) |

The three `/api/config/*` calls save a **new** server, select it and connect. `name` is optional: by
default the description from the `vpn://` key or the server address is used. Selecting another server
reconnects the tunnel to it, unless the tunnel was stopped with `/api/down`. Selecting the backup server
swaps the two roles. Deleting the selected server makes the backup the selected one, or stops the tunnel
if there is no backup.

### Failover

With a backup server, the container runs two tunnels: `awg0` to the selected server and `awg1` to the
backup. Every 5 s it pings `CHECK_HOSTS` through each of them. After 3 failed checks in a row on the link
in use, LAN traffic is switched to the other link (if that one is answering); after 12 successful checks
in a row (~1 min) on the selected server, traffic switches back. The switch is a single route change in
the container, so nothing has to change on the router. Connections that were open at the moment of the
switch break because the public IP changes; apps reconnect on their own.

`/api/status` reports which link carries traffic in `using` (`primary` or `backup`) and the state of
each link in `links`.

## Security notes

- **The web UI has no authentication.** Restrict access to `172.18.0.2:80` with the RouterOS firewall
  (see the [setup guide](docs/mikrotik-setup.md#9-protect-the-web-ui)).
- **The router login is not stored.** The Router card keeps it in the browser tab's `sessionStorage` and
  sends it with each request. Without it the container cannot change the router. Give that RouterOS
  user only the `read,write,api,rest-api` policies and `address=172.18.0.2/32`.
- **Inbound connections from the tunnel are blocked.** Amnezia servers forward traffic between their
  clients by default. The container accepts only replies to connections opened from your LAN, so other
  clients of the same server cannot reach the web UI or your LAN. See
  [verifying isolation](docs/mikrotik-setup.md#verify-that-the-container-is-reachable-only-from-your-lan).
- SSH host keys are not verified: only enter servers you trust, on a network you trust.
- Private keys are stored unencrypted in `profiles.json` on the mounted disk, as with any WireGuard setup.

## Project layout

```
backend/                 Go backend (awg-manager)
  internal/conf/         AmneziaWG/WireGuard config parser
  internal/profiles/     saved servers and the selected one
  internal/vpnkey/       vpn:// key decoder
  internal/sshprov/      client provisioning over SSH
  internal/tunnel/       interfaces, routing, NAT and failover between the two links
  internal/routeros/     RouterOS REST API client
  internal/routing/      routing plan -> router rules (routing rules, mangle, address lists, DNS)
  internal/geoip/        per-country subnet lists, served to the router as a .rsc script
web/                     static web UI (plain HTML/JS/CSS)
docker/                  nginx.conf, entrypoint.sh
docs/                    router setup guide
```

## License

[MIT](LICENSE) for the code in this repository.

The container image also includes third-party software under its own licenses:
[amneziawg-go](https://github.com/amnezia-vpn/amneziawg-go) (MIT),
[amneziawg-tools](https://github.com/amnezia-vpn/amneziawg-tools) (GPL-2.0, the `awg` binary, built unmodified from upstream source),
nginx (BSD-2-Clause) and Alpine Linux packages.

This project is not affiliated with Amnezia VPN or MikroTik.
