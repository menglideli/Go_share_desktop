// Package signal 是分享端与观看端之间的「控制面」：HTTP 上的一次握手，
// 换完 SDP 就退场，真正的画面走 P2P，不经过这台 HTTP 服务。
//
// 为什么是 HTTP 而不是自建 TCP 协议：内网里最容易出问题的不是协议效率，
// 而是「同事输错地址/授权码时看到什么」。HTTP 能直接用浏览器、curl 验证，
// 排障成本最低；握手一次只几百字节，性能完全不是瓶颈。
//
// 采用 non-trickle（等候选收集完再一次性交换 SDP）：
// 内网 host candidate 收集通常 <100ms，换来的是"一个请求完成接入"的简单性。
package signal

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"goshare/internal/netif"
)

// 握手失败限次：局域网里最常见的不是攻击，是同事连错机器反复重试。
// 但不限次的话，8^6 的空间被脚本跑一遍也就几分钟 —— 所以按 IP 限次 + 冷却。
const (
	maxFailPerIP    = 5
	failCooldown    = 30 * time.Second
	failWindowReset = 5 * time.Minute // 长时间无失败则清空旧的失败记录
	// 请求体上限：SDP（含全部 ICE 候选）实测几十 KB，2MB 是宽松但有限的兜底。
	maxBody = 2 << 20
)

var (
	// ErrBadCode 授权码错误。
	ErrBadCode = errors.New("signal: 授权码错误")
	// ErrRateLimited 失败次数过多，需冷却。
	ErrRateLimited = errors.New("signal: 尝试过于频繁，请稍后再试")
	// ErrFull 观众数已达上限。
	ErrFull = errors.New("signal: 观看人数已满")
)

// Info 是分享端对外公布的会话信息（未接入前可匿名获取，方便排障）。
type Info struct {
	Name       string `json:"name"`
	Version    int    `json:"version"`
	NeedCode   bool   `json:"needCode"`
	Viewers    int    `json:"viewers"`
	MaxViewers int    `json:"maxViewers"`
	// Width/Height 是发送分辨率，观看端据此准备画布，避免首帧才知尺寸导致抖动。
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Preset  string `json:"preset"`
	Backend string `json:"backend"`
}

// JoinRequest 是观众接入请求。
type JoinRequest struct {
	Code   string                    `json:"code"`
	Viewer string                    `json:"viewer"`
	Offer  webrtc.SessionDescription `json:"offer"`
	// Token 由服务端在调用 Accept 之前分配。分享端在检测到连接断了时，
	// 用它调 Server.Drop 精确回收这一位 —— 按 IP 踢在同机多观众时会误伤。
	Token     string `json:"-"`
	RemoteIP  string `json:"-"`
	UserAgent string `json:"-"`
}

// ServerConfig 是信令服务配置。
type ServerConfig struct {
	Name string
	// Port 起始端口（0 或 9000）。被占用时自动顺延，实际端口看 Server.Port()。
	Port int
	// Code 授权码；空则自动生成，可之后用 Rotate() 轮换。
	Code string
	// MaxViewers 同时观看上限，0 用默认 8。
	MaxViewers int
	// Accept 由分享端实现：为这位观众建 Peer、吃下 offer、返回 answer。
	// 返回的 closeFn 会在观众断开或被踢时调用（用于释放 Peer）。
	Accept func(req JoinRequest) (answer webrtc.SessionDescription, closeFn func(), err error)
	// Describe 可选，用于填充 Info 里的分辨率/档位/采集后端。
	Describe func() (w, h int, preset, backend string)
	Logger   *log.Logger
}

// Server 是分享端侧的 HTTP 信令服务。
type Server struct {
	cfg     ServerConfig
	code    string
	mu      sync.Mutex
	viewers map[string]*viewer
	fails   map[string]*failRec
	srv     *http.Server
	ln      net.Listener
}

type viewer struct {
	Token    string
	Name     string
	RemoteIP string
	Joined   time.Time
	closeFn  func()
}

type failRec struct {
	n        int
	lastFail time.Time
}

// NewServer 创建信令服务（尚未监听）。
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.MaxViewers <= 0 {
		cfg.MaxViewers = 8
	}
	code := cfg.Code
	if code == "" {
		code = NewCode()
		if code == "" {
			return nil, errors.New("signal: 生成授权码失败（系统熵源不可用）")
		}
	}
	return &Server{
		cfg:     cfg,
		code:    code,
		viewers: map[string]*viewer{},
		fails:   map[string]*failRec{},
	}, nil
}

