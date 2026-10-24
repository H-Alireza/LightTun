// ltcar: split-TCP reverse tunnel. Each user connection is terminated on the Iran side and carried over its own
// TCP connection from the abroad side, so only one TCP crosses the international link (no TCP-over-TCP) and one
// slow user never blocks another (one stream per carrier, no multiplexing). Carriers are reused: when a user
// connection ends cleanly its carrier goes back to the pool with its congestion window intact, and the most
// recently used carriers are handed out first, so short requests skip slow start.
//
//	server (Iran):   ltcar server -listen 0.0.0.0:PORT -users 0.0.0.0:443,udp:0.0.0.0:443 -token T
//	client (abroad): ltcar client -remote IRAN_IP:PORT -target 127.0.0.1 -pool 64 -token T
//
// LightTun runs it as tunnel type "ltcar" (lighttun install ltcar builds it from this file).
//
// UDP rides over the same TCP carriers (raw UDP from abroad is throttled on these routes): each source address on
// the Iran side is a flow with its own carrier, one datagram per frame, ended after -udp-idle without traffic.
//
// Ports: -listen and -remote take a range (host:15900-15915) to spread carriers over many ports, and -hop retires
// idle carriers after a jittered lifetime so the pool keeps moving to fresh connections. A busy carrier is never
// moved (that would reorder the user's bytes); it hops once its user is done.
//
// Wire: the client dials and sends MAGIC+sha256(token). While idle it sends 0x00 every 10 s; the server watches
// for that. The server claims an idle carrier, sends [proto 0 tcp/1 udp][target port:16], the client answers 0x01
// (anything before it is a late ping), then both sides send frames [type:8 len:24]: DATA, FIN (clean end of that
// direction) or RST (abort). Once each side has sent and received one FIN/RST the carrier is idle again on both ends.
//
// Carrier socket buffers are not autotuned: autotuned carriers grew to ~20 MB each and pushed the Iran side into
// TCP memory pressure, which clamps every socket's window (including the users'). Instead a fixed budget per side
// (default 32 MB) is split over the carriers carrying a stream, each getting between 256 KB and -buf (8 MB).
// The link needs only its bandwidth-delay product in flight (~25 MB at 2 Gbit/s and 100 ms), however many users.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"flag"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	fData = 0
	fFin  = 1
	fRst  = 2
	chunk = 256 << 10
)

var (
	magic   = []byte("LTC3")
	hello   []byte
	ccName  string
	lowat   int
	sockBuf int
	udpIdle time.Duration
	hop     time.Duration
	pingInt = 10 * time.Second
	idleMax = 35 * time.Second

	served, reused, waited, failed, dials, dialErr, udpFlows, udpDrops, hops atomic.Int64
)

func sockopts(_, _ string, rc syscall.RawConn) error {
	return rc.Control(func(fd uintptr) {
		if ccName != "" {
			syscall.SetsockoptString(int(fd), syscall.IPPROTO_TCP, syscall.TCP_CONGESTION, ccName)
		}
		if lowat > 0 {
			syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, 25, lowat) // TCP_NOTSENT_LOWAT
		}
		if sockBuf > 0 { // before connect/listen, so the window scale covers it
			syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, sockBuf)
			syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, sockBuf)
		}
	})
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

type carrier struct {
	c    *net.TCPConn
	r    *bufio.Reader
	wmu  sync.Mutex
	born time.Time
	life time.Duration // client: retire when idle past this (0: never)
}

// expand turns "host:a-b" into one address per port; "host:p" stays as is.
func expand(addr string) []string {
	h, ps, err := net.SplitHostPort(addr)
	if err != nil {
		log.Fatal(err)
	}
	a, b, isRange := strings.Cut(ps, "-")
	if !isRange {
		return []string{addr}
	}
	lo, err1 := strconv.Atoi(a)
	hi, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil || lo > hi || hi > 65535 {
		log.Fatalf("bad port range %q", ps)
	}
	var out []string
	for p := lo; p <= hi; p++ {
		out = append(out, net.JoinHostPort(h, strconv.Itoa(p)))
	}
	return out
}

var (
	budget  int
	amu     sync.Mutex
	active  = map[*carrier]struct{}{}
	applied int
)

