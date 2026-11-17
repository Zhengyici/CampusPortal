package main

// 托盘图标：不引第三方库，全部用 Win32 API 自己画图标 + Shell_NotifyIcon。
//
// 设计要点：
//   - 图标在运行时用 CreateDIBSection 逐像素画出来（Wi-Fi 三道弧 + 圆点），
//     颜色随状态变化，所以 exe 里不需要带 .ico 文件，也不用落地临时文件。
//   - 窗口是隐藏的，只用来收托盘消息和弹右键菜单，靠 GetMessage 泵消息。
//   - 消息泵必须跑在锁定后的 OS 线程上，所以 runTray 里做了 runtime.LockOSThread()。

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

// ------------------------------------------------------------------ 运行状态（守护循环写，托盘读）

type status struct {
	mu      sync.RWMutex
	online  bool
	checked time.Time
	detail  string
	busy    bool
	logins  int
	force   bool // 托盘点了「立即连接」：下一轮跳过探测，强制走一遍登录
}

// requestForce 由托盘调用：要求下一轮无条件登录一次。
func (s *status) requestForce() {
	s.mu.Lock()
	s.force = true
	s.mu.Unlock()
}

// consumeForce 由守护循环调用：返回是否有待处理的强制登录请求，并清零。
func (s *status) consumeForce() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.force
	s.force = false
	return f
}

func (s *status) set(online bool, detail string) {
	s.mu.Lock()
	s.online, s.detail, s.checked, s.busy = online, detail, time.Now(), false
	s.mu.Unlock()
}

func (s *status) markBusy(detail string) {
	s.mu.Lock()
	s.busy = true
	s.detail = detail
	s.mu.Unlock()
}

func (s *status) countLogin() {
	s.mu.Lock()
	s.logins++
	s.mu.Unlock()
}

func (s *status) snapshot() (online bool, checked time.Time, detail string, busy bool, logins int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.online, s.checked, s.detail, s.busy, s.logins
}

// ------------------------------------------------------------------ Win32 常量

const (
	wmApp        = 0x8000
	wmTray       = wmApp + 1
	wmTimer      = 0x0113
	wmCommand    = 0x0111
	wmDestroy    = 0x0002
	wmNull       = 0x0000
	wmLButtonUp  = 0x0202
	wmRButtonUp  = 0x0205
	wmLButtonDbl = 0x0203

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x1
	nifIcon    = 0x2
	nifTip     = 0x4
	nifInfo    = 0x10

	niiInfo = 0x1

	trayID   = 1
	idTimer  = 1
	idConn   = 1
	idLog    = 2
	idExit   = 3
	idStatus = 100

	mfString       = 0x0
	mfGrayed       = 0x1
	mfSeparator    = 0x800
	tpmRightButton = 0x2
	tpmReturnCmd   = 0x100

	smCXSmIcon = 49

	idcArrow = 32512

	notifyIconDataSize = 976 // sizeof(NOTIFYICONDATAW)，x64
)

var (
	u32                  = syscall.NewLazyDLL("user32.dll")
	sh32                 = syscall.NewLazyDLL("shell32.dll")
	g32                  = syscall.NewLazyDLL("gdi32.dll")
	procRegisterClassEx  = u32.NewProc("RegisterClassExW")
	procCreateWindowEx   = u32.NewProc("CreateWindowExW")
	procDefWindowProc    = u32.NewProc("DefWindowProcW")
	procGetMessage       = u32.NewProc("GetMessageW")
	procTranslateMessage = u32.NewProc("TranslateMessage")
	procDispatchMessage  = u32.NewProc("DispatchMessageW")
	procPostQuitMessage  = u32.NewProc("PostQuitMessage")
	procPostMessage      = u32.NewProc("PostMessageW")
	procCreatePopupMenu  = u32.NewProc("CreatePopupMenu")
	procAppendMenu       = u32.NewProc("AppendMenuW")
	procTrackPopupMenu   = u32.NewProc("TrackPopupMenu")
	procDestroyMenu      = u32.NewProc("DestroyMenu")
	procGetCursorPos     = u32.NewProc("GetCursorPos")
	procSetForeground    = u32.NewProc("SetForegroundWindow")
	procDestroyIcon      = u32.NewProc("DestroyIcon")
	procLoadCursor       = u32.NewProc("LoadCursorW")
	procGetSystemMetrics = u32.NewProc("GetSystemMetrics")
	procRegisterWindowMs = u32.NewProc("RegisterWindowMessageW")
	procSetTimer         = u32.NewProc("SetTimer")
	procCreateIconIndir  = u32.NewProc("CreateIconIndirect") // user32，不是 gdi32
	procGetDC            = u32.NewProc("GetDC")
	procReleaseDC        = u32.NewProc("ReleaseDC")
	procShellNotifyIcon  = sh32.NewProc("Shell_NotifyIconW")
	procCreateDIBSection = g32.NewProc("CreateDIBSection")
	procCreateBitmap     = g32.NewProc("CreateBitmap")
	procDeleteObject     = g32.NewProc("DeleteObject")
	procGetModuleHandle  = kernel32.NewProc("GetModuleHandleW")
)

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type wndMsg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	ptX     int32
	ptY     int32
}

