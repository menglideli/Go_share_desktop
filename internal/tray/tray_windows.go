//go:build windows

// Package tray 提供 Windows 系统托盘（通知区域图标），纯 syscall 实现，
// 不引入任何第三方依赖（CGO_ENABLED=0 友好，与 internal/clip 同风格）。
//
// 结构：一个 message-only 隐藏窗口（HWND_MESSAGE 父窗口）跑在独立的、
// 锁定 OS 线程的 goroutine 里处理托盘回调与菜单消息；托盘图标本身用
// CreateIconIndirect 程序化绘制（蓝底圆 + 白色屏幕），不依赖 exe 内嵌资源，
// 因此 `go run` 开发态与发布构建表现一致。
package tray

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Config 是托盘的初始配置。OnShow / OnQuit 必填，OnStop 可空
// （为空时菜单里"停止分享"始终灰着）。
//
// 所有回调都从托盘消息循环线程里用 **go** 起——回调里允许做
// ShowWindow / 业务停止这类阻塞动作，不会卡住托盘自己的消息循环。
type Config struct {
	Tooltip string
	OnShow  func() // 双击图标 或 菜单「显示主窗口」
	OnStop  func() // 菜单「停止分享」（仅分享中可用）
	OnQuit  func() // 菜单「退出」
}

const (
	nimAdd    = 0x0
	nimModify = 0x1
	nimDelete = 0x2

	nifMessage = 0x1
	nifIcon    = 0x2
	nifTip     = 0x4
	nifInfo    = 0x10

	niifInfo = 0x1

	wmDestroy       = 0x0002
	wmCommand       = 0x0111
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205
	wmNull          = 0x0000
	wmApp           = 0x8000

	msgTrayCallback = wmApp + 201 // Shell_NotifyIcon 的回调消息
	msgTrayDestroy  = wmApp + 202 // 自定义：让消息循环线程销毁窗口

	mfString    = 0x0
	mfGrayed    = 0x1
	mfSeparator = 0x800

	tpmRightButton = 0x2
	tpmBottomAlign = 0x20

	hwndMessage = ^uintptr(2) // (HWND)-3，message-only 窗口的父窗口

	idiApplication = 32512

	idmShow = 1001
	idmStop = 1002
	idmQuit = 1003
)

var (
	shell32            = windows.NewLazySystemDLL("shell32.dll")
	procNotifyIcon     = shell32.NewProc("Shell_NotifyIconW")
	user32t            = windows.NewLazySystemDLL("user32.dll")
	procRegisterClass  = user32t.NewProc("RegisterClassExW")
	procCreateWindowEx = user32t.NewProc("CreateWindowExW")
	procDefWindowProc  = user32t.NewProc("DefWindowProcW")
	procGetMessage     = user32t.NewProc("GetMessageW")
	procTranslateMsg   = user32t.NewProc("TranslateMessage")
	procDispatchMsg    = user32t.NewProc("DispatchMessageW")
	procPostQuit       = user32t.NewProc("PostQuitMessage")
	procPostMessage    = user32t.NewProc("PostMessageW")
	procDestroyWindow  = user32t.NewProc("DestroyWindow")
	procCreatePopup    = user32t.NewProc("CreatePopupMenu")
	procAppendMenu     = user32t.NewProc("AppendMenuW")
	procTrackPopup     = user32t.NewProc("TrackPopupMenu")
	procDestroyMenu    = user32t.NewProc("DestroyMenu")
	procSetMenuDefault = user32t.NewProc("SetMenuDefaultItem")
	procSetForeground  = user32t.NewProc("SetForegroundWindow")
	procGetCursorPos   = user32t.NewProc("GetCursorPos")
	procLoadIcon       = user32t.NewProc("LoadIconW")
	procCreateIconInd  = user32t.NewProc("CreateIconIndirect")
	procDestroyIcon    = user32t.NewProc("DestroyIcon")
	// ⚠️ 位图 API 在 gdi32，不在 user32 —— LazyProc 找错 DLL 会
	// mustFind panic 直接崩进程（traytest 第一轮就是这么挂的）。
	gdi32t             = windows.NewLazySystemDLL("gdi32.dll")
	procCreateDIB      = gdi32t.NewProc("CreateDIBSection")
	procCreateBitmap   = gdi32t.NewProc("CreateBitmap")
	procDeleteObject   = gdi32t.NewProc("DeleteObject")
	kernel32t          = windows.NewLazySystemDLL("kernel32.dll")
	procGetModule      = kernel32t.NewProc("GetModuleHandleW")
)