func (k *carrier) setBuf(n int) {
	if rc, err := k.c.SyscallConn(); err == nil {
		rc.Control(func(fd uintptr) {
			syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, n)
			syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, n)
		})
	}
}

// track adds or removes a carrier from the busy set and re-splits the buffer budget when the share moved by >25%.
func track(k *carrier, add bool) {
	if sockBuf <= 0 || budget <= 0 {
		return
	}
	amu.Lock()
	defer amu.Unlock()
	if add {
		active[k] = struct{}{}
	} else {
		delete(active, k)
	}
	per := max(256<<10, min(sockBuf, budget/max(len(active), 1)))
	if add {
		k.setBuf(per)
	}
	if per*4 < applied*3 || per*3 > applied*4 {
		for kk := range active {
			kk.setBuf(per)
		}
		applied = per
	}
}

func newCarrier(c *net.TCPConn) *carrier {
	c.SetNoDelay(true)
	c.SetKeepAlive(true)
	c.SetKeepAlivePeriod(20 * time.Second)
	return &carrier{c: c, r: bufio.NewReaderSize(c, 64<<10), born: time.Now()}
}

func (k *carrier) frame(t byte, p []byte) error { // p has 4 free bytes in front of the payload
	n := len(p) - 4
	p[0], p[1], p[2], p[3] = t, byte(n>>16), byte(n>>8), byte(n)
	k.wmu.Lock()
	defer k.wmu.Unlock()
	_, err := k.c.Write(p)
	return err
}

// stream relays one user connection over the carrier. It returns true when the carrier is clean and reusable.
// With ack set (server side), the client's 0x01 is read before the first frame, while the user's bytes already flow.
func stream(k *carrier, u *net.TCPConn, ack bool) bool {
	u.SetNoDelay(true)
	track(k, true)
	defer track(k, false)
	var sentEnd atomic.Bool
	var broken atomic.Bool
	end := func(t byte) {
		if sentEnd.CompareAndSwap(false, true) {
			if k.frame(t, make([]byte, 4)) != nil {
				broken.Store(true)
			}
		}
	}
	upDone := make(chan struct{})
	go func() { // user -> carrier
		defer close(upDone)
		buf := make([]byte, 4+chunk)
		for {
			n, err := u.Read(buf[4:])
			if n > 0 && !sentEnd.Load() {
				if k.frame(fData, buf[:4+n]) != nil {
					broken.Store(true)
					k.c.Close()
					return
				}
			}
			if err != nil {
				if err == io.EOF {
					end(fFin)
				} else {
					end(fRst)
				}
				return
			}
		}
	}()
	// carrier -> user, until the peer's FIN/RST
	hdr := make([]byte, 4)
	buf := make([]byte, chunk)
	uok := true
	if ack {
		k.c.SetReadDeadline(time.Now().Add(15 * time.Second))
		for {
			b, err := k.r.ReadByte()
			if err != nil {
				broken.Store(true)
				break
			}
			if b == 1 {
				break
			}
		}
		k.c.SetReadDeadline(time.Time{})
	}
	for !broken.Load() {
		if _, err := io.ReadFull(k.r, hdr); err != nil {
			broken.Store(true)
			break
		}
		n := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
		if hdr[0] == fData {
			for n > 0 {
				m, err := k.r.Read(buf[:min(n, len(buf))])
				if err != nil {
					broken.Store(true)
					break
				}
				n -= m
				if uok {
					if _, err := u.Write(buf[:m]); err != nil {
						uok = false
						u.SetLinger(0)
						u.Close() // stops the up goroutine, which sends RST
					}
				}
			}
			if broken.Load() {
				break
			}
			continue
		}
		if hdr[0] == fFin && uok {
			u.CloseWrite()
		} else {
			u.SetLinger(0)
			u.Close()
		}
		break
	}
	if broken.Load() {
		k.c.Close()
		u.Close()
		<-upDone
		return false
	}
	<-upDone
	u.Close()
	return !broken.Load()
}

// ---------------- server (Iran) ----------------

type idle struct {
	k     *carrier
	state atomic.Int32 // 0 idle, 1 claimed, 2 dead
	done  chan struct{}
}

