package signal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
)

// Client 是观看端侧的信令客户端。
type Client struct {
	// Addr 是用户填写的 "IP:端口"（不含协议）。
	Addr string
	base string
	hc   *http.Client
}

// NewClient 创建客户端。addr 可以是 "192.168.1.5:9000"，
// 也容忍用户粘贴的 "http://192.168.1.5:9000" —— 从聊天窗口复制过来的多半带协议头。
func NewClient(addr string) *Client {
	a := strings.TrimSpace(addr)
	a = strings.TrimPrefix(a, "http://")
	a = strings.TrimPrefix(a, "https://")
	a = strings.TrimSuffix(a, "/")
	return &Client{Addr: a, base: "http://" + a, hc: &http.Client{Timeout: 15 * time.Second}}
}

// SetTimeout 调整 HTTP 超时（接入握手在内网通常 <1s，跨地域时可放宽）。
func (c *Client) SetTimeout(d time.Duration) { c.hc.Timeout = d }

// Info 拉取分享端会话信息（不需要授权码）。
func (c *Client) Info(ctx context.Context) (Info, error) {
	var out Info
	err := c.do(ctx, http.MethodGet, "/api/info", nil, &out)
	return out, err
}

// Join 提交 offer 并拿回 answer。成功表示握手完成，媒体面由调用方自行等待就绪。
func (c *Client) Join(ctx context.Context, code, name string, offer webrtc.SessionDescription) (token string, answer webrtc.SessionDescription, hostName string, err error) {
	var resp joinRespWire
	err = c.do(ctx, http.MethodPost, "/api/join", joinReqWire{Code: code, Name: name, Offer: offer}, &resp)
	if err != nil {
		return "", webrtc.SessionDescription{}, "", err
	}
	return resp.Token, resp.Answer, resp.Name, nil
}

// Bye 主动告知分享端"我走了"，让对方立刻回收 Peer 而不是等 ICE 超时。
func (c *Client) Bye(ctx context.Context, token string) error {
	var out map[string]bool
	return c.do(ctx, http.MethodPost, "/api/bye", map[string]string{"token": token}, &out)
}

// do 发一个 JSON 请求。错误会被翻译成 ErrBadCode / ErrRateLimited / ErrFull 这类
// 可直接显示在界面上的语义错误 —— 否则用户只能看到 "401 Unauthorized"。
func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Accept", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("连接 %s 失败：%w", c.Addr, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return translateStatus(resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("解析服务端响应失败：%w", err)
	}
	return nil
}

// translateStatus 把 HTTP 状态码翻成人话。
func translateStatus(code int, raw []byte) error {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	msg := strings.TrimSpace(e.Error)

	var target error
	switch {
	case strings.Contains(msg, ErrBadCode.Error()):
		target = ErrBadCode
	case strings.Contains(msg, ErrRateLimited.Error()):
		target = ErrRateLimited
	case strings.Contains(msg, ErrFull.Error()):
		target = ErrFull
	}
	if target != nil {
		return fmt.Errorf("%w", target)
	}
	if msg != "" {
		return errors.New(msg)
	}
	switch code {
	case http.StatusUnauthorized:
		return ErrBadCode
	case http.StatusTooManyRequests:
		return ErrRateLimited
	case http.StatusServiceUnavailable:
		return ErrFull
	}
	return fmt.Errorf("服务端返回 %d", code)
}