// notifyIconDataW 镜像 NOTIFYICONDATAW（x64）。cbSize 用 unsafe.Sizeof 填，
// 对应 Vista 版结构 —— 不用更新的版本，系统按 cbSize 识别，行为最兼容。
type notifyIconDataW struct {
	cbSize            uint32
	hWnd              windows.HWND
	uID               uint32
	uFlags            uint32
	uCallbackMessage  uint32
	hIcon             windows.Handle
	szTip             [128]uint16
	dwState           uint32
	dwStateMask       uint32
	szInfo            [256]uint16
	uTimeoutOrVersion uint32
	szInfoTitle       [64]uint16
	dwInfoFlags       uint32
	guidItem          windows.GUID
	hBalloonIcon      windows.Handle
}

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     windows.Handle
	hIcon         windows.Handle
	hCursor       windows.Handle
	hbrBackground windows.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       windows.Handle
}

type msgStruct struct {
	hwnd    windows.HWND
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ X, Y int32 }
}

type pointStruct struct{ X, Y int32 }

type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

type iconInfo struct {
	fIcon    uint32
	xHotspot uint32
	yHotspot uint32
	hbmMask  windows.Handle
	hbmColor windows.Handle
}

// Tray 是一个托盘图标实例。一个进程只应有一个。
type Tray struct {
	hwnd   windows.HWND // message-only 窗口（在 loop 线程创建）
	icon   windows.Handle
	ready  chan error
	closed chan struct{}

	sharing atomic.Bool // 控制菜单「停止分享」的可用状态
	once    sync.Once
}

// theTray / theCfg 供 WndProc 回调访问（单例，一个进程一个托盘）。
var (
	theTray *Tray
	theCfg  Config
)

// New 创建托盘图标。成功返回后图标已出现在通知区域；
// 消息循环在内部 goroutine（已 LockOSThread）里运行。
func New(cfg Config) (*Tray, error) {
	if cfg.OnShow == nil || cfg.OnQuit == nil {
		return nil, fmt.Errorf("tray: OnShow 与 OnQuit 回调必填")
	}
	if cfg.Tooltip == "" {
		cfg.Tooltip = "GoShare"
	}
	t := &Tray{ready: make(chan error, 1), closed: make(chan struct{})}
	theTray, theCfg = t, cfg
	go t.loop(cfg.Tooltip)
	if err := <-t.ready; err != nil {
		return nil, err
	}
	return t, nil
}

// SetSharing 切换「停止分享」菜单项的可用状态，并顺带更新气泡提示语。
func (t *Tray) SetSharing(sharing bool, tooltip string) {
	t.sharing.Store(sharing)
	if tooltip != "" {
		t.SetTooltip(tooltip)
	}
}

// SetTooltip 更新鼠标悬停提示。可从任意 goroutine 调用。
func (t *Tray) SetTooltip(text string) {
	d := t.baseData(nifTip)
	copyUTF16(d.szTip[:], text)
	procNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&d)))
}

// Balloon 弹一个气泡通知（NIF_INFO）。失败静默——气泡在很多系统上
// 会被转成普通通知或直接禁用，不值得当错误上报。
func (t *Tray) Balloon(title, text string) {
	d := t.baseData(nifInfo)
	copyUTF16(d.szInfo[:], text)
	copyUTF16(d.szInfoTitle[:], title)
	d.dwInfoFlags = niifInfo
	d.uTimeoutOrVersion = 5000 // ms（Win10 起实际由系统接管，给个保守值）
	procNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&d)))
}

// Close 移除托盘图标并销毁消息循环。可多次调用（只生效一次）。
func (t *Tray) Close() {
	t.once.Do(func() {
		// 不能从非创建线程直接 DestroyWindow（虽然很多版本上"恰好能用"），
		// 发消息让 loop 线程自己销毁，路径确定。
		procPostMessage.Call(uintptr(t.hwnd), msgTrayDestroy, 0, 0)
		<-t.closed
	})
}