type point struct{ x, y int32 }

type iconInfo struct {
	fIcon    int32
	xHotspot uint32
	yHotspot uint32
	hbmMask  uintptr
	hbmColor uintptr
}

// ------------------------------------------------------------------ 图标绘制

func toUTF16z(s string) *uint16 {
	u := utf16.Encode([]rune(s))
	u = append(u, 0)
	return &u[0]
}

// cover 算出一个「到某个距离的覆盖度」的软边 alpha（0~1）。
func cover(dist, target, half, feather float64) float64 {
	d := math.Abs(dist-target) - half
	if d <= -feather {
		return 1
	}
	if d >= feather {
		return 0
	}
	return (feather - d) / (2 * feather)
}

// drawWifi 画一枚 Wi-Fi 图标，返回 BGRA（图标要求的预乘 alpha 格式）。
func drawWifi(size int, r, g, b byte) []byte {
	px := make([]byte, size*size*4)
	cx := float64(size) / 2
	cy := 0.74 * float64(size)
	thick := 0.105 * float64(size)
	dotR := 0.085 * float64(size)
	rings := []float64{0.175 * float64(size), 0.335 * float64(size), 0.495 * float64(size)}
	feather := 0.55
	half := thick / 2
	spread := math.Pi / 4 // 弧张角 ±45°

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx := float64(x) + 0.5
			fy := float64(y) + 0.5
			dx := fx - cx
			dy := cy - fy // 向上为正
			dist := math.Hypot(dx, dy)

			a := cover(dist, 0, dotR, feather)
			if dy >= 0 {
				ang := math.Atan2(math.Abs(dx), dy)
				if ang < spread {
					for _, rr := range rings {
						if c := cover(dist, rr, half, feather); c > a {
							a = c
						}
					}
				} else if ang < spread+0.16 { // 弧的末端做一点收边，别像被刀切
					fade := (spread + 0.16 - ang) / 0.16
					for _, rr := range rings {
						if c := cover(dist, rr, half, feather) * fade; c > a {
							a = c
						}
					}
				}
			}
			if a <= 0.002 {
				continue
			}
			i := (y*size + x) * 4
			// 预乘 alpha：图标 DIB 的约定
			px[i+0] = byte(float64(b) * a)
			px[i+1] = byte(float64(g) * a)
			px[i+2] = byte(float64(r) * a)
			px[i+3] = byte(255 * a)
		}
	}
	return px
}

// bitmapInfoHeader 与 Win32 的 BITMAPINFOHEADER 一致（40 字节）。
type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32 // 取负 = top-down，像素行从上到下
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