// SetAccept 设置（或替换）接入处理器。可在 Start 之后调用：
// 分享端往往是"先起信令把地址显示给用户，采集就绪后再能接客"。
func (s *Server) SetAccept(f func(req JoinRequest) (answer webrtc.SessionDescription, closeFn func(), err error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Accept = f
}

// Start 监听端口。占用时从起始端口顺延，最多试 20 个。
//
// ⚠️ 端口冲突是真实会发生的：上次的进程没退干净、或者另一个软件占了 9000。
// 顺延后必须让界面显示"实际端口"，否则用户照着 9000 告诉同事就永远连不上。
func (s *Server) Start() error {
	base := s.cfg.Port
	if base <= 0 {
		base = 9000
	}
	var lastErr error
	for i := 0; i < 20; i++ {
		p := base + i
		ln, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(p)))
		if err != nil {
			lastErr = err
			continue
		}
		s.ln = ln
		s.srv = &http.Server{Handler: s.mux(), ReadHeaderTimeout: 10 * time.Second}
		s.cfg.Port = p
		go func() { _ = s.srv.Serve(ln) }()
		if s.cfg.Logger != nil && i > 0 {
			s.cfg.Logger.Printf("信令端口 %d 被占用，已顺延到 %d", base, p)
		}
		return nil
	}
	return lastErr
}

// Port 返回实际监听端口（Start 之后才有意义）。
func (s *Server) Port() int { return s.cfg.Port }

// Code 返回当前授权码。
func (s *Server) Code() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code
}

// Rotate 轮换授权码。已接入的观众不受影响（他们握过手了）。
func (s *Server) Rotate() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := NewCode()
	if c != "" {
		s.code = c
	}
	return s.code
}

// Close 关闭服务并断开所有观众。
func (s *Server) Close() error {
	if s.srv == nil {
		return nil
	}
	s.mu.Lock()
	for _, v := range s.viewers {
		if v.closeFn != nil {
			v.closeFn()
		}
	}
	s.viewers = map[string]*viewer{}
	s.mu.Unlock()
	return s.srv.Close()
}

// ShareAddrs 返回可直接复制给同事的 "IP:端口" 列表（已按网卡优先级排序）。
func (s *Server) ShareAddrs() []string {
	ifs := netif.Shareable()
	out := make([]string, 0, len(ifs))
	for _, i := range ifs {
		out = append(out, net.JoinHostPort(i.IP.String(), strconv.Itoa(s.cfg.Port)))
	}
	return out
}

// AddrLine 返回"地址 · 授权码"的一行文本，一键复制用。
func (s *Server) AddrLine() string {
	addrs := s.ShareAddrs()
	if len(addrs) == 0 {
		return ""
	}
	return addrs[0] + " · " + s.Code()
}

// Viewers 返回当前观众列表快照。
func (s *Server) Viewers() []ViewerInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ViewerInfo, 0, len(s.viewers))
	for _, v := range s.viewers {
		out = append(out, ViewerInfo{Name: v.Name, IP: v.RemoteIP, Joined: v.Joined})
	}
	return out
}

// ViewerInfo 是界面上展示的观众条目（阶段 4 踢人按钮要用）。
type ViewerInfo struct {
	Name   string
	IP     string
	Joined time.Time
}

// Drop 按 token 精确移除一位观众并释放其 Peer。
//
// 用于"观众那边根本没打招呼就走了"的场景：进程被任务管理器杀掉、网线拔掉、
// 笔记本合盖 —— 这些都收不到 Bye。分享端靠 ICE 状态回调发现问题后调这里，
// 否则幽灵观众会一直占着人数名额（实测：第二次接入显示"当前 2 人"，实际只有 1 人）。
// take 摘掉观众记录并返回它，不执行 closeFn。
func (s *Server) take(token string) (*viewer, bool) {
	s.mu.Lock()
	v, ok := s.viewers[token]
	if ok {
		delete(s.viewers, token)
	}
	s.mu.Unlock()
	return v, ok
}

// Drop 移除观众并执行它的清理回调。
//
// 注意调用链不能回环：cleanup → Drop → closeFn → cleanup 会死锁（sync.Once
// 不允许同一 goroutine 重入）。分享端请在自己的 cleanup 里用 Detach。
func (s *Server) Drop(token string) {
	v, ok := s.take(token)
	if !ok {
		return
	}
	if v.closeFn != nil {
		v.closeFn()
	}
	if s.cfg.Logger != nil {
		s.cfg.Logger.Printf("回收观众 %s（%s）", v.Name, v.RemoteIP)
	}
}

// Detach 只摘掉观众记录、不触发回调。
//
// 用于「清理动作本来就由调用方发起」的场景 —— 分享端的 cleanup 已经要关 Peer 了，
// 再让服务端回调它自己就是自锁。见 R27。
func (s *Server) Detach(token string) {
	v, ok := s.take(token)
	if ok && s.cfg.Logger != nil {
		s.cfg.Logger.Printf("摘除观众记录 %s（%s）", v.Name, v.RemoteIP)
	}
}

