// Package discover 是局域网自动发现：分享端周期性广播"我在这"，观看端扫一次就能列出
// 可加入的分享，不用手抄 IP。
//
// 设计取舍：
//  1. 只广播"名字 + 地址"，**绝不广播授权码**。发现是广播给整个网段的，
//     把授权码放进去等于把它贴在楼道里；授权码仍然要靠会议里口头/聊天窗口传递。
//  2. 用 UDP 广播而不是组播：组播在许多企业网的交换机上默认不通，
//     且要额外处理 TTL/IGMP；广播到每个子网的广播地址，内网一定可达。
//  3. 每个包自带全部信息（无状态），丢一个包不影响下一次。
package discover

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"sync"
	"time"

	"goshare/internal/netif"
)

// Port 是发现用的固定 UDP 端口（与信令端口解耦，顺延信令端口时不影响发现）。
const Port = 45921

// Version 是协议版本，跨版本不兼容时让对端直接忽略。
const Version = 1

// Announce 是分享端广播的一条公告。
type Announce struct {
	V        int      `json:"v"`
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Addrs    []string `json:"addrs"`
	Viewers  int      `json:"viewers"`
	MaxView  int      `json:"maxViewers"`
	NeedCode bool     `json:"needCode"`
	Width    int      `json:"width"`
	Height   int      `json:"height"`
}

// Entry 是观看端看到的一条可加入项。
type Entry struct {
	Announce
	From string    // 来源 IP
	Seen time.Time // 最后一次收到
}

// ---------------------------------------------------------------------------
// 分享端：广播
// ---------------------------------------------------------------------------

// Announcer 周期性向各子网广播公告。
type Announcer struct {
	mu   sync.Mutex
	a    Announce
	stop chan struct{}
	done chan struct{}
}

// NewAnnouncer 创建并立即开始广播。
func NewAnnouncer(a Announce, every time.Duration) (*Announcer, error) {
	if every <= 0 {
		every = 1500 * time.Millisecond
	}
	if a.V == 0 {
		a.V = Version
	}
	an := &Announcer{a: a, stop: make(chan struct{}), done: make(chan struct{})}
	go an.loop(every)
	return an, nil
}

// Update 修改公告内容（比如观众数变化），下一次广播生效。
func (an *Announcer) Update(f func(*Announce)) {
	an.mu.Lock()
	defer an.mu.Unlock()
	f(&an.a)
}

// Close 停止广播。
func (an *Announcer) Close() {
	close(an.stop)
	<-an.done
}

func (an *Announcer) loop(every time.Duration) {
	defer close(an.done)
	t := time.NewTicker(every)
	defer t.Stop()
	an.send()
	for {
		select {
		case <-an.stop:
			return
		case <-t.C:
			an.send()
		}
	}
}

func (an *Announcer) send() {
	an.mu.Lock()
	msg, err := json.Marshal(an.a)
	an.mu.Unlock()
	if err != nil {
		return
	}
	for _, b := range netif.BroadcastAddrs() {
		// 每次都新建 UDP socket：广播地址数量少（实测 6 个）、频率低（1.5s），
		// 换来的是不用维护 socket 生命周期，也不用处理网卡热插拔后的地址失效。
		c, err := net.DialTimeout("udp", net.JoinHostPort(b, strconv.Itoa(Port)), 200*time.Millisecond)
		if err != nil {
			continue
		}
		_, _ = c.Write(msg)
		_ = c.Close()
	}
}

// ---------------------------------------------------------------------------
// 观看端：扫描
// ---------------------------------------------------------------------------

// Browser 监听广播并收集条目。
type Browser struct {
	conn *net.UDPConn
}

// NewBrowser 绑定发现端口开始监听。
func NewBrowser() (*Browser, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: Port})
	if err != nil {
		return nil, err
	}
	return &Browser{conn: conn}, nil
}

// Close 停止监听。
func (b *Browser) Close() error { return b.conn.Close() }

// Scan 收集 d 时长内的广播，按名称去重（同一分享会周期性重复）。
// ctx 取消时立即返回已收集到的部分。
func (b *Browser) Scan(ctx context.Context, d time.Duration) ([]Entry, error) {
	deadline := time.Now().Add(d)
	out := map[string]Entry{}
	if err := b.conn.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return values(out), ctx.Err()
		default:
		}
		if time.Now().After(deadline) {
			return values(out), nil
		}
		n, addr, err := b.conn.ReadFromUDP(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return values(out), nil
			}
			if ctx.Err() != nil {
				return values(out), ctx.Err()
			}
			return values(out), err
		}
		var a Announce
		if err := json.Unmarshal(buf[:n], &a); err != nil || a.V != Version {
			continue
		}
		key := a.ID
		if key == "" {
			key = addr.IP.String()
		}
		out[key] = Entry{Announce: a, From: addr.IP.String(), Seen: time.Now()}
	}
}

func values(m map[string]Entry) []Entry {
	out := make([]Entry, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