// makeIcon 把一段 BGRA 像素包成 HICON。
//
// 注意两个坑（都踩过）：
//   - 必须用 struct 传 BITMAPINFOHEADER，手工拼 40 字节的数组会让 CreateDIBSection 静默返回 0；
//   - CreateIconIndirect 在 user32.dll，不在 gdi32.dll。
func makeIcon(size int, px []byte) uintptr {
	bih := bitmapInfoHeader{
		biSize:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		biWidth:       int32(size),
		biHeight:      -int32(size),
		biPlanes:      1,
		biBitCount:    32,
		biCompression: 0, // BI_RGB
	}

	// 用屏幕 DC 建 DIB：兼容 DC 在某些会话下建不出来
	sdc, _, _ := procGetDC.Call(0)
	if sdc == 0 {
		return 0
	}
	var bits unsafe.Pointer
	hbmColor, _, _ := procCreateDIBSection.Call(sdc, uintptr(unsafe.Pointer(&bih)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	procReleaseDC.Call(0, sdc)
	if hbmColor == 0 || bits == nil {
		return 0
	}
	copy(unsafe.Slice((*byte)(bits), size*size*4), px)

	// 掩码位图全 0：32bpp 带 alpha 时由 alpha 通道决定透明
	maskLen := ((size + 15) / 16) * 2 * size
	mask := make([]byte, maskLen)
	hbmMask, _, _ := procCreateBitmap.Call(uintptr(size), uintptr(size), 1, 1, uintptr(unsafe.Pointer(&mask[0])))
	if hbmMask == 0 {
		procDeleteObject.Call(hbmColor)
		return 0
	}

	ii := iconInfo{fIcon: 1, hbmMask: hbmMask, hbmColor: hbmColor}
	h, _, _ := procCreateIconIndir.Call(uintptr(unsafe.Pointer(&ii)))

	procDeleteObject.Call(hbmColor)
	procDeleteObject.Call(hbmMask)
	return h
}

// ------------------------------------------------------------------ 托盘本体

type tray struct {
	hwnd     uintptr
	state    *status
	wake     chan<- struct{}
	logPath  string
	dir      string
	log      *Logger
	iconOn   uintptr
	iconOff  uintptr
	iconBusy uintptr
	lastIcon uintptr
	lastTip  string
	acked    bool // 已经加过一次图标（explorer 重启后要重加）
	taskbar  uint32
}

var (
	trayInstance *tray
	trayProc     = syscall.NewCallback(trayWndProc)
)

func trayWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	t := trayInstance
	if t != nil {
		switch {
		case msg == wmTray:
			ev := uint32(lParam) & 0xffff
			if ev == wmRButtonUp || ev == wmLButtonUp || ev == wmLButtonDbl {
				t.showMenu()
				return 0
			}
			return 0
		case msg == wmTimer:
			t.refresh(false)
			return 0
		case msg == wmCommand:
			switch int(wParam & 0xffff) {
			case idConn:
				t.triggerConnect()
			case idLog:
				t.openLog()
			case idExit:
				t.quit()
			}
			return 0
		case msg == wmDestroy:
			procPostQuitMessage.Call(0)
			return 0
		}
		if t != nil && t.taskbar != 0 && uint32(msg) == t.taskbar {
			t.acked = false
			t.refresh(true) // explorer 重启：把图标加回去
			return 0
		}
	}
	r, _, _ := procDefWindowProc.Call(hwnd, msg, wParam, lParam)
	return r
}

func (t *tray) triggerConnect() {
	t.state.requestForce()
	t.state.markBusy("手动连接中…")
	select {
	case t.wake <- struct{}{}:
	default: // 已经排了一个待处理任务，不重复排队
	}
	t.refresh(true)
}

func (t *tray) openLog() {
	if _, err := os.Stat(t.logPath); err != nil {
		t.balloon("CampusPortal", "还没有日志文件（暂无异常记录）")
		return
	}
	c := exec.Command("notepad.exe", t.logPath)
	_ = c.Start()
}

func (t *tray) quit() {
	t.remove()
	os.Exit(0)
}

func (t *tray) balloon(title, text string) {
	buf := t.newNID(nifInfo)
	putUTF16(buf[40:], 128, "CampusPortal")
	putUTF16(buf[304:], 256, text)
	putUTF16(buf[820:], 64, title)
	binary.LittleEndian.PutUint32(buf[948:], niiInfo)
	procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&buf[0])))
}