// Kick 踢掉一位观众（按 IP 匹配，界面层够用）。
func (s *Server) Kick(ip string) int {
	s.mu.Lock()
	var doomed []*viewer
	for tok, v := range s.viewers {
		if v.RemoteIP == ip {
			doomed = append(doomed, v)
			delete(s.viewers, tok)
		}
	}
	s.mu.Unlock()
	// 摘完必须解锁再回调：closeFn 极可能反向调用 srv 的方法（分享端的 cleanup
	// 就会这么做），持锁回调等于自己锁自己。
	for _, v := range doomed {
		if v.closeFn != nil {
			v.closeFn()
		}
	}
	return len(doomed)
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

func (s *Server) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("/api/info", s.handleInfo)
	m.HandleFunc("/api/join", s.handleJoin)
	m.HandleFunc("/api/bye", s.handleBye)
	return m
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	info := Info{
		Name:       s.cfg.Name,
		Version:    1,
		NeedCode:   true,
		Viewers:    len(s.viewers),
		MaxViewers: s.cfg.MaxViewers,
	}
	if s.cfg.Describe != nil {
		info.Width, info.Height, info.Preset, info.Backend = s.cfg.Describe()
	}
	writeJSON(w, http.StatusOK, info)
}

type joinReqWire struct {
	Code  string                    `json:"code"`
	Name  string                    `json:"name"`
	Offer webrtc.SessionDescription `json:"offer"`
}

type joinRespWire struct {
	Token  string                    `json:"token"`
	Answer webrtc.SessionDescription `json:"answer"`
	Name   string                    `json:"hostName"`
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	ip := clientIP(r)
	s.mu.Lock()
	rec := s.fails[ip]
	tooMany := rec != nil && rec.n >= maxFailPerIP && time.Since(rec.lastFail) < failCooldown
	s.mu.Unlock()
	if tooMany {
		writeErr(w, http.StatusTooManyRequests, ErrRateLimited.Error())
		return
	}

	var jr joinReqWire
	if err := readJSON(w, r, &jr); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if !CodeEqual(jr.Code, s.Code()) {
		s.noteFail(ip)
		writeErr(w, http.StatusUnauthorized, ErrBadCode.Error())
		return
	}
	s.mu.Lock()
	full := len(s.viewers) >= s.cfg.MaxViewers
	accept := s.cfg.Accept
	s.mu.Unlock()
	if full {
		writeErr(w, http.StatusServiceUnavailable, ErrFull.Error())
		return
	}
	if accept == nil {
		writeErr(w, http.StatusNotImplemented, "分享端未就绪")
		return
	}

	tok := newToken()
	// 顺序很讲究：先登记，再建 Peer。
	// 因为 Accept 内部有可能**立刻**发现连接已经断了并回调 cleanup → Drop(tok)；
	// 若那时条目还没进表，Drop 会扑空，Accept 返回后又把它登记进去 ——
	// 结果就是一条永远回收不掉的幽灵观众，白占一个人数名额。
	s.mu.Lock()
	s.viewers[tok] = &viewer{Token: tok, Name: jr.Name, RemoteIP: ip, Joined: time.Now()}
	s.mu.Unlock()

	answer, closeFn, err := accept(JoinRequest{
		Code:      jr.Code,
		Viewer:    jr.Name,
		Offer:     jr.Offer,
		Token:     tok,
		RemoteIP:  ip,
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		s.mu.Lock()
		delete(s.viewers, tok)
		s.mu.Unlock()
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.mu.Lock()
	v, ok := s.viewers[tok]
	if !ok {
		// 握手期间就被 Drop 了：别复活它，直接释放刚建好的 Peer。
		s.mu.Unlock()
		if closeFn != nil {
			closeFn()
		}
		writeJSON(w, http.StatusOK, joinRespWire{Token: tok, Answer: answer, Name: s.cfg.Name})
		return
	}
	v.closeFn = closeFn
	delete(s.fails, ip) // 成功后清空失败记录
	total := len(s.viewers)
	s.mu.Unlock()
	if s.cfg.Logger != nil {
		s.cfg.Logger.Printf("观众接入 %s（%s），当前 %d 人", jr.Name, ip, total)
	}
	writeJSON(w, http.StatusOK, joinRespWire{Token: tok, Answer: answer, Name: s.cfg.Name})
}

func (s *Server) handleBye(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	s.mu.Lock()
	v, ok := s.viewers[req.Token]
	if ok {
		if v.closeFn != nil {
			v.closeFn()
		}
		delete(s.viewers, req.Token)
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) noteFail(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.fails[ip]
	now := time.Now()
	if rec == nil || now.Sub(rec.lastFail) > failWindowReset {
		rec = &failRec{}
		s.fails[ip] = rec
	}
	rec.n++
	rec.lastFail = now
	if len(s.fails) > 256 { // 防止被扫端口时无限增长
		for k, v := range s.fails {
			if now.Sub(v.lastFail) > failWindowReset {
				delete(s.fails, k)
			}
		}
	}
}

// clientIP 取客户端 IP。内网直连没有反向代理，直接用 RemoteAddr 就够；
// 但仍要剥掉端口，否则限次会按端口分桶而失效。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	return dec.Decode(v)
}

// newToken 生成会话 token（仅用于 bye / 踢人时定位观众，不需要不可猜测，但也不该可枚举）。
func newToken() string {
	c := NewCode()
	if c == "" {
		return ""
	}
	return c + "-" + strconv.Itoa(int(time.Now().UnixNano()%1000000))
}
