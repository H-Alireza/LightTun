#!/usr/bin/env python3
"""Load test for ltcar: many users arriving at once, like a relay restart.

  load.py echo PORT                    (abroad side) TCP + UDP echo on 127.0.0.1:PORT
  load.py run PORT TCP UDP SECONDS     (Iran side) open TCP connections and UDP flows to 127.0.0.1:PORT all at
                                       once, each sends a small message every 2 s and waits for the echo
"""
import asyncio, socket, sys, time


async def echo(port):
    async def tcp(r, w):
        try:
            while d := await r.read(65536):
                w.write(d)
                await w.drain()
        except (ConnectionError, OSError):
            pass
        w.close()

    class Udp(asyncio.DatagramProtocol):
        def connection_made(self, t): self.t = t
        def datagram_received(self, d, a): self.t.sendto(d, a)

    loop = asyncio.get_running_loop()
    await loop.create_datagram_endpoint(Udp, local_addr=("127.0.0.1", port))
    srv = await asyncio.start_server(tcp, "127.0.0.1", port, backlog=8192)
    await srv.serve_forever()


async def run(port, ntcp, nudp, secs):
    st = dict(tcp_ok=0, tcp_fail=0, tcp_rt=[], first=[], udp_ok=0, udp_lost=0, udp_rt=[], udp_first_fail=0)
    end = time.time() + secs

    async def tcp_user(i):
        t0 = time.time()
        try:
            r, w = await asyncio.wait_for(asyncio.open_connection("127.0.0.1", port), 10)
            first = True
            while time.time() < end:
                t = time.time()
                w.write(b"x" * 100)
                await w.drain()
                await asyncio.wait_for(r.readexactly(100), 10)
                st["tcp_rt"].append(time.time() - t)
                if first:
                    st["first"].append(time.time() - t0); first = False
                st["tcp_ok"] += 1
                await asyncio.sleep(2)
            w.close()
        except Exception:
            st["tcp_fail"] += 1

    loop = asyncio.get_running_loop()

    async def udp_user(i):
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        s.setblocking(False)
        s.connect(("127.0.0.1", port))
        got_first = False
        n = 0
        while time.time() < end:
            m = b"%d:%d" % (i, n); n += 1
            t = time.time()
            s.send(m)
            try:
                while True:
                    d = await asyncio.wait_for(loop.sock_recv(s, 2048), 3)
                    if d == m:
                        break
                st["udp_rt"].append(time.time() - t); st["udp_ok"] += 1; got_first = True
            except asyncio.TimeoutError:
                st["udp_lost"] += 1
            await asyncio.sleep(max(0, 2 - (time.time() - t)))
        if not got_first:
            st["udp_first_fail"] += 1
        s.close()

    await asyncio.gather(*[tcp_user(i) for i in range(ntcp)], *[udp_user(i) for i in range(nudp)])

    def pct(a, p):
        a = sorted(a)
        return a[min(len(a) - 1, int(len(a) * p))] * 1000 if a else float("nan")
    print("TCP users %d: failed %d, echoes %d, first echo p50 %.0f / p95 %.0f / max %.0f ms, echo rtt p50 %.0f / p95 %.0f ms"
          % (ntcp, st["tcp_fail"], st["tcp_ok"], pct(st["first"], .5), pct(st["first"], .95), pct(st["first"], 1),
             pct(st["tcp_rt"], .5), pct(st["tcp_rt"], .95)))
    print("UDP flows %d: never answered %d, echoes %d, lost %d, rtt p50 %.0f / p95 %.0f ms"
          % (nudp, st["udp_first_fail"], st["udp_ok"], st["udp_lost"], pct(st["udp_rt"], .5), pct(st["udp_rt"], .95)))


if __name__ == "__main__":
    if sys.argv[1] == "echo":
        asyncio.run(echo(int(sys.argv[2])))
    else:
        asyncio.run(run(int(sys.argv[2]), int(sys.argv[3]), int(sys.argv[4]), float(sys.argv[5])))