func (t *tray) newNID(extra uint32) []byte {
	buf := make([]byte, notifyIconDataSize)
	binary.LittleEndian.PutUint32(buf[0:], notifyIconDataSize)
	binary.LittleEndian.PutUint64(buf[8:], uint64(t.hwnd))
	binary.LittleEndian.PutUint32(buf[16:], trayID)
	binary.LittleEndian.PutUint32(buf[20:], nifMessage|nifIcon|nifTip|extra)
	binary.LittleEndian.PutUint32(buf[24:], wmTray)
	binary.LittleEndian.PutUint64(buf[32:], uint64(t.currentIcon()))
	return buf
}

func putUTF16(dst []byte, units int, s string) {
	u := utf16.Encode([]rune(s))
	if len(u) > units-1 {
		u = u[:units-1]
	}
	for i, c := range u {
		binary.LittleEndian.PutUint16(dst[i*2:], c)
	}
}

func (t *tray) currentIcon() uintptr {
	online, _, _, busy, _ := t.state.snapshot()
	switch {
	case busy:
		return t.iconBusy
	case online:
		return t.iconOn
	default:
		return t.iconOff
	}
}

func (t *tray) tip() string {
	online, checked, detail, busy, logins := t.state.snapshot()
	head := "未知"
	switch {
	case busy:
		head = "检查中…"
	case online:
		head = "在线"
	default:
		head = "离线"
	}
	if checked.IsZero() {
		return fmt.Sprintf("CampusPortal · %s｜尚未检查", head)
	}
	tip := fmt.Sprintf("CampusPortal · %s｜上次检查 %s", head, checked.Format("01-02 15:04"))
	if logins > 0 {
		tip += fmt.Sprintf("｜已自动登录 %d 次", logins)
	}
	_ = detail
	return tip
}

// refresh 更新提示文字和图标；force 为 true 时重新 add 图标。
func (t *tray) refresh(force bool) {
	tip := t.tip()
	icon := t.currentIcon()
	if !force && tip == t.lastTip && icon == t.lastIcon && t.acked {
		return
	}
	buf := t.newNID(0)
	putUTF16(buf[40:], 128, tip)
	op := uintptr(nimModify)
	if !t.acked || force {
		op = nimAdd
	}
	if r, _, _ := procShellNotifyIcon.Call(op, uintptr(unsafe.Pointer(&buf[0]))); r == 0 && op == nimAdd {
		return // 加不上（资源管理器还没起来），留待下一轮
	}
	t.acked = true
	t.lastTip, t.lastIcon = tip, icon
}

func (t *tray) remove() {
	buf := t.newNID(0)
	binary.LittleEndian.PutUint32(buf[20:], 0)
	procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&buf[0])))
}

func (t *tray) showMenu() {
	online, checked, detail, busy, logins := t.state.snapshot()
	h := t.hwnd
	menu, _, _ := procCreatePopupMenu.Call()

	add := func(flags uintptr, id uintptr, text string) {
		procAppendMenu.Call(menu, flags, id, uintptr(unsafe.Pointer(toUTF16z(text))))
	}

	stateText := "状态：离线"
	if busy {
		stateText = "状态：检查中…"
	} else if online {
		stateText = "状态：在线"
	}
	add(mfString|mfGrayed, idStatus, stateText)

	checkText := "上次检查：尚未检查"
	if !checked.IsZero() {
		checkText = "上次检查：" + checked.Format("01-02 15:04:05")
		if !busy {
			if online {
				checkText += "（正常）"
			} else {
				checkText += "（异常）"
			}
		}
	}
	add(mfString|mfGrayed, idStatus, checkText)

	if logins > 0 {
		add(mfString|mfGrayed, idStatus, fmt.Sprintf("本次运行已自动登录 %d 次", logins))
	}
	_ = detail

	add(mfSeparator, 0, "")
	connText := "立即连接"
	if busy {
		connText = "正在连接…"
	}
	add(mfString, idConn, connText)
	add(mfString, idLog, "打开日志")
	add(mfSeparator, 0, "")
	add(mfString, idExit, "退出")

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForeground.Call(h)
	cmd, _, _ := procTrackPopupMenu.Call(menu,
		tpmRightButton|tpmReturnCmd, uintptr(pt.x), uintptr(pt.y), 0, h, 0)
	procDestroyMenu.Call(menu)
	procPostMessage.Call(h, wmNull, 0, 0) // 让菜单在点击别处时正常消失

	switch int(cmd) {
	case idConn:
		t.triggerConnect()
	case idLog:
		t.openLog()
	case idExit:
		t.quit()
	}
}

