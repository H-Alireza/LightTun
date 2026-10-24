#!/bin/bash
# lt.sh up server | up client IRAN_IP | down | run ROUNDS | urun ROUNDS     (temporary, all under /root/lttest)
#   S: ltcar spread over 16 tunnel ports 15900-15915, users 15882/15883; H: same over 15920-15935 with -hop 20s,
#      users 15892/15893   (prun ROUNDS compares L, S, H and M)
#   L: ltcar (Go, split TCP, one pre-dialed carrier per user connection, pool 64)  tunnel 15870, users 15872/15873
#   M: BackPack tcpmux, pool 8, mux_con 16                                         tunnel 15850, users 15852/15853
# Download hz -> s176: iperf 16 and 64 streams (1 MB receive buffers) 15 s; then 20 fetches of a 256 KB file
# (one new connection each, like page loads), time to first byte and total.
# urun: UDP download through each tunnel (iperf3 -u at fixed rates, 1400-byte datagrams), then UDP echo RTT idle
# and while 16 TCP streams download through the same tunnel (shows whether UDP queues behind bulk TCP).
D=/root/lttest BP=/usr/local/bin/backpack TOK=lt-ltcartest-71c9e2a4
down() { pkill -f "[l]tcar (server|client)"; pkill -f "[b]ackpack -c $D/"; pkill -f "[l]t-lt-iperf"; pkill -f "[i]perf3 -s -B 127.0.0.1 -p 158"; pkill -f "[h]ttp.server 158"; pkill -f "[l]t-lt-echo"; sleep 1
         rm -rf $D; echo "down, left: $(pgrep -fc '[l]tcar|backpack -c /root/lttest')"; }
[[ $1 == down ]] && { down; exit; }
mux() {
  if [[ $1 == server ]]; then
    printf '[server]\nbind_addr = "0.0.0.0:15850"\nports = ["15852", "15853"]\naccept_udp = true\nheartbeat = 40\nchannel_size = 4096\nmux_con = 16\n'
  else
    printf '[client]\nremote_addr = "%s:15850"\nconnection_pool = 8\naggressive_pool = false\nretry_interval = 3\ndial_timeout = 10\nmux_session = 8\n' "$2"
  fi
  printf 'transport = "tcpmux"\ntoken = "lt-lttest-mux-4e19b6"\nnodelay = true\nkeepalive_period = 75\nlog_level = "warn"\nskip_optz = true\nso_rcvbuf = 8388608\nso_sndbuf = 8388608\nmux_version = 2\nmux_framesize = 32768\nmux_recievebuffer = 16777216\nmux_streambuffer = 2097152\n'
}
if [[ $1 == up ]]; then
  down >/dev/null; mkdir -p $D; mv /root/ltcar $D/; chmod +x $D/ltcar
  if [[ $2 == server ]]; then
    us(){ echo 127.0.0.1:$1,127.0.0.1:$2,udp:127.0.0.1:$1,udp:127.0.0.1:$2; }
    setsid $D/ltcar server -listen 0.0.0.0:15870 -users $(us 15872 15873) -token $TOK > $D/L.log 2>&1 </dev/null &
    setsid $D/ltcar server -listen 0.0.0.0:15900-15915 -users $(us 15882 15883) -token $TOK > $D/S.log 2>&1 </dev/null &
    setsid $D/ltcar server -listen 0.0.0.0:15920-15935 -users $(us 15892 15893) -token $TOK > $D/H.log 2>&1 </dev/null &
    mux server > $D/M.toml
  else
    setsid $D/ltcar client -remote $3:15870 -pool 64 -token $TOK > $D/L.log 2>&1 </dev/null &
    setsid $D/ltcar client -remote $3:15900-15915 -pool 64 -token $TOK > $D/S.log 2>&1 </dev/null &
    setsid $D/ltcar client -remote $3:15920-15935 -pool 64 -hop 20s -token $TOK > $D/H.log 2>&1 </dev/null &
    mux client $3 > $D/M.toml
    head -c 262144 /dev/urandom > $D/f256k
    for p in 15872 15882 15892 15852; do setsid bash -c "while :; do iperf3 -s -B 127.0.0.1 -p $p; sleep 0.5; done # lt-lt-iperf" >/dev/null 2>&1 </dev/null & done
    for p in 15873 15883 15893 15853; do setsid python3 -c "import socket
s=socket.socket(2,2); s.bind(('127.0.0.1',$p))
while 1:
  d,a=s.recvfrom(65536); s.sendto(d,a)" lt-lt-echo >/dev/null 2>&1 </dev/null & done
    for p in 15873 15883 15893 15853; do (cd $D && setsid python3 -m http.server $p --bind 127.0.0.1 >/dev/null 2>&1 </dev/null &) ; done
  fi
  setsid $BP -c $D/M.toml > $D/M.log 2>&1 </dev/null &
  sleep 3; echo started; pgrep -fa '[l]tcar|backpack -c /root/lttest' | cut -c1-90; exit