func (ic *idle) watch() {
	defer close(ic.done)
	for {
		ic.k.c.SetReadDeadline(time.Now().Add(idleMax))
		b, err := ic.k.r.ReadByte()
		if err != nil || b != 0 {
			if ic.state.CompareAndSwap(0, 2) {
				ic.k.c.Close()
			}
			return
		}
	}
}

type stack struct {
	mu     sync.Mutex
	list   []*idle
	notify chan struct{}
}

func (s *stack) push(k *carrier) {
	ic := &idle{k: k, done: make(chan struct{})}
	go ic.watch()
	s.mu.Lock()
	s.list = append(s.list, ic)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *stack) pop() *idle {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.list) > 0 {
		ic := s.list[len(s.list)-1]
		s.list = s.list[:len(s.list)-1]
		if ic.state.CompareAndSwap(0, 1) {
			return ic
		}
	}
	return nil
}

func (s *stack) sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.list[:0]
	for _, ic := range s.list {
		if ic.state.Load() == 0 {
			l = append(l, ic)
		}
	}
	s.list = l
	return len(l)
}

func server(listen, users string) {
	pool := &stack{notify: make(chan struct{}, 1)}
	lc := net.ListenConfig{Control: sockopts}
	for _, addr := range expand(listen) {
		ln, err := lc.Listen(context.Background(), "tcp", addr)
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					continue
				}
				go func(c *net.TCPConn) {
					c.SetReadDeadline(time.Now().Add(5 * time.Second))
					h := make([]byte, len(hello))
					if _, err := io.ReadFull(c, h); err != nil || subtle.ConstantTimeCompare(h, hello) != 1 {
						c.Close()
						return
					}
					pool.push(newCarrier(c))
				}(c.(*net.TCPConn))
			}
		}()
	}

	claim := func() *carrier {
		t := time.NewTimer(5 * time.Second)
		defer t.Stop()
		first := true
		for {
			ic := pool.pop()
			if ic == nil {
				if first {
					waited.Add(1)
					first = false
				}
				select {
				case <-pool.notify:
					continue
				case <-t.C:
					return nil
				}
			}
			ic.k.c.SetReadDeadline(time.Now()) // stop the watcher
			<-ic.done
			ic.k.c.SetReadDeadline(time.Time{})
			return ic.k
		}
	}

	for _, u := range strings.Split(users, ",") {
		udp := strings.HasPrefix(u, "udp:")
		u = strings.TrimPrefix(strings.TrimPrefix(u, "udp:"), "tcp:")
		_, ps, err := net.SplitHostPort(u)
		if err != nil {
			log.Fatal(err)
		}
		port, _ := strconv.Atoi(ps)
		if udp {
			go udpServer(u, port, claim, pool)
			continue
		}
		uln, err := net.Listen("tcp", u)
		if err != nil {
			log.Fatal(err)
		}
		go func(port int) {
			for {
				uc, err := uln.Accept()
				if err != nil {
					continue
				}
				go func(u *net.TCPConn) {
					k := claim()
					if k == nil {
						failed.Add(1)
						u.Close()
						return
					}
					k.wmu.Lock()
					_, err := k.c.Write([]byte{0, byte(port >> 8), byte(port)})
					k.wmu.Unlock()
					if err != nil {
						failed.Add(1)
						k.c.Close()
						u.Close()
						return
					}
					served.Add(1)
					if stream(k, u, true) {
						pool.push(k)
					}
				}(uc.(*net.TCPConn))
			}
		}(port)
	}
	for {
		time.Sleep(30 * time.Second)
		log.Printf("idle %d  served %d  udp flows %d  udp drops %d  waited %d  failed %d",
			pool.sweep(), served.Load(), udpFlows.Load(), udpDrops.Load(), waited.Load(), failed.Load())
	}
}

// ---------------- UDP ----------------

