# LightTun

A small tunnel manager for Ubuntu. It is built for servers that carry high-traffic VPN users and gamers at the same time.
It does the heavy work with kernel tunnels to keep CPU use low, puts gaming traffic ahead of bulk traffic, checks tunnel health and fails over automatically, and includes a one-command benchmark to choose the best tunnel type for each route.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/H-Alireza/LightTun/main/install.sh | sudo bash -s -- --fec
```

`--fec` adds tinyfecVPN + udp2raw, and `--rathole` adds rathole. Drop both for kernel tunnels only.
To install from a copy of the repo instead, run `sudo bash install.sh --fec`.

Then run `lighttun` to open the menu, or use the commands below.

## Tunnel types

| Type | Runs in | CPU | Use it for |
|---|---|---|---|
| `gre` | kernel | very low | Default for bulk and gaming. Use `--key` to run several GRE tunnels between the same two servers. |
| `ipip` | kernel | lowest | Smallest overhead. Only one per server pair. |
| `vxlan` | kernel, UDP | low | When GRE is blocked. |
| `fou` | kernel, UDP | very low | GRE inside UDP: costs about the same as GRE but looks like UDP on the network. |
| `fec` | tinyfecVPN | medium | Lossy routes. Error correction removes lag spikes. Add `--raw yes` to wrap it in fake TCP (udp2raw) where UDP is throttled. |
| `rathole` | rathole | medium | Reverse TCP/UDP port tunnel, for when the far side can't accept incoming connections. Not for gaming. |

Every tunnel survives reboots (it runs as a systemd service `lighttun@NAME` or `lighttun-proc@NAME`). Its config is in `/etc/lighttun/tunnels/NAME.conf`.

## Everyday commands

```bash
lighttun add                       # wizard
lighttun add --name fnland --type gre --local 65.109.140.176 --remote 94.139.181.126 --addr 10.10.13.2/30 --mtu 1420
lighttun peer fnland               # prints the exact command to run on the other server
lighttun edit fnland               # wizard with the current values
lighttun set fnland MTU=1400 PRIORITY=gaming
lighttun del fnland
lighttun list | status | health
lighttun restart all
```

### Port forwarding (entry server → exit server)

`--tcp "443 8443"` and `--udp "51820 10000:10100"` forward this server's public ports through the tunnel to the peer.
Clients connect to the entry server, and Xray/WireGuard runs on the exit server, listening on its tunnel IP or `0.0.0.0`.
`--backup OTHER_TUNNEL` moves those ports to another tunnel automatically if this one goes down or stays degraded, and moves them back once it recovers.

## Gaming and bulk on the same server

Use two tunnels between the same servers, one per traffic type, and turn on QoS:

```bash
# on the entry server
lighttun add --name bulk  --type gre --key 1 --priority bulk   --local A --remote B --addr 10.77.10.1/30 --tcp "443 8443"
lighttun add --name game  --type gre --key 2 --priority gaming --local A --remote B --addr 10.77.11.1/30 --udp "51820" --backup bulk
lighttun qos on --bw 950mbit      # set ~5% below the real upload speed
```

How it works:

- Each tunnel's outer packets carry a priority mark: `gaming` gets EF, `bulk` gets CS1, `normal` gets none.
- `qos on` puts the **cake** queue on the WAN interface. It sends gaming packets first and keeps bulk traffic from building a ping-raising queue. The marks are cleared again after sorting, so ISPs never see them.
- Run the same setup on the other server, so the return direction is covered too.
- QoS only helps when the link is actually busy. Shaping uses a little CPU, so on a 10 Gbit link with plenty of spare capacity, leave it off.

Client protocol matters: for gaming, give players **WireGuard** or another UDP protocol (Hysteria2, TUIC), forwarded over the gaming tunnel with `--udp`.
VLESS/VMess over TCP carries game UDP inside TCP, which causes stutter whatever the tunnel is.

## Benchmark: pick the best tunnel for a route

```bash
# server B (start first)
lighttun bench serve <IP of A>
# server A
lighttun bench run <IP of B>
```

The benchmark creates temporary GRE, IPIP, VXLAN and FOU tunnels, plus these FEC and reverse variants:

- `fec`: FEC over plain UDP
- `fecraw`: FEC disguised as TCP (udp2raw)
- `fecrev`: the same, but the abroad server opens the connection (reverse)
- `fecicmp`: FEC disguised as ping
- `rathole`: reverse TCP tunnel, opened by the abroad server

The FEC variants need `install fec` and rathole needs `install rathole`.
A type that connects but loses more than half its packets is reported as **BLOCKED** and skipped right away, which is common on filtered Iranian routes.

On each type that works, it measures:

- ping and jitter
- loss and jitter on a game-like UDP stream
- ping while a TCP transfer is running (bufferbloat)
- upload and download speed
- CPU use

Then it ranks the tunnels with a **gaming score** and a **bulk score**, and removes everything when it's done. A full run takes about 2-3 minutes.

## Health monitor and alerts

`lighttun-monitor` pings each tunnel's peer: every 5 s for gaming tunnels and every 15 s for the others. It tracks ping, jitter and loss.

- **Degraded:** loss or jitter is above the limit. Set the limits with `MAX_LOSS` and `MAX_JITTER`.
- **Down:** 100% loss. After 3 failed checks it restarts the tunnel, at most once a minute.
- **Failover:** forwarded ports move to the backup tunnel and come back once the tunnel recovers.

`lighttun telegram` sets up alerts. Servers in Iran can't reach the Telegram API directly, so set a proxy there (`socks5h://...`) or only enable alerts on the foreign servers.

## Server tuning (`lighttun optimize`, applied at install)

- BBR congestion control with fq
- Large socket buffers
- A large conntrack table, with longer UDP timeouts so game sessions don't drop
- 1M file descriptors
- `rp_filter` off and forwarding on
- Bigger NIC ring buffers, irqbalance, and the performance CPU governor where available
- MSS clamping on every tunnel
- No conntrack for the outer packets of kernel tunnels

Settings are in `/etc/sysctl.d/99-lighttun.conf`.

## Files

```
/usr/local/bin/lighttun            the script
/etc/lighttun/tunnels/*.conf       one file per tunnel
/etc/lighttun/lighttun.conf        QoS + Telegram settings
/var/lib/lighttun/                 health state
/opt/lighttun/                     tinyvpn, udp2raw, rathole
```