// ------------------------------------------------------------------ 启动

// runTray 建隐藏窗口 + 托盘图标，然后阻塞在消息泵里。返回非 nil 表示托盘起不来。
func runTray(st *status, wake chan<- struct{}, dir string, log *Logger) error {
	runtime.LockOSThread()

	hInst, _, _ := procGetModuleHandle.Call(0)
	clsName := toUTF16z("CampusPortalTrayWnd")
	wndName := toUTF16z("CampusPortal")
	cur, _, _ := procLoadCursor.Call(0, idcArrow)

	sm, _, _ := procGetSystemMetrics.Call(smCXSmIcon)
	small := int(sm)
	if small < 16 {
		small = 16
	}

	iconOn := makeIcon(small, drawWifi(small, 0x22, 0xD3, 0xA6))  // 青绿 = 在线
	iconOff := makeIcon(small, drawWifi(small, 0xFF, 0x4F, 0x1A)) // 橙红 = 离线
	iconBusy := makeIcon(small, drawWifi(small, 0x8B, 0x5C, 0xF6))
	if iconOn == 0 || iconOff == 0 || iconBusy == 0 {
		return fmt.Errorf("makeIcon 失败（size=%d, on=%d off=%d busy=%d）", small, iconOn, iconOff, iconBusy)
	}

	wc := wndClassEx{
		cbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		style:         0,
		lpfnWndProc:   trayProc,
		hInstance:     hInst,
		hIcon:         iconOn,
		hCursor:       cur,
		hbrBackground: 0,
		lpszClassName: clsName,
		hIconSm:       iconOn,
	}
	if r, _, e := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return fmt.Errorf("RegisterClassExW 失败: %v (class=CampusPortalTrayWnd)", e)
	}

	hwnd, _, e2 := procCreateWindowEx.Call(0,
		uintptr(unsafe.Pointer(clsName)), uintptr(unsafe.Pointer(wndName)),
		0, 0, 0, 0, 0, 0, 0, hInst, 0)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowExW 失败: %v", e2)
	}

	t := &tray{
		hwnd: hwnd, state: st, wake: wake, log: log, dir: dir,
		logPath: filepath.Join(dir, logFile),
		iconOn:  iconOn, iconOff: iconOff, iconBusy: iconBusy,
	}
	trayInstance = t

	if name, _, _ := procRegisterWindowMs.Call(uintptr(unsafe.Pointer(toUTF16z("TaskbarCreated")))); name != 0 {
		t.taskbar = uint32(name)
	}

	procSetTimer.Call(hwnd, idTimer, 1000, 0)
	if err := t.add(); err != nil {
		return exeErr("Shell_NotifyIcon(NIM_ADD)", err)
	}
	t.refresh(true)

	var m wndMsg
	for {
		r, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
	t.remove()
	runtime.UnlockOSThread()
	return nil
}

func exeErr(stage string, err error) error {
	if err == nil || err.Error() == "The operation completed successfully." {
		return fmt.Errorf("%s 返回失败（errno 为 0，通常是当前会话没有资源管理器 / 无窗口站）", stage)
	}
	return fmt.Errorf("%s 失败: %v", stage, err)
}

// add 首次加入托盘。
//
// Shell_NotifyIcon 偶尔会返回 E_ACCESSDENIED（任务栏还没准备好像素、explorer
// 正在重建、或被安全软件短暂拦一下），所以这里重试若干次而不是一次失败就放弃。
func (t *tray) add() error {
	var lastErr error
	for i := 0; i < 20; i++ {
		buf := t.newNID(0)
		putUTF16(buf[40:], 128, t.tip())
		r, _, e := procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&buf[0])))
		if r != 0 {
			t.acked = true
			t.lastTip, t.lastIcon = t.tip(), t.currentIcon()
			return nil
		}
		lastErr = e
		time.Sleep(500 * time.Millisecond)
	}
	return lastErr
}