// dgram relays one UDP flow over the carrier, one datagram per DATA frame. Datagrams from in are batched into one
// write when several are queued; when in is closed (the flow ended) a FIN follows. out delivers the peer's
// datagrams; onEnd stops the producer of in (it must close in) once the peer's FIN/RST or a carrier error arrives.
// Returns true when the carrier is clean and reusable.
func dgram(k *carrier, in <-chan []byte, out func([]byte), onEnd func(), ack bool) bool {
	track(k, true)
	defer track(k, false)
	var broken atomic.Bool
	upDone := make(chan struct{})
	go func() {
		defer close(upDone)
		b := make([]byte, 0, chunk)
		add := func(p []byte) {
			n := len(p)
			b = append(b, fData, byte(n>>16), byte(n>>8), byte(n))
			b = append(b, p...)
		}
		flush := func() {
			if len(b) > 0 && !broken.Load() {
				k.wmu.Lock()
				_, err := k.c.Write(b)
				k.wmu.Unlock()
				if err != nil {
					broken.Store(true)
					k.c.Close()
				}
			}
			b = b[:0]
		}
		for p := range in {
			add(p)
		more:
			for len(b) < chunk/2 {
				select {
				case p, ok := <-in:
					if !ok {
						break more
					}
					add(p)
				default:
					break more
				}
			}
			flush()
		}
		if !broken.Load() && k.frame(fFin, make([]byte, 4)) != nil {
			broken.Store(true)
		}
	}()
	if ack {
		k.c.SetReadDeadline(time.Now().Add(15 * time.Second))
		for {
			c, err := k.r.ReadByte()
			if err != nil {
				broken.Store(true)
				break
			}
			if c == 1 {
				break
			}
		}
		k.c.SetReadDeadline(time.Time{})
	}
	hdr := make([]byte, 4)
	buf := make([]byte, 1<<16)
	for !broken.Load() {
		if _, err := io.ReadFull(k.r, hdr); err != nil {
			broken.Store(true)
			break
		}
		n := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
		if hdr[0] != fData {
			break
		}
		if n > len(buf) {
			broken.Store(true)
			break
		}
		if _, err := io.ReadFull(k.r, buf[:n]); err != nil {
			broken.Store(true)
			break
		}
		out(buf[:n])
	}
	if broken.Load() {
		k.c.Close()
	}
	onEnd()
	<-upDone
	return !broken.Load()
}

type flow struct {
	ch   chan []byte
	last atomic.Int64
}

// udpServer runs one UDP user port: each source address is a flow with its own carrier, ended after udpIdle.
func udpServer(addr string, port int, claim func() *carrier, pool *stack) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		log.Fatal(err)
	}
	ln, err := net.ListenUDP("udp", ua)
	if err != nil {
		log.Fatal(err)
	}
	ln.SetReadBuffer(8 << 20)
	ln.SetWriteBuffer(8 << 20)
	var mu sync.Mutex
	flows := map[netip.AddrPort]*flow{}
	drop := func(a netip.AddrPort, f *flow) {
		mu.Lock()
		if flows[a] == f {
			delete(flows, a)
			close(f.ch)
		}
		mu.Unlock()
	}
	go func() {
		for {
			time.Sleep(5 * time.Second)
			old := time.Now().Add(-udpIdle).UnixNano()
			mu.Lock()
			for a, f := range flows {
				if f.last.Load() < old {
					delete(flows, a)
					close(f.ch)
				}
			}
			mu.Unlock()
		}
	}()
	buf := make([]byte, 1<<16)
	for {
		n, a, err := ln.ReadFromUDPAddrPort(buf)
		if err != nil {
			continue
		}
		now := time.Now().UnixNano()
		p := append([]byte(nil), buf[:n]...)
		mu.Lock()
		f := flows[a]
		if f == nil {
			f = &flow{ch: make(chan []byte, 1024)}
			flows[a] = f
			udpFlows.Add(1)
			go func(a netip.AddrPort, f *flow) {
				k := claim()
				if k == nil {
					failed.Add(1)
					drop(a, f)
					return
				}
				k.wmu.Lock()
				_, err := k.c.Write([]byte{1, byte(port >> 8), byte(port)})
				k.wmu.Unlock()
				if err != nil {
					failed.Add(1)
					k.c.Close()
					drop(a, f)
					for range f.ch {
					}
					return
				}
				served.Add(1)
				out := func(d []byte) {
					f.last.Store(time.Now().UnixNano())
					ln.WriteToUDPAddrPort(d, a)
				}
				if dgram(k, f.ch, out, func() { drop(a, f) }, true) {
					pool.push(k)
				}
			}(a, f)
		}
		f.last.Store(now)
		select {
		case f.ch <- p:
		default:
			udpDrops.Add(1)
		}
		mu.Unlock()
	}
}