fi
if [[ $1 == run ]]; then
  HZ=$(getconf CLK_TCK)
  sm(){ python3 -c 'import json,sys
try:
  d=json.load(sys.stdin); print(round(d["end"]["sum_received"]["bits_per_second"]/1e6), "| per 5 s:", " ".join(str(round(i["sum"]["bits_per_second"]/1e6)) for i in d["intervals"]))
except Exception: print("-")'; }
  cpu(){ for p in $(pgrep -f "$1"); do awk '{print $14+$15}' /proc/$p/stat; done | awk '{s+=$1} END{print s+0}'; }
  med(){ sort -n | awk '{a[NR]=$1} END{printf "median %.0f ms, worst %.0f ms", a[int((NR+1)/2)]*1000, a[NR]*1000}'; }
  ss -tn state established "( sport = :15870 or sport = :15850 )" | tail -n +2 | awk '{print $3}' | sort | uniq -c | sed 's/^/carriers: /'
  for r in $(seq ${2:-2}); do
    for P in 16 64; do
      o="L M"; (( r % 2 == 0 )) && o="M L"
      for v in $o; do
        case $v in L) p=15872 pat="ltcar server" n="ltcar (Go)" ;; M) p=15852 pat="backpack -c $D/M.toml" n="tcpmux    " ;; esac
        lo=$(awk '{print $1}' /proc/sys/net/ipv4/tcp_mem)
        for i in $(seq 30); do (( $(awk '/^TCP:/{print $NF}' /proc/net/sockstat) < lo / 4 )) && break; sleep 1; done
        pr1=$(nstat -az TcpExtTCPMemoryPressures | awk 'NR>1{print $2}')
        c1=$(cpu "$pat")
        x=$(timeout 25 iperf3 -c 127.0.0.1 -p $p -t 15 -i 5 -P $P -w 1M -R -J 2>/dev/null | sm)
        c2=$(cpu "$pat")
        pr2=$(nstat -az TcpExtTCPMemoryPressures | awk 'NR>1{print $2}')
        echo "$(date -u +%T) round $r P$P $n: down $x Mbit/s | tunnel cpu $(( (c2-c1)*100/HZ/15 ))% of a core | s176 pressure +$((pr2-pr1))"
      done
    done
    for v in L M; do
      case $v in L) p=15873 n="ltcar (Go)" ;; M) p=15853 n="tcpmux    " ;; esac
      out=$(for i in $(seq 20); do curl -s -o /dev/null -m 10 -w '%{time_starttransfer} %{time_total}\n' http://127.0.0.1:$p/f256k || echo "10 10"; done)
      echo "$(date -u +%T) round $r 256 KB fetch $n: first byte $(awk '{print $1}' <<<"$out" | med) | total $(awk '{print $2}' <<<"$out" | med)"
    done
  done
  tail -qn 1 $D/L.log
fi
if [[ $1 == urun ]]; then
  HZ=$(getconf CLK_TCK)
  cpu(){ for p in $(pgrep -f "$1"); do awk '{print $14+$15}' /proc/$p/stat; done | awk '{s+=$1} END{print s+0}'; }
  us(){ python3 -c 'import json,sys
try:
  e=json.load(sys.stdin)["end"]; s=e.get("sum_received") or e["sum"]; t=e["sum"]
  l = t.get("lost_percent", 0); print("%d Mbit/s delivered (sent %d), lost %.1f%%, jitter %.1f ms" % (s["bits_per_second"]/1e6*(1-l/100), s["bits_per_second"]/1e6, l, t.get("jitter_ms", 0)))
except Exception as x: print("-", x)'; }
  ping(){ python3 - "$@" <<'PY'
import socket, sys, time
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(1)
a, n, r = ("127.0.0.1", int(sys.argv[1])), int(sys.argv[2]), []
for i in range(n):
    t = time.time(); s.sendto(str(i).encode().ljust(64), a)
    try:
        while True:
            d, _ = s.recvfrom(2048)
            if d.split()[0] == str(i).encode():
                r.append((time.time() - t) * 1000); break
    except socket.timeout:
        pass
    time.sleep(0.02)
r.sort()
print("rtt median %.0f ms, p95 %.0f ms, worst %.0f ms, lost %d/%d" % (r[len(r) // 2], r[int(len(r) * .95)], r[-1], n - len(r), n) if r else "all lost")
PY
  }
  for r in $(seq ${2:-2}); do
    o="L M"; (( r % 2 == 0 )) && o="M L"
    for b in "100M 1" "400M 1" "800M 1" "200M 4"; do
      set -- $b
      for v in $o; do
        case $v in L) p=15872 pat="ltcar server" n="ltcar (Go)" ;; M) p=15852 pat="backpack -c $D/M.toml" n="tcpmux    " ;; esac
        c1=$(cpu "$pat")
        x=$(timeout 20 iperf3 -c 127.0.0.1 -p $p -u -b $1 -l 1400 -P $2 -t 10 -R -J 2>/dev/null | us)
        c2=$(cpu "$pat")
        echo "$(date -u +%T) round $r UDP $2 x $1 $n: $x | tunnel cpu $(( (c2-c1)*100/HZ/10 ))% of a core"
      done
    done
    for v in $o; do
      case $v in L) p=15872 e=15873 n="ltcar (Go)" ;; M) p=15852 e=15853 n="tcpmux    " ;; esac
      echo "$(date -u +%T) round $r UDP echo idle      $n: $(ping $e 100)"
      (timeout 20 iperf3 -c 127.0.0.1 -p $p -t 12 -P 16 -w 1M -R > /tmp/lt-load.out 2>&1 &); sleep 2
      echo "$(date -u +%T) round $r UDP echo + 16 TCP $n: $(ping $e 100)"
      sleep 8; echo "                          (TCP load during that: $(grep -E 'SUM.*receiver' /tmp/lt-load.out | awk '{print $6, $7}'))"
    done
  done
  tail -qn 1 $D/L.log
fi
if [[ $1 == prun ]]; then
  HZ=$(getconf CLK_TCK)
  sm(){ python3 -c 'import json,sys
try:
  d=json.load(sys.stdin); print(round(d["end"]["sum_received"]["bits_per_second"]/1e6), "| per 5 s:", " ".join(str(round(i["sum"]["bits_per_second"]/1e6)) for i in d["intervals"]))
except Exception: print("-")'; }
  us(){ python3 -c 'import json,sys
try:
  e=json.load(sys.stdin)["end"]; t=e["sum"]; l=t.get("lost_percent", 0); print("%d Mbit/s delivered, lost %.1f%%" % (t["bits_per_second"]/1e6*(1-l/100), l))
except Exception as x: print("-", x)'; }
  cpu(){ for p in $(pgrep -f "$1"); do awk '{print $14+$15}' /proc/$p/stat; done | awk '{s+=$1} END{print s+0}'; }
  med(){ sort -n | awk '{a[NR]=$1} END{printf "median %.0f ms, worst %.0f ms", a[int((NR+1)/2)]*1000, a[NR]*1000}'; }
  calm(){ lo=$(awk '{print $1}' /proc/sys/net/ipv4/tcp_mem); for i in $(seq 30); do (( $(awk '/^TCP:/{print $NF}' /proc/net/sockstat) < lo / 4 )) && break; sleep 1; done; }
  pick(){ case $1 in L) p=15872 pat="ltcar server -listen 0.0.0.0:15870 " n="ltcar 1 port     " ;;
                     S) p=15882 pat="ltcar server -listen 0.0.0.0:15900-" n="ltcar 16 ports   " ;;
                     H) p=15892 pat="ltcar server -listen 0.0.0.0:15920-" n="ltcar 16 + hop   " ;;
                     M) p=15852 pat="backpack -c $D/M.toml" n="tcpmux           " ;; esac; }
  ss -tn state established "( sport >= :15900 and sport <= :15935 )" | tail -n +2 | awk '{sub(/.*:/,"",$3); print $3}' | sort | uniq -c | awk '{printf "%s:%s ", $2, $1} END{print ""}' | sed 's/^/carriers per port: /'
  for r in $(seq ${2:-2}); do
    o="L S H M"; (( r % 2 == 0 )) && o="M H S L"
    for t in P16 P64 UDP; do
      for v in $o; do
        pick $v; calm; c1=$(cpu "$pat")
        case $t in
          P16) x=$(timeout 25 iperf3 -c 127.0.0.1 -p $p -t 15 -i 5 -P 16 -w 1M -R -J 2>/dev/null | sm); s=15 ;;
          P64) x=$(timeout 25 iperf3 -c 127.0.0.1 -p $p -t 15 -i 5 -P 64 -w 1M -R -J 2>/dev/null | sm); s=15 ;;
          UDP) x=$(timeout 20 iperf3 -c 127.0.0.1 -p $p -u -b 200M -l 1400 -P 4 -t 10 -R -J 2>/dev/null | us); s=10 ;;
        esac
        c2=$(cpu "$pat")
        echo "$(date -u +%T) round $r $t $n: $x | cpu $(( (c2-c1)*100/HZ/s ))%"
      done
    done
    for v in $o; do
      pick $v; e=$((p+1))
      out=$(for i in $(seq 20); do curl -s -o /dev/null -m 10 -w '%{time_starttransfer} %{time_total}\n' http://127.0.0.1:$e/f256k || echo "10 10"; sleep 1; done)
      echo "$(date -u +%T) round $r fetch $n: first byte $(awk '{print $1}' <<<"$out" | med) | total $(awk '{print $2}' <<<"$out" | med)"
    done
  done
  tail -qn 1 $D/L.log $D/S.log $D/H.log
fi
