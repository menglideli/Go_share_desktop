// Package netif 枚举本机的 IPv4 网卡，回答一个具体问题：
// 「分享端应该把哪个地址抄给同事？」
//
// 实测（2026-09-23，本机）：12 个 IPv4 —— 以太网 1 个（默认路由出口）、
// ZeroTier ×2、VMware ×2、Hyper-V ×1、4 个 169.254 链路本地。
// 把 169.254 或 ZeroTier 地址排在前面，同事复制过去就是连不上的 —— 所以过滤与排序
// 不是"锦上添花"，是这个功能能不能用的前提。
package netif

import (
	"net"
	"sort"
	"strings"
)

// Kind 是网卡类型，决定排序与展示分组。
type Kind int

const (
	// KindPhysical 物理网卡：真实局域网，同事大概率在同一个网段。
	KindPhysical Kind = iota
	// KindVPN 异地组网（ZeroTier / Tailscale 等）：跨地域可达，但同事未必装了同一个客户端。
	KindVPN
	// KindVirtual 虚拟机 / 容器虚拟网卡：通常只有本机可达，不该推荐给别人。
	KindVirtual
)

func (k Kind) String() string {
	switch k {
	case KindPhysical:
		return "物理网卡"
	case KindVPN:
		return "异地组网"
	case KindVirtual:
		return "虚拟网卡"
	}
	return "未知"
}

// IF 是一个可用于分享的 IPv4 地址（不是"网卡"：一块网卡可能有多个地址）。
type IF struct {
	Name      string
	IP        net.IP
	Mask      net.IPMask
	Broadcast net.IP
	Kind      Kind
	// Default 表示这是本机默认路由的出接口，最可能是"真正上网的那张卡"。
	Default bool
}

// Note 是给界面用的一句话说明。
func (i IF) Note() string {
	switch {
	case i.Default && i.Kind == KindVPN:
		return "默认路由 · 异地可达"
	case i.Default:
		return "默认路由 · 优先复制这个"
	case i.Kind == KindVPN:
		return "异地可达（对方需装同一客户端）"
	case i.Kind == KindVirtual:
		return "虚拟机/容器专用，通常只有本机可达"
	}
	return "局域网"
}

// String 返回可直接展示的一行。
func (i IF) String() string {
	return i.IP.String() + " · " + i.Name + " · " + i.Kind.String()
}

// vpnKeys / virtualKeys 按网卡名关键词归类。Windows 上 net.Interface.Name 取的是
// 适配器名（"以太网"、"ZeroTier One [xxxx]"、"VMware Network Adapter VMnet8"），
// 用关键词匹配足够，读注册表或 MIB_IFROW 反而更脆。
var (
	vpnKeys     = []string{"zerotier", "tailscale", "wireguard", "openvpn", "hamachi", "radmin", "tap-windows", "tun"}
	virtualKeys = []string{"vmware", "virtualbox", "hyper-v", "vethernet", "virtual", "npcap", "loopback", "teredo", "isatap", "docker", "wsl"}
)

func classify(name string) Kind {
	low := strings.ToLower(name)
	for _, k := range vpnKeys {
		if strings.Contains(low, k) {
			return KindVPN
		}
	}
	for _, k := range virtualKeys {
		if strings.Contains(low, k) {
			return KindVirtual
		}
	}
	return KindPhysical
}

// Interfaces 返回所有可用于分享的 IPv4 地址，已排序（最该推荐的排最前）。
//
// 过滤掉的：回环、169.254 链路本地、未启用的接口、非 IPv4。
// 这些地址发给同事 100% 连不上，留在列表里只会让人复制错。
func Interfaces() ([]IF, error) {
	def := DefaultRouteIP()
	all, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []IF
	for _, ni := range all {
		if ni.Flags&net.FlagUp == 0 || ni.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ni.Addrs()
		if err != nil {
			continue
		}
		k := classify(ni.Name)
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, IF{
				Name:      ni.Name,
				IP:        append(net.IP(nil), ip...),
				Mask:      ipnet.Mask,
				Broadcast: broadcast(ip, ipnet.Mask),
				Kind:      k,
				Default:   def != nil && def.Equal(ip),
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return rank(out[i]) > rank(out[j])
	})
	return out, nil
}

// rank 越大越靠前：默认路由 > 物理 > 异地组网 > 虚拟；同类按地址稳定排序。
func rank(i IF) int {
	r := 0
	if i.Default {
		r += 100
	}
	switch i.Kind {
	case KindPhysical:
		r += 30
	case KindVPN:
		r += 20
	case KindVirtual:
		r += 10
	}
	if i.IP != nil && len(i.IP) == 4 {
		r -= int(i.IP[3]) % 10 // 打破平局的稳定扰动，别整成随机
	}
	return r
}

// broadcast 计算子网广播地址（局域网发现要用），掩码异常时返回 nil。
func broadcast(ip net.IP, mask net.IPMask) net.IP {
	ones, bits := mask.Size()
	if bits != 32 || ones <= 0 || ones >= 31 {
		return nil
	}
	b := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		b[i] = ip[i] | ^mask[i]
	}
	return b
}

// DefaultRouteIP 通过一次 UDP "连接"拿到默认路由的出接口地址。
//
// UDP 的 Dial 不发任何数据包，只查路由表 —— 所以即便目标不可达也能拿到本地地址，
// 这正是我们要的（内网环境未必真有外网）。拿不到就返回 nil，调用方按无默认路由处理。
func DefaultRouteIP() net.IP {
	conn, err := net.Dial("udp", "223.5.5.5:80")
	if err != nil {
		return nil
	}
	defer conn.Close()
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

// Shareable 是 Interfaces 的容错版本：出错时返回空列表而不是让调用方崩。
// 网卡枚举失败不该阻止用户分享（他还可以手动填地址）。
func Shareable() []IF {
	out, err := Interfaces()
	if err != nil {
		return nil
	}
	return out
}

// BroadcastAddrs 返回所有可发广播的地址（局域网发现用），已去重。
func BroadcastAddrs() []string {
	var out []string
	seen := map[string]bool{}
	for _, i := range Shareable() {
		if i.Broadcast == nil {
			continue
		}
		s := i.Broadcast.String()
		if s == "255.255.255.255" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		// 拿不到掩码（比如某些点对点网卡）时退回全局广播，至少能覆盖默认接口
		out = append(out, "255.255.255.255")
	}
	return out
}