// udpClient answers a UDP flow: a connected socket to the target, its replies sent back as datagrams.
func udpClient(k *carrier, dst string) bool {
	c, err := net.Dial("udp", dst)
	if err != nil {
		failed.Add(1)
		return abortStream(k)
	}
	uc := c.(*net.UDPConn)
	uc.SetReadBuffer(8 << 20)
	uc.SetWriteBuffer(8 << 20)
	in := make(chan []byte, 1024)
	var closing atomic.Bool
	go func() {
		defer close(in)
		buf := make([]byte, 1<<16)
		for {
			n, err := uc.Read(buf)
			if err != nil {
				if !closing.Load() && errors.Is(err, syscall.ECONNREFUSED) {
					continue // target not listening (yet): the ICMP error must not end the flow
				}
				return
			}
			in <- append([]byte(nil), buf[:n]...)
		}
	}()
	return dgram(k, in, func(d []byte) { uc.Write(d) }, func() { closing.Store(true); uc.Close() }, false)
}

// ---------------- client (abroad) ----------------

// next waits on an idle carrier (pinging every pingInt) for the server's [proto][port] header, then acks it.
// With -hop, an idle carrier past its lifetime sends 0x02 instead of a ping: the server drops it from its pool and
// closes it, unless it was claimed at that moment, in which case its header still arrives and is served first.
func next(k *carrier) (byte, int, bool) {
	hdr := make([]byte, 3)
	got := 0
	retiring := false
	for got < 3 {
		if !retiring && got == 0 && k.life > 0 && time.Since(k.born) > k.life {
			retiring = true
			hops.Add(1)
			k.wmu.Lock()
			k.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_, err := k.c.Write([]byte{2})
			k.c.SetWriteDeadline(time.Time{})
			k.wmu.Unlock()
			if err != nil {
				return 0, 0, false
			}
			k.c.SetReadDeadline(time.Now().Add(3 * time.Second))
		} else if !retiring {
			k.c.SetReadDeadline(time.Now().Add(pingInt))
		}
		b, err := k.r.ReadByte()
		if err == nil {
			hdr[got] = b
			got++
			continue
		}
		if retiring || !isTimeout(err) {
			return 0, 0, false
		}
		if got == 0 {
			k.wmu.Lock()
			k.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_, err = k.c.Write([]byte{0})
			k.c.SetWriteDeadline(time.Time{})
			k.wmu.Unlock()
			if err != nil {
				return 0, 0, false
			}
		}
	}
	k.c.SetReadDeadline(time.Time{})
	if _, err := k.c.Write([]byte{1}); err != nil {
		return 0, 0, false
	}
	return hdr[0], int(hdr[1])<<8 | int(hdr[2]), true
}

