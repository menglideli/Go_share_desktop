// rtcprobe — 阶段 0 · S0-4
// 验证 pion/webrtc v4：
//   1. 两个 PeerConnection 能否建立 ICE 直连 + DTLS（内网无 STUN/TURN）
//   2. 建连耗时
//   3. DataChannel（unordered + maxRetransmits=0，即"类 UDP"）的单向延迟
//   4. 大帧分片传输的吞吐与丢包（MJPEG 单帧可达数百 KB）
//
// 背景：编码器已改为 Motion JPEG（每帧独立、数百 KB），
// 因此需要确认 DataChannel 是否撑得住，以及是否需要走 RTP 或裸 UDP。
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"time"

	"github.com/pion/webrtc/v4"
)

func u16(v uint16) *uint16 { return &v }

func main() {
	fmt.Printf("\n=== pion/webrtc 传输验证 · S0-4 ===\n")
	fmt.Printf("模式：同进程两 PeerConnection（仍经真实 UDP socket + ICE + DTLS）\n\n")

	t0 := time.Now()

	pc1, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		fmt.Printf("pc1 创建失败: %v\n", err)
		return
	}
	defer pc1.Close()
	pc2, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		fmt.Printf("pc2 创建失败: %v\n", err)
		return
	}
	defer pc2.Close()

	// ---- 接收端：DataChannel 回调 ----
	type recvRec struct {
		seq   uint64
		sentN int64
		at    time.Time
		n     int
	}
	recvd := make(chan recvRec, 4096)
	pc2.OnDataChannel(func(d *webrtc.DataChannel) {
		fmt.Printf("  接收端收到 DataChannel: label=%s\n", d.Label())
		d.OnMessage(func(msg webrtc.DataChannelMessage) {
			if len(msg.Data) < 16 {
				return
			}
			seq := binary.LittleEndian.Uint64(msg.Data[0:8])
			sentN := int64(binary.LittleEndian.Uint64(msg.Data[8:16]))
			recvd <- recvRec{seq: seq, sentN: sentN, at: time.Now(), n: len(msg.Data)}
		})
	})

	// ---- 发送端：创建 DataChannel（不可靠、乱序，最接近 UDP） ----
	dc, err := pc1.CreateDataChannel("frames", &webrtc.DataChannelInit{
		Ordered:        new(bool), // false
		MaxRetransmits: u16(0),
	})
	if err != nil {
		fmt.Printf("CreateDataChannel 失败: %v\n", err)
		return
	}
	dcOpen := make(chan struct{}, 1)
	dc.OnOpen(func() {
		fmt.Printf("  DataChannel 已打开\n")
		dcOpen <- struct{}{}
	})

	// ---- ICE candidate 交换 ----
	pc1.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		_ = pc2.AddICECandidate(c.ToJSON())
	})
	pc2.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		_ = pc1.AddICECandidate(c.ToJSON())
	})

	connected := make(chan struct{}, 1)
	pc1.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		fmt.Printf("  pc1 ICE 状态: %s\n", s)
		if s == webrtc.ICEConnectionStateConnected || s == webrtc.ICEConnectionStateCompleted {
			select {
			case connected <- struct{}{}:
			default:
			}
		}
	})

	// ---- 信令交换 ----
	offer, err := pc1.CreateOffer(nil)
	if err != nil {
		fmt.Printf("CreateOffer 失败: %v\n", err)
		return
	}
	if err := pc1.SetLocalDescription(offer); err != nil {
		fmt.Printf("pc1 SetLocalDescription 失败: %v\n", err)
		return
	}
	if err := pc2.SetRemoteDescription(*pc1.LocalDescription()); err != nil {
		fmt.Printf("pc2 SetRemoteDescription 失败: %v\n", err)
		return
	}
	answer, err := pc2.CreateAnswer(nil)
	if err != nil {
		fmt.Printf("CreateAnswer 失败: %v\n", err)
		return
	}
	if err := pc2.SetLocalDescription(answer); err != nil {
		fmt.Printf("pc2 SetLocalDescription 失败: %v\n", err)
		return
	}
	if err := pc1.SetRemoteDescription(*pc2.LocalDescription()); err != nil {
		fmt.Printf("pc1 SetRemoteDescription 失败: %v\n", err)
		return
	}

	// ---- 等待建连 ----
	select {
	case <-connected:
		fmt.Printf("  ✅ ICE+DTLS 建连成功，耗时 %v\n", time.Since(t0))
	case <-time.After(20 * time.Second):
		fmt.Printf("  ❌ 20s 内未建连\n")
		return
	}

	select {
	case <-dcOpen:
	case <-time.After(10 * time.Second):
		fmt.Printf("  ❌ DataChannel 未打开\n")
		return
	}

	// ---- 延迟测试：64KB 消息 × 100 ----
	fmt.Printf("\n[1] 单条 64KB 消息 × 100（测单向延迟）\n")
	payload := make([]byte, 65536-16)
	for i := range payload {
		payload[i] = byte(i)
	}
	send := func(seq uint64, body []byte) {
		msg := make([]byte, 16+len(body))
		binary.LittleEndian.PutUint64(msg[0:8], seq)
		binary.LittleEndian.PutUint64(msg[8:16], uint64(time.Now().UnixNano()))
		copy(msg[16:], body)
		_ = dc.Send(msg)
	}

	const N = 100
	go func() {
		for i := 0; i < N; i++ {
			send(uint64(i), payload)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	var latSum time.Duration
	got := 0
	deadline := time.After(15 * time.Second)
loop1:
	for got < N {
		select {
		case r := <-recvd:
			lat := r.at.Sub(time.Unix(0, r.sentN))
			latSum += lat
			got++
		case <-deadline:
			break loop1
		}
	}
	if got > 0 {
		fmt.Printf("  收到 %d/%d 条，平均单向延迟 %.2f ms\n", got, N,
			float64(latSum.Microseconds())/float64(got)/1000.0)
	} else {
		fmt.Printf("  ❌ 未收到任何消息\n")
	}

	// ---- 吞吐测试：模拟 1080p MJPEG 帧（680KB）分片发送 ----
	fmt.Printf("\n[2] 模拟 1080p MJPEG 帧 680KB，分 16 片发送 × 30 帧\n")
	frame := make([]byte, 680*1024)
	for i := range frame {
		frame[i] = byte(i * 7)
	}
	const chunks = 16
	chunkSize := (len(frame) + chunks - 1) / chunks

	tStart := time.Now()
	for f := 0; f < 30; f++ {
		for c := 0; c < chunks; c++ {
			lo := c * chunkSize
			hi := lo + chunkSize
			if hi > len(frame) {
				hi = len(frame)
			}
			if lo >= len(frame) {
				break
			}
			send(uint64(f*1000+c), frame[lo:hi])
		}
		time.Sleep(33 * time.Millisecond) // 30fps 节奏
	}
	elapsed := time.Since(tStart)

	// 排空
	got2 := 0
	deadline2 := time.After(8 * time.Second)
loop2:
	for {
		select {
		case <-recvd:
			got2++
		case <-deadline2:
			break loop2
		}
	}
	sent := 30 * chunks
	fmt.Printf("  发送 %d 片 / 收到 %d 片（丢包 %.1f%%）  用时 %v\n",
		sent, got2, 100.0*(1-float64(got2)/float64(sent)), elapsed)
	mbps := float64(got2*chunkSize) * 8 / elapsed.Seconds() / 1e6
	fmt.Printf("  实测吞吐 ≈ %.1f Mbps（30fps 节奏下）\n", mbps)

	fmt.Printf("\n=== S0-4 结束 ===\n\n")
	os.Exit(0)
}
