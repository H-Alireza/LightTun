# LightTun

A small tunnel manager for Ubuntu. It is built for servers that carry high-traffic VPN users and gamers at the same time.
It does the heavy work with kernel tunnels to keep CPU use low, puts gaming traffic ahead of bulk traffic, checks tunnel health and fails over automatically, and includes a one-command benchmark to choose the best tunnel type for each route.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/H-Alireza/LightTun/main/install.sh | sudo bash -s -- --fec
```

`--fec` adds tinyfecVPN + udp2raw, `--rathole` adds rathole and `--backpack` adds [BackPack](https://github.com/AminMGMT/BackPack). Drop them for kernel tunnels only.
To install from a copy of the repo instead, run `sudo bash install.sh --fec`.

Then run `lighttun` to open the menu, or use the commands below.

## Tunnel types

| Type | Runs in | CPU | Use it for |
|---|---|---|---|
| `gre` | kernel | very low | Default for bulk and gaming. Use `--key` to run several GRE tunnels between the same two servers. |
| `ipip` | kernel | lowest | Smallest overhead. Only one per server pair. |
| `vxlan` | kernel, UDP | low | When GRE is blocked. |
| `fou` | kernel, UDP | very low | GRE inside UDP: costs about the same as GRE but looks like UDP on the network. |
| `wg` | kernel WireGuard | low | Encrypted, so filters can't look inside. Good for gaming. Both keys come from one `--secret`. |
| `fec` | tinyfecVPN | medium | Lossy routes. Error correction removes lag spikes. Add `--raw yes` to wrap it in fake TCP (udp2raw) where UDP is throttled. |
| `rathole` | rathole | medium | Reverse TCP/UDP port tunnel, for when the far side can't accept incoming connections. Not for gaming. |
| `backpack` | BackPack | medium | Reverse port tunnel like rathole, but the abroad side keeps a pool of warm connections (no handshake per new user connection) with heartbeats. `--carrier tcp` (default), `tcpmux` (many short connections) or `kcp` (UDP with error correction). Use `--role server` on Iran, `client` abroad. |

Every tunnel survives reboots (it runs as a systemd service `lighttun@NAME` or `lighttun-proc@NAME`). Its config is in `/etc/lighttun/tunnels/NAME.conf`.

## Filtered routes (Iran → abroad): `--opener`

On many Iranian servers the filter works by **who starts the flow**, not by protocol:

- a flow the Iran server starts toward an abroad server gets about 6 packets through, then it is cut, and it stays cut while Iran keeps sending
- a flow the abroad server starts passes in both directions, whatever the protocol or port
- the filter forgets an idle flow after about 15–45 s

`--opener` makes the abroad server always open the tunnel:

```bash
# abroad server
lighttun add --name t1 --type vxlan --opener me   --local ABROAD --remote IRAN   --addr 10.77.20.2/30 ...
# Iran server (lighttun peer t1 prints this line for you)
lighttun add --name t1 --type vxlan --opener peer --local IRAN   --remote ABROAD --addr 10.77.20.1/30 ...
```

- `me` (abroad): sends a keepalive every second, so the flow never goes idle. `wg` does this with persistent-keepalive.
- `peer` (Iran): a kernel firewall rule (iptables `recent`) holds this side's tunnel packets until the abroad side has been heard from in the last 10 s, so Iran never starts the flow. A `wg` tunnel on this side has no endpoint at all.
- VXLAN and FOU use a fixed source port, so each tunnel is one flow.
- It costs almost no CPU, and it is harmless on routes that aren't filtered.

**Don't use GRE on filtered routes.** GRE is not encrypted and the filter reads the packets inside it: pings pass, but TCP connections inside get cut. Use `vxlan` (fastest) or `wg` (encrypted, safest if the filter starts reading VXLAN too).

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

The benchmark creates temporary GRE, IPIP, VXLAN, FOU and WireGuard tunnels, all opened by the abroad (serve) side as with `--opener`, plus these FEC and reverse variants:

- `fec`: FEC over plain UDP
- `fecraw`: FEC disguised as TCP (udp2raw)
- `fecrev`: FEC over plain UDP, opened by the abroad server (reverse)
- `fecicmp`: FEC disguised as ping
- `rathole`: reverse TCP tunnel, opened by the abroad server
- `bp`, `bpmux`, `bpkcp`: BackPack reverse tunnel over TCP, TCP with mux, and UDP+KCP+FEC, opened by the abroad server

The FEC variants need `install fec`, rathole needs `install rathole`, the BackPack ones need `install backpack` (or an existing `backpack` binary), and `wg` needs `wireguard-tools` (the installer adds it).
To test only some types, list them on both sides: `lighttun bench serve IP vxlan wg` and `lighttun bench run IP vxlan wg`.
A type that connects but loses more than half its packets is reported as **BLOCKED** and skipped right away, which is common on filtered Iranian routes.

On each type that works, it measures:

- ping and jitter (port tunnels: through a forwarded UDP echo port)
- loss and jitter on a game-like UDP stream
- ping while a TCP transfer is running (bufferbloat)
- upload and download speed
- CPU use, also per Gbit/s moved (CPU/G), so a slow tunnel does not look cheap
- the largest packet that really passes through the tunnel (MTU column), so you can set `--mtu`

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
/opt/lighttun/                     tinyvpn, udp2raw, rathole, backpack
```