func client(remote, target string, poolSize, par int) {
	var idleN, pending atomic.Int64
	kick := make(chan struct{}, 1)
	poke := func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
	d := net.Dialer{Timeout: 10 * time.Second, Control: sockopts}
	remotes := expand(remote)
	var rr atomic.Uint64

	// carry runs one carrier for its whole life: idle until a stream, relay it, idle again while it stays clean.
	carry := func(k *carrier) {
		defer k.c.Close()
		for {
			proto, port, ok := next(k)
			idleN.Add(-1)
			poke()
			if !ok {
				return
			}
			served.Add(1)
			dst := net.JoinHostPort(target, strconv.Itoa(port))
			if proto == 1 {
				udpFlows.Add(1)
				ok = udpClient(k, dst)
			} else if lc, err := net.DialTimeout("tcp", dst, 5*time.Second); err != nil {
				// the server already relays the user's bytes: abort the stream, drain, keep the carrier
				failed.Add(1)
				ok = abortStream(k)
			} else {
				ok = stream(k, lc.(*net.TCPConn), false)
			}
			if !ok || idleN.Load() >= int64(2*poolSize) {
				return
			}
			reused.Add(1)
			idleN.Add(1)
		}
	}

	dial := func() {
		defer func() { pending.Add(-1); poke() }()
		dials.Add(1)
		nc, err := d.Dial("tcp", remotes[rr.Add(1)%uint64(len(remotes))])
		if err != nil {
			dialErr.Add(1)
			time.Sleep(time.Second)
			return
		}
		k := newCarrier(nc.(*net.TCPConn))
		if hop > 0 { // 0.5-1.5x, so carriers don't all retire together
			k.life = hop/2 + rand.N(hop)
		}
		if _, err := k.c.Write(hello); err != nil {
			k.c.Close()
			return
		}
		idleN.Add(1)
		go carry(k)
	}

	go func() {
		for {
			time.Sleep(30 * time.Second)
			log.Printf("idle %d  served %d  udp flows %d  reused %d  failed %d  dials %d  dial errors %d  hops %d",
				idleN.Load(), served.Load(), udpFlows.Load(), reused.Load(), failed.Load(), dials.Load(), dialErr.Load(), hops.Load())
		}
	}()
	poke()
	for {
		select {
		case <-kick:
		case <-time.After(time.Second):
		}
		for idleN.Load()+pending.Load() < int64(poolSize) && pending.Load() < int64(par) {
			pending.Add(1)
			go dial()
		}
	}
}

// abortStream answers a stream whose target could not be reached: RST, then drain until the peer's FIN/RST.
func abortStream(k *carrier) bool {
	if k.frame(fRst, make([]byte, 4)) != nil {
		return false
	}
	hdr := make([]byte, 4)
	for {
		if _, err := io.ReadFull(k.r, hdr); err != nil {
			return false
		}
		n := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
		if hdr[0] != fData {
			return true
		}
		if _, err := k.r.Discard(n); err != nil {
			return false
		}
	}
}

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 || (os.Args[1] != "server" && os.Args[1] != "client") {
		log.Fatal("usage: ltcar server|client [flags]")
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	listen := fs.String("listen", "0.0.0.0:7000", "server: carrier listen address, or a port range host:a-b")
	users := fs.String("users", "", "server: user listen addresses, comma separated, udp: prefix for UDP (the port is passed to the client)")
	remote := fs.String("remote", "", "client: server address, or a port range host:a-b to spread carriers over")
	fs.DurationVar(&hop, "hop", 0, "client: retire idle carriers after about this long and redial on the next port (0: never)")
	target := fs.String("target", "127.0.0.1", "client: host the user ports are forwarded to")
	pool := fs.Int("pool", 64, "client: idle carriers kept ready")
	par := fs.Int("par", 16, "client: concurrent dials")
	token := fs.String("token", "", "shared secret (default: $LTCAR_TOKEN, which keeps it out of ps)")
	name := fs.String("name", "", "name shown in log lines")
	fs.StringVar(&ccName, "cc", "bbr", "congestion control for carrier sockets (empty: system default)")
	fs.IntVar(&lowat, "lowat", 0, "TCP_NOTSENT_LOWAT bytes on carrier sockets (0: off)")
	fs.IntVar(&sockBuf, "buf", 8<<20, "largest SO_RCVBUF/SO_SNDBUF per carrier (0: kernel autotuning)")
	fs.IntVar(&budget, "budget", 32<<20, "socket buffer budget split over busy carriers")
	fs.DurationVar(&udpIdle, "udp-idle", 60*time.Second, "server: end a UDP flow after this long without traffic")
	fs.Parse(os.Args[2:])
	if *token == "" {
		*token = os.Getenv("LTCAR_TOKEN")
	}
	if *token == "" {
		log.Fatal("-token or LTCAR_TOKEN is required")
	}
	if *name != "" {
		log.SetPrefix(*name + " ")
	}
	sum := sha256.Sum256([]byte(*token))
	hello = append(append([]byte{}, magic...), sum[:]...)
	if os.Args[1] == "server" {
		if *users == "" {
			log.Fatal("-users is required")
		}
		server(*listen, *users)
	} else {
		if *remote == "" {
			log.Fatal("-remote is required")
		}
		client(*remote, *target, *pool, *par)
	}
}
