package relay

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Meter 给每条链路开一个本机端口，把中转发往住宅IP的 TCP 连接原样转发过去，
// 同时精确计数上下行字节。中转的「落地」节点连的是这个端口，而不是住宅IP本身。
type Meter struct {
	mu sync.Mutex
	fw map[int64]*forward
}

type forward struct {
	ln     net.Listener
	target atomic.Value // string：住宅IP的 host:port
	up     atomic.Int64 // 手机 → 住宅IP
	down   atomic.Int64 // 住宅IP → 手机
	active atomic.Int32
	last   atomic.Int64 // 最近一次有数据的时间（UnixNano）
}

// Counter 是某条链路的累计流量和当前连接数。
type Counter struct {
	Up, Down int64
	Active   int
	Last     time.Time
}

// Ensure 保证这条链路有转发端口并指向 target，返回端口号。
func (m *Meter) Ensure(chain int64, target string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fw == nil {
		m.fw = map[int64]*forward{}
	}
	if f, ok := m.fw[chain]; ok {
		f.target.Store(target)
		return f.ln.Addr().(*net.TCPAddr).Port, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	f := &forward{ln: ln}
	f.target.Store(target)
	m.fw[chain] = f
	go f.serve()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// Retain 关掉不在 keep 里的转发（链路被删了）。
func (m *Meter) Retain(keep map[int64]bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, f := range m.fw {
		if !keep[id] {
			f.ln.Close()
			delete(m.fw, id)
		}
	}
}

// Counters 返回各链路的累计流量（面板进程启动以来）。
func (m *Meter) Counters() map[int64]Counter {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[int64]Counter, len(m.fw))
	for id, f := range m.fw {
		c := Counter{Up: f.up.Load(), Down: f.down.Load(), Active: int(f.active.Load())}
		if n := f.last.Load(); n > 0 {
			c.Last = time.Unix(0, n)
		}
		out[id] = c
	}
	return out
}

func (f *forward) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(c)
	}
}

func (f *forward) handle(client net.Conn) {
	defer client.Close()
	upstream, err := net.DialTimeout("tcp", f.target.Load().(string), 15*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	f.active.Add(1)
	defer f.active.Add(-1)
	f.last.Store(time.Now().UnixNano())

	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn, n *atomic.Int64) {
		buf := make([]byte, 32*1024)
		for {
			k, err := src.Read(buf)
			if k > 0 {
				n.Add(int64(k))
				f.last.Store(time.Now().UnixNano())
				if _, werr := dst.Write(buf[:k]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(upstream, client, &f.up)
	go pipe(client, upstream, &f.down)
	<-done
	<-done
}
