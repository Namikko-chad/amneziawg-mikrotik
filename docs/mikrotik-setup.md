# MikroTik setup guide

This guide takes you from a stock RouterOS 7 router to a LAN whose traffic goes through AmneziaWG.
It covers sending all traffic, selected devices or selected sites through the tunnel, plus DNS, IPv6,
updates and troubleshooting.

Run the commands in the RouterOS terminal: *Winbox → New Terminal*, or `ssh admin@192.168.88.1`.

## Contents

1. [Before you start](#1-before-you-start)
2. [Install the container package](#2-install-the-container-package)
3. [Prepare storage](#3-prepare-storage)
4. [Create the container network](#4-create-the-container-network)
5. [Upload the image](#5-upload-the-image)
6. [Create and start the container](#6-create-and-start-the-container)
7. [Configure the tunnel in the web UI](#7-configure-the-tunnel-in-the-web-ui)
8. [Route traffic through the tunnel](#8-route-traffic-through-the-tunnel)
9. [Protect the web UI](#9-protect-the-web-ui)
10. [Send DNS through the tunnel](#10-send-dns-through-the-tunnel)
11. [IPv6](#11-ipv6)
12. [Verify](#12-verify)
13. [Update the image](#13-update-the-image)
14. [Uninstall](#14-uninstall)
15. [Troubleshooting](#15-troubleshooting)

---

## 1. Before you start

### Assumptions

The commands below assume the RouterOS default configuration (*defconf*). If your setup differs,
substitute your own values everywhere they appear:

| Item | Value used in this guide |
|---|---|
| LAN bridge | `bridge` |
| Interface lists | `LAN`, `WAN` |
| LAN subnet | `192.168.88.0/24` |
| Router LAN address | `192.168.88.1` |
| Container network | `172.18.0.0/30`: router `172.18.0.1`, container `172.18.0.2` |
| USB disk | `usb1` |

Choose a container subnet that is not used anywhere else in your network or on your VPN server.

### Check RouterOS version and architecture

```routeros
/system/resource/print
```

- `version` must be **7.x**. 7.13 or newer is recommended. To update, run
  `/system/package/update/check-for-updates`, then `/system/package/update/install`.
- `architecture-name` decides which image you need:

| `architecture-name` | Image | Example models |
|---|---|---|
| `arm64` | `amneziawg-mikrotik-arm64.tar` | hAP ax², hAP ax³, RB5009, Chateau ax, L009, CCR2004, CCR2116 |
| `arm` | `amneziawg-mikrotik-armv7.tar` | hAP ac², hAP ac³, RB4011, cAP ac, RB3011 |
| `x86_64` | `amneziawg-mikrotik-amd64.tar` | CHR, x86 PCs |
| `mipsbe`, `mmips`, `smips`, `tile`, `ppc` | **not supported** | hEX (RB750Gr3), hAP lite, hAP ac lite, … |

### Check free space

```routeros
/system/resource/print
```

Look at `free-hdd-space`. The unpacked container takes about 30 MB, and during import the `.tar`
(about 10 MB) is stored as well. If you have less than about 60 MB free, use a USB disk (step 3).
This is recommended in any case, because it spares the internal flash.

## 2. Install the container package

### 2.1 Install the package

```routeros
/system/package/print
```

If `container` is not listed:

1. Go to [mikrotik.com/download](https://mikrotik.com/download) and download **Extra packages** for
   **exactly** your RouterOS version and architecture.
2. Extract `container-7.xx-<arch>.npk` from the archive.
3. Upload it to the router: drag it into *Winbox → Files*, or use
   `scp container-7.xx-arm64.npk admin@192.168.88.1:/`.
4. Reboot: `/system/reboot`.

### 2.2 Enable container mode

For security reasons, container support must be confirmed physically:

```routeros
/system/device-mode/update container=yes
```

Within **5 minutes** after running this, either:

- briefly press the **Reset** (or **Mode**) button on the router, or
- on devices without a button, unplug the power and plug it back in.

On CHR/x86 a normal reboot is enough. Then check:

```routeros
/system/device-mode/print
```

You should see `container: yes`.

## 3. Prepare storage

### Option A: USB disk (recommended)

> ⚠️ Formatting erases everything on the disk.

```routeros
/disk/print
/disk/format usb1 file-system=ext4
```

Some 7.x versions call this command `/disk/format-drive usb1 file-system=ext4`. Use the disk name
shown by `/disk/print` (`usb1`, `disk1`, `usb1-part1`, …).

### Option B: internal flash

Only if you have enough free space (see step 1). In all commands below, drop the `usb1/` prefix:
for example, `root-dir=awg` instead of `root-dir=usb1/awg`.

### Temporary directory for image import

```routeros
/container/config/set tmpdir=usb1/tmp
```

## 4. Create the container network

The container connects to the router through a virtual Ethernet pair (`veth`).

```routeros
/interface/veth/add name=veth-awg address=172.18.0.2/30 gateway=172.18.0.1
/ip/address/add address=172.18.0.1/30 interface=veth-awg
/interface/list/member/add list=LAN interface=veth-awg
```

Adding the veth to the `LAN` list lets the default firewall pass the container's traffic to the
internet and to the router's DNS.

**Do not** add `veth-awg` to the bridge. It is a separate routed interface.

Check that both ends match. Mismatched addresses are the most common cause of an unreachable container:

```routeros
/interface/veth/print detail
/ip/address/print where interface=veth-awg
```

You should see `address=172.18.0.2/30 gateway=172.18.0.1` on the veth, and `172.18.0.1/30` on
`veth-awg`.

The container needs NAT to the internet. Defconf already has it (`defconf: masquerade`):

```routeros
/ip/firewall/nat/print
```

If there is no masquerade rule for `out-interface-list=WAN`, add one:

```routeros
/ip/firewall/nat/add chain=srcnat out-interface-list=WAN action=masquerade
```

## 5. Upload the image

Download the image for your architecture from
[Releases](https://github.com/Namikko-chad/amneziawg-mikrotik/releases), or build it with `make build-<arch>`.

Upload it from your computer:

```sh
scp amneziawg-mikrotik-arm64.tar admin@192.168.88.1:/
```

You can also drag it into *Winbox → Files*. Then check where it landed:

```routeros
/file/print where name~"amnezia"
```

Use the path **exactly as shown** in the `file=` parameter in the next step. A file in the root is
just `amneziawg-mikrotik-arm64.tar`, while a file on the USB disk is `usb1/amneziawg-mikrotik-arm64.tar`.

## 6. Create and start the container

### 6.1 Persistent storage for the config

The VPN config lives in `/etc/amnezia` inside the container. Mount a directory from the disk there so
the config survives container re-creation and image updates.

The syntax depends on the RouterOS version.

**RouterOS 7.20 and newer:**

```routeros
/container/mounts/add list=awg_conf src=usb1/awg-conf dst=/etc/amnezia
```

**Older 7.x:**

```routeros
/container/mounts/add name=awg_conf src=usb1/awg-conf dst=/etc/amnezia
```

If you get `bad parameter name`, you are using the syntax for the other version. If unsure, type
`/container/mounts/add ` and press **Tab** twice to see the parameters your version accepts.

### 6.2 Create the container

**RouterOS 7.20 and newer** (`mountlists=`):

```routeros
/container/add file=amneziawg-mikrotik-arm64.tar interface=veth-awg \
    root-dir=usb1/awg mountlists=awg_conf hostname=awg \
    logging=yes start-on-boot=yes
```

**Older 7.x** (`mounts=`):

```routeros
/container/add file=amneziawg-mikrotik-arm64.tar interface=veth-awg \
    root-dir=usb1/awg mounts=awg_conf hostname=awg \
    logging=yes start-on-boot=yes
```

`input does not match any value of file` means the `file=` path is wrong. See step 5.

### 6.3 Wait for extraction and start

```routeros
/container/print
```

The status goes from `extracting` to `stopped`, which takes 20–60 s on a USB disk. Then start it:

```routeros
/container/start [find interface=veth-awg]
```

After a few seconds `/container/print` shows the `R` (running) flag. Check from the router:

```routeros
/ping 172.18.0.2 count=3
/tool/fetch url="http://172.18.0.2/api/status" output=user
```

The fetch should return JSON like `{"configured":false,...}`. Container logs:

```routeros
/log/print where topics~"container"
```

You can now delete the uploaded `.tar`:

```routeros
/file/remove amneziawg-mikrotik-arm64.tar
```

## 7. Configure the tunnel in the web UI

Open **http://172.18.0.2/** from a computer in your LAN.

> **If the page keeps loading forever** while `/tool/fetch` from the router works, a VPN client on
> *your computer* (for example the Amnezia app) is probably capturing `172.18.0.0/30`. Turn it off, or
> add `172.18.0.0/30` to its split-tunnelling exclusions, or use the port forward from [step 9](#9-protect-the-web-ui).

Choose one method:

| Tab | What to paste |
|---|---|
| **vpn:// key** | In the Amnezia app, go to *Server → Share → AmneziaWG* and copy the `vpn://…` string. If the key contains full server access, enter a client name too. |
| **Config** | An exported AmneziaWG `.conf` file. Paste it, or use *Load file*. |
| **SSH server** | The IP, user and password (or private key) of the VPS where the Amnezia app installed AmneziaWG. A new client is created on the server and shows up in the Amnezia app under the given name. |

After **Apply** the badge turns **connected**, and the handshake time and traffic counters update.
If it shows **no handshake**, open *Logs* at the bottom of the page.

At this point the tunnel is up, but nothing is routed into it yet.

## 8. Route traffic through the tunnel

### 8.1 Routing table (required for every option below)

```routeros
/routing/table/add name=to-awg fib
/ip/route/add dst-address=0.0.0.0/0 gateway=172.18.0.2 routing-table=to-awg check-gateway=ping comment="awg"
```

`check-gateway=ping` deactivates the route when the container stops responding.

Then choose **one** of the options below.

### 8.2 Option A: all LAN traffic (full tunnel)

Order matters: exceptions first, then the catch-all rule.

```routeros
/routing/rule/add dst-address=192.168.88.0/24 action=lookup-only-in-table table=main comment="awg: LAN direct"
/routing/rule/add dst-address=172.18.0.0/30 action=lookup-only-in-table table=main comment="awg: container direct"
/routing/rule/add src-address=192.168.88.0/24 action=lookup table=to-awg comment="awg: LAN via tunnel"
```

- The first two rules keep local traffic local: the router itself, devices talking to each other,
  and the web UI.
- The third rule sends everything else from the LAN to the container.
- The container's own traffic comes from `172.18.0.2`, so it does not match the third rule and goes
  straight to the ISP. There are no routing loops.

These rules work with FastTrack, so you don't need to change the firewall.

#### Kill switch

With `action=lookup` (as above), LAN traffic **falls back to the direct ISP route** if the container
goes down. To block internet access instead while the tunnel is down:

```routeros
# enable kill switch
/routing/rule/set [find comment="awg: LAN via tunnel"] action=lookup-only-in-table

# disable kill switch (fall back to the ISP when the tunnel is down)
/routing/rule/set [find comment="awg: LAN via tunnel"] action=lookup
```

#### Temporarily bypass the tunnel for the whole LAN

```routeros
/routing/rule/disable [find comment="awg: LAN via tunnel"]
/routing/rule/enable  [find comment="awg: LAN via tunnel"]
```

#### Exclude a device or a destination

Place exceptions **before** the catch-all rule:

```routeros
# a device that should never use the tunnel (e.g. a TV)
/routing/rule/add src-address=192.168.88.50/32 action=lookup-only-in-table table=main \
    comment="awg: direct device" place-before=[find comment="awg: LAN via tunnel"]

# a destination subnet that should go direct (e.g. your bank)
/routing/rule/add dst-address=203.0.113.0/24 action=lookup-only-in-table table=main \
    comment="awg: direct destination" place-before=[find comment="awg: LAN via tunnel"]
```

Check the result:

```routeros
/routing/rule/print
```

### 8.3 Option B: only selected devices

Instead of the catch-all rule from option A, add one rule per device:

```routeros
/routing/rule/add dst-address=192.168.88.0/24 action=lookup-only-in-table table=main comment="awg: LAN direct"
/routing/rule/add src-address=192.168.88.50/32 action=lookup table=to-awg comment="awg: device via tunnel"
/routing/rule/add src-address=192.168.88.51/32 action=lookup table=to-awg comment="awg: device via tunnel"
```

Give these devices static DHCP leases so their addresses don't change: *IP → DHCP Server → Leases →
Make Static*.

### 8.4 Option C: only selected sites

Routing rules cannot match domain names, so this option uses an address list with mangle. RouterOS
resolves domain names in address lists to IPs automatically.

```routeros
/ip/firewall/address-list/add list=vpn address=youtube.com
/ip/firewall/address-list/add list=vpn address=googlevideo.com
/ip/firewall/address-list/add list=vpn address=203.0.113.0/24

/ip/firewall/mangle/add chain=prerouting in-interface=bridge dst-address-list=vpn \
    connection-mark=no-mark action=mark-connection new-connection-mark=awg-conn passthrough=yes comment="awg"
/ip/firewall/mangle/add chain=prerouting in-interface=bridge connection-mark=awg-conn \
    action=mark-routing new-routing-mark=to-awg passthrough=no comment="awg"
```

FastTrack bypasses mangle, so exclude marked connections from it:

```routeros
/ip/firewall/filter/set [find action=fasttrack-connection] connection-mark=no-mark
```

`in-interface=bridge` keeps the container's own traffic out of the tunnel.

> Large services (YouTube, Instagram, …) use many domains and CDN addresses. For reliable results,
> prefer option A with exceptions, or maintain the address list from a published IP list.

## 9. Protect the web UI

**The web UI has no authentication.** Anyone who can reach `172.18.0.2:80` can read the masked
config, replace it, or stop the tunnel. Allow only your admin devices:

```routeros
/ip/firewall/address-list/add list=awg-admins address=192.168.88.10
/ip/firewall/filter/add chain=forward dst-address=172.18.0.2 protocol=tcp dst-port=80 \
    src-address-list=!awg-admins action=drop comment="awg: protect web UI"
```

Use your computer's real IP address (`ipconfig` on Windows, `ip addr` on Linux). Ideally give it a
static DHCP lease.

### Optional: reach the web UI at the router's address

This helps if a VPN client on your computer captures `172.18.0.0/30`, because most clients keep the
local LAN (`192.168.88.0/24`) outside the tunnel:

```routeros
/ip/firewall/nat/add chain=dstnat dst-address=192.168.88.1 protocol=tcp dst-port=8080 \
    action=dst-nat to-addresses=172.18.0.2 to-ports=80 comment="awg: web UI"
```

The web UI is then at **http://192.168.88.1:8080/**. The firewall rule above still applies.

## 10. Send DNS through the tunnel

By default LAN devices ask the router for DNS, and the router asks your ISP's DNS servers directly.
The ISP can then see and tamper with lookups. To use public resolvers through the tunnel:

```routeros
/ip/dhcp-client/set [find] use-peer-dns=no
/ip/dns/set servers=1.1.1.1,1.0.0.1
/routing/rule/add dst-address=1.1.1.1/32 action=lookup table=to-awg comment="awg: DNS via tunnel" place-before=0
/routing/rule/add dst-address=1.0.0.1/32 action=lookup table=to-awg comment="awg: DNS via tunnel" place-before=0
/ip/dns/cache/flush
```

These rules also match the router's own queries. With `action=lookup`, DNS falls back to the direct
route when the tunnel is down.

## 11. IPv6

The container tunnels **IPv4 only**. If your ISP provides IPv6, some traffic will bypass the tunnel. Check:

```routeros
/ipv6/address/print
```

If there are global addresses (anything other than `fe80::`), disable IPv6:

```routeros
/ipv6/settings/set disable-ipv6=yes
```

Alternatively, disable IPv6 only on the LAN side: remove the IPv6 address from `bridge` and turn off
IPv6 router advertisements (ND).

## 12. Verify

1. Turn off any VPN client on your computer.
2. Open [ifconfig.me](https://ifconfig.me). It should show your **VPS** IP.
3. The web UI shows **connected**, and its traffic counters increase.
4. The route through the container is active (`A` flag):
   ```routeros
   /ip/route/print where routing-table=to-awg
   ```
5. Inside the container:
   ```routeros
   /container/shell [find interface=veth-awg]
   ```
   ```sh
   awg show                 # latest handshake, transfer
   ip rule                  # "iif veth-awg lookup 51820"
   ip route show table 51820
   exit
   ```
   Inside the container the network interface has the **same name as the veth** (`veth-awg`), not `eth0`.
   The backend detects this automatically. The *Logs* section of the web UI shows
   `tunnel: LAN interface is veth-awg`.

## 13. Update the image

The config is on the mounted disk, so it survives an update.

```routeros
/container/stop [find interface=veth-awg]
```

Wait until `/container/print` shows the container as stopped, then:

```routeros
/container/remove [find interface=veth-awg]
```

Upload the new `.tar` (step 5), then repeat steps 6.2 and 6.3. The tunnel comes back up automatically
with the saved config.

## 14. Uninstall

```routeros
/container/stop [find interface=veth-awg]
/container/remove [find interface=veth-awg]
/routing/rule/remove [find comment~"^awg"]
/ip/route/remove [find routing-table=to-awg]
/routing/table/remove [find name=to-awg]
/ip/firewall/mangle/remove [find comment="awg"]
/ip/firewall/filter/remove [find comment~"^awg"]
/ip/firewall/nat/remove [find comment~"^awg"]
/interface/list/member/remove [find interface=veth-awg]
/ip/address/remove [find interface=veth-awg]
/interface/veth/remove veth-awg
```

If you changed FastTrack for option C, restore it:

```routeros
/ip/firewall/filter/set [find action=fasttrack-connection] !connection-mark
```

If you changed DNS, restore it with `/ip/dhcp-client/set [find] use-peer-dns=yes` and
`/ip/dns/set servers=""`.

## 15. Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `bad parameter name` on `/container/mounts/add` | Wrong syntax for your RouterOS version: 7.20+ uses `list=` and `mountlists=`, older versions use `name=` and `mounts=`. See [6.1](#61-persistent-storage-for-the-config). |
| `input does not match any value of file` | The `file=` path is wrong. Run `/file/print where name~"amnezia"` and use the name exactly as shown. |
| Wrong image architecture / container fails to start immediately | Compare `architecture-name` in `/system/resource/print` with the image (`arm` → `armv7`, `arm64` → `arm64`). |
| `ping 172.18.0.2` times out, `fetch` says *Host is unreachable* | The veth address doesn't match the router address. Inside the container (`/container/shell …`, then `ip addr`) the address must be `172.18.0.2/30`. Fix it with the container stopped: `/interface/veth/set veth-awg address=172.18.0.2/30 gateway=172.18.0.1`. Also make sure the veth is not a bridge port. |
| `fetch` from the router works, but the browser loads forever | A VPN client on your computer routes `172.18.0.0/30` into its tunnel. See [step 7](#7-configure-the-tunnel-in-the-web-ui). On Linux, `ip route get 172.18.0.2` shows which interface is used. On Windows, use `Find-NetRoute -RemoteIPAddress 172.18.0.2`. |
| Browser can't connect at all | Check the web UI firewall rule from [step 9](#9-protect-the-web-ui): is your computer's IP in `awg-admins`? |
| Web UI shows **no handshake** | The container has no internet access (check NAT, [step 4](#4-create-the-container-network)), the UDP port is blocked on the VPS, or the config or key is outdated. Check *Logs* in the web UI. |
| SSH tab: `authentication failed` | Wrong user or password, or the server allows only key authentication. |
| SSH tab: `no running amnezia-awg / amnezia-awg2 container` | AmneziaWG was not installed on that server through the Amnezia app, or it is stopped. |
| SSH tab: `sudo` errors | The user must be allowed to use `sudo`. With key authentication, sudo must not require a password. |
| Tunnel is connected but traffic doesn't go through it | Check rule order (`/routing/rule/print`), and that the `to-awg` route is active (`/ip/route/print where routing-table=to-awg`). For option C, check the mangle counters (`/ip/firewall/mangle/print stats`) and the FastTrack change. |
| Some sites go direct even in full-tunnel mode | IPv6 ([step 11](#11-ipv6)), or DNS returns different results ([step 10](#10-send-dns-through-the-tunnel)). |
| Container stops on its own | `/log/print where topics~"container"`. Errors mentioning `/dev/net/tun` or `iptables` are worth [reporting](https://github.com/Namikko-chad/amneziawg-mikrotik/issues) together with your model and RouterOS version. |

When asking for help, please include:

```routeros
/system/resource/print
/container/print detail
/log/print where topics~"container"
/routing/rule/print
/ip/route/print where routing-table=to-awg
```

Include the *Logs* section of the web UI as well, with any IP addresses and keys you don't want to share removed.