// ShowMenuForTest 以编程方式弹出右键菜单（模拟一次右键点击）。
//
// 仅用于自动化验证（cmd/traytest）：菜单是模态的，弹出后托盘消息循环
// 阻塞在 TrackPopupMenu 里，这段时间正好可以截屏取证。
// 菜单最终要人工点掉或杀掉进程 —— 不要在产品代码里调它。
func (t *Tray) ShowMenuForTest() {
	procPostMessage.Call(uintptr(t.hwnd), msgTrayCallback, 1, wmRButtonUp)
}

// baseData 构造一份带窗口句柄与回调消息的 NOTIFYICONDATAW。
func (t *Tray) baseData(flags uint32) notifyIconDataW {
	return notifyIconDataW{
		cbSize:           uint32(unsafe.Sizeof(notifyIconDataW{})),
		hWnd:             t.hwnd,
		uID:              1,
		uFlags:           flags,
		uCallbackMessage: msgTrayCallback,
	}
}

// loop 是托盘线程主函数：建窗口、加图标、跑消息循环。
func (t *Tray) loop(tooltip string) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	className, _ := syscall.UTF16PtrFromString("GoShareTrayMsgWnd")
	hInst, _, _ := procGetModule.Call(0)
	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   syscall.NewCallback(trayWndProc),
		hInstance:     windows.Handle(hInst),
		lpszClassName: className,
	}
	if r, _, err := procRegisterClass.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		t.ready <- fmt.Errorf("tray: 注册窗口类失败：%w", err)
		return
	}
	hwnd, _, err := procCreateWindowEx.Call(
		0, uintptr(unsafe.Pointer(className)), 0, 0,
		0, 0, 0, 0,
		hwndMessage, 0, hInst, 0)
	if hwnd == 0 {
		t.ready <- fmt.Errorf("tray: 创建消息窗口失败：%w", err)
		return
	}
	t.hwnd = windows.HWND(hwnd)

	icon := makeShareIcon(32)
	if icon == 0 {
		// 程序化图标失败时回退系统应用图标——不至于让整个托盘起不来。
		def, _, _ := procLoadIcon.Call(0, idiApplication)
		icon = windows.Handle(def)
	}
	t.icon = icon

	d := t.baseData(nifMessage | nifIcon | nifTip)
	d.hIcon = icon
	copyUTF16(d.szTip[:], tooltip)
	if r, _, err := procNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&d))); r == 0 {
		procDestroyWindow.Call(hwnd)
		t.ready <- fmt.Errorf("tray: Shell_NotifyIcon(NIM_ADD) 失败：%w", err)
		return
	}
	t.ready <- nil

	var m msgStruct
	for {
		r, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 || int32(r) == -1 { // WM_QUIT / 错误
			break
		}
		procTranslateMsg.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMsg.Call(uintptr(unsafe.Pointer(&m)))
	}

	// 消息循环退出后的清理：先摘图标（不然会留下"幽灵图标"，
	// 要鼠标划过去才被系统清掉），再毁图标句柄。
	dd := t.baseData(0)
	procNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&dd)))
	if t.icon != 0 {
		procDestroyIcon.Call(uintptr(t.icon))
	}
	close(t.closed)
}

// trayWndProc 是 message-only 窗口的窗口过程。
//
// ⚠️ 业务回调一律 go 出去跑：OnStop/OnQuit 里会停管线、关窗口，
// 在窗口过程里同步做等于在托盘消息循环里阻塞自己。
func trayWndProc(hwnd windows.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case msgTrayCallback:
		switch uint32(lParam) {
		case wmLButtonDblClk:
			go theCfg.OnShow()
		case wmRButtonUp:
			showMenu(hwnd)
		}
		return 0
	case wmCommand:
		switch uint32(wParam) & 0xFFFF {
		case idmShow:
			go theCfg.OnShow()
		case idmStop:
			if theCfg.OnStop != nil {
				go theCfg.OnStop()
			}
		case idmQuit:
			go theCfg.OnQuit()
		}
		return 0
	case msgTrayDestroy:
		procDestroyWindow.Call(uintptr(hwnd))
		return 0
	case wmDestroy:
		procPostQuit.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProc.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
	return r
}

// showMenu 在鼠标位置弹出右键菜单（每次现建现销，状态来自原子量，
// 避免"常驻菜单 + 跨线程 EnableMenuItem"的同步问题）。
func showMenu(hwnd windows.HWND) {
	menu, _, _ := procCreatePopup.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	appendItem(menu, idmShow, "显示主窗口", false)
	appendItem(menu, idmStop, "停止分享", !theTray.sharing.Load() || theCfg.OnStop == nil)
	appendSep(menu)
	appendItem(menu, idmQuit, "退出", false)
	// 双击默认项 = 「显示主窗口」
	procSetMenuDefault.Call(menu, idmShow, 1)

	var pt pointStruct
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// 经典坑：不先 SetForegroundWindow，点菜单外区域菜单不收起、
	// WM_COMMAND 也可能收不到。TrackPopupMenu 返回后再补一个 WM_NULL。
	procSetForeground.Call(uintptr(hwnd))
	procTrackPopup.Call(menu, tpmRightButton|tpmBottomAlign,
		uintptr(pt.X), uintptr(pt.Y), 0, uintptr(hwnd), 0)
	procPostMessage.Call(uintptr(hwnd), wmNull, 0, 0)
}

func appendItem(menu uintptr, id uintptr, text string, grayed bool) {
	s, err := syscall.UTF16PtrFromString(text)
	if err != nil {
		return
	}
	flags := uintptr(mfString)
	if grayed {
		flags |= mfGrayed
	}
	procAppendMenu.Call(menu, flags, id, uintptr(unsafe.Pointer(s)))
}

func appendSep(menu uintptr) {
	procAppendMenu.Call(menu, mfSeparator, 0, 0)
}

// copyUTF16 把 Go 字符串拷进定长 UTF-16 数组（含终止 0；超长截断）。
func copyUTF16(dst []uint16, s string) {
	u, err := syscall.UTF16FromString(s)
	if err != nil {
		return
	}
	copy(dst, u) // u 末尾自带 0；copy 截断到 len(dst)
}

// makeShareIcon 程序化绘制托盘图标：蓝色圆底 + 白色显示器剪影。
//
// 用 CreateDIBSection 拿到一块 32bpp 位图的可写内存，Go 侧直接填像素
// （top-down，BGRA），再配一张全 0 的 1bpp mask（全不透明）交给
// CreateIconIndirect。运行时零资源依赖，go run 与发布构建一致。
func makeShareIcon(size int) windows.Handle {
	hdr := bitmapInfoHeader{
		biSize:      uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		biWidth:     int32(size),
		biHeight:    -int32(size), // 负 = top-down
		biPlanes:    1,
		biBitCount:  32,
		biSizeImage: uint32(size * size * 4),
	}
	bmi := struct {
		hdr bitmapInfoHeader
		clr [3]uint32
	}{hdr: hdr}
	// bits 声明成 *byte 而不是 uintptr：Windows 分配的这块内存在本函数内
	// 一直有效，直接转切片画像素；若走 uintptr→unsafe.Pointer 会被 vet
	// 的 checkptr 警告（那种转换确实只在特定时序下安全，这里避开更干净）。
	var bits *byte
	hbm, _, err := procCreateDIB.Call(0, uintptr(unsafe.Pointer(&bmi)), 0,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbm == 0 || bits == nil {
		_ = err
		return 0
	}
	defer procDeleteObject.Call(hbm)

	// 直接往 DIB 内存里画像素（BGRA 小端字节序）。
	px := unsafe.Slice(bits, size*size*4)
	PaintIcon(px, size, true)

	mask, _, _ := procCreateBitmap.Call(uintptr(size), uintptr(size), 1, 1, 0)
	if mask == 0 {
		return 0
	}
	defer procDeleteObject.Call(mask)

	ii := iconInfo{fIcon: 1, hbmMask: windows.Handle(mask), hbmColor: windows.Handle(hbm)}
	hIcon, _, _ := procCreateIconInd.Call(uintptr(unsafe.Pointer(&ii)))
	return windows.Handle(hIcon)
}
