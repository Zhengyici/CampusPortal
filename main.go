package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

const (
	VERSION   = "1.1.2"
	cfgFile   = "config.json"
	secFile   = "secrets.dat"
	logFile   = "portal.log"
	pidFile   = "portal.pid"
	stateFile = "portal.state"
	logMax    = 512 * 1024
	appID     = "CampusPortal"
)

// ------------------------------------------------------------------ 配置

type Config struct {
	User string `json:"user"`

	// 登录接口（用域名，TLS 证书按域名校验）
	Portal string `json:"portal"`

	// 门户真实 IP（可选，推荐填）。
	// 门户通常是内网地址，而 Clash / mihomo 一类代理的 TUN + fake-ip 模式
	// 会把域名解析成 198.18.x.x 的假地址，登录请求就打不到门户。
	// 填了真实 IP 就固定直连它，SNI 和证书校验仍按域名走。留空则走系统解析。
	PortalIP string `json:"portal_ip"`

	// 绑定本机哪个 IPv4 作为源地址去连门户（留空则在运行时自动挑选同网段地址）。
	// 作用：把登录流量钉在校园网网卡上，绕开 Clash 的 TUN 虚拟网卡。
	BindLocalIP string `json:"bind_local_ip"`

	ProbeURLs  []string `json:"probe_urls"`
	ProbeTexts []string `json:"probe_expect"`

	Interval   int `json:"interval_sec"`
	Timeout    int `json:"timeout_sec"`
	MaxBackoff int `json:"max_backoff_sec"`
}

func defaultConfig() Config {
	return Config{
		User: "",

		// 门户地址因校而异，留空，由 -setup 询问后写入 config.json。
		Portal:   "",
		PortalIP: "",
		ProbeURLs: []string{
			"http://www.baidu.com/",
			"http://www.msftconnecttest.com/connecttest.txt",
		},
		ProbeTexts: []string{
			"百度一下",
			"Microsoft Connect Test",
		},
		// 10 分钟一轮：实测每轮探测约 0.14 秒 CPU、2.3 KB 流量，
		// 一天 144 轮合计不到 2 秒 CPU、335 KB，占用可以忽略；
		// 换来的是掉线最多 10 分钟就被发现（原来是 30 分钟）。
		Interval:   600,
		Timeout:    8,
		MaxBackoff: 300,
	}
}

// ------------------------------------------------------------------ 日志

type Logger struct {
	mu   sync.Mutex
	path string
}

func (l *Logger) log(level, format string, a ...interface{}) {
	line := fmt.Sprintf("%s [%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), level, fmt.Sprintf(format, a...))
	l.mu.Lock()
	defer l.mu.Unlock()
	if fi, err := os.Stat(l.path); err == nil && fi.Size() > logMax {
		_ = os.Remove(l.path + ".1")
		_ = os.Rename(l.path, l.path+".1")
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line)
	_ = f.Close()
}

func (l *Logger) Info(f string, a ...interface{})  { l.log("INFO", f, a...) }
func (l *Logger) Warn(f string, a ...interface{})  { l.log("WARN", f, a...) }
func (l *Logger) Error(f string, a ...interface{}) { l.log("ERROR", f, a...) }

// ------------------------------------------------------------------ Win32

var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	crypt32              = syscall.NewLazyDLL("crypt32.dll")
	procProtect          = crypt32.NewProc("CryptProtectData")
	procUnprotect        = crypt32.NewProc("CryptUnprotectData")
	procLocalFree        = kernel32.NewProc("LocalFree")
	procCreateMutex      = kernel32.NewProc("CreateMutexW")
	procGetConsoleMode   = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode   = kernel32.NewProc("SetConsoleMode")
	procAttachConsole    = kernel32.NewProc("AttachConsole")
	procAllocConsole     = kernel32.NewProc("AllocConsole")
	procGetStdHandle     = kernel32.NewProc("GetStdHandle")
	procGetFileType      = kernel32.NewProc("GetFileType")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procSetConsoleTitle  = kernel32.NewProc("SetConsoleTitleW")
	user32               = syscall.NewLazyDLL("user32.dll")
	procShowWindow       = user32.NewProc("ShowWindow")
	procMessageBox       = user32.NewProc("MessageBoxW")
)

const (
	stdOutputHandle     = ^uintptr(11) // -11
	attachParentProcess = ^uintptr(0)  // -1
	fileTypeChar        = 1
	fileTypePipe        = 3
	fileTypeDisk        = 4
	swHide              = 0
	mbIconInformation   = 0x00000040
	mbIconWarning       = 0x00000030
	mbIconQuestion      = 0x00000020
	mbYesNo             = 0x00000004
	idYes               = 6
	// CreateProcess 的 CreationFlags：让新进程完全脱离当前控制台，
	// 关掉向导窗口时守护进程不会跟着一起死。
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
)

// ensureConsole 为 GUI 子系统程序按需接上控制台。
// 程序编译成 -H windowsgui（后台运行时不弹黑框）；被 cmd 双击或命令行调用时，
// 这里再附着到父进程的控制台，提示符和输出就正常了。
// 如果 stdout 已被重定向到文件或管道，则保持原样不动。
// 找不到父控制台时（典型场景：资源管理器里直接双击 exe）自己开一个，
// 否则所有提示都会写进无底洞，用户看到的只是「双击了没反应」。
func ensureConsole() {
	if h, _, _ := procGetStdHandle.Call(stdOutputHandle); h != 0 && h != ^uintptr(0) {
		switch t, _, _ := procGetFileType.Call(h); t {
		case fileTypeChar: // 已经是控制台
			return
		case fileTypePipe, fileTypeDisk: // 被重定向，保留
			return
		}
	}
	// 先试着借用父进程的控制台（从 cmd 里调用时走这条）。
	if r, _, _ := procAttachConsole.Call(attachParentProcess); r == 0 {
		// 没有父控制台：自己开一个，让提示能显示出来。
		if r2, _, _ := procAllocConsole.Call(); r2 == 0 {
			return
		}
	}
	if title, err := syscall.UTF16PtrFromString("CampusPortal"); err == nil {
		procSetConsoleTitle.Call(uintptr(unsafe.Pointer(title)))
	}
	if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stdout = f
		os.Stderr = f
	}
	if f, err := os.OpenFile("CONIN$", os.O_RDWR, 0); err == nil {
		os.Stdin = f
	}
}

// msgBox 弹一个原生提示框。用于没有控制台、又必须让用户看见信息的场合。
func msgBox(text, caption string, warn bool) {
	t, err1 := syscall.UTF16PtrFromString(text)
	c, err2 := syscall.UTF16PtrFromString(caption)
	if err1 != nil || err2 != nil {
		return
	}
	flags := uintptr(mbIconInformation)
	if warn {
		flags = mbIconWarning
	}
	procMessageBox.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), flags)
}

// hasConsole 判断当前进程是否真的能往某个地方输出提示。
func hasConsole() bool {
	h, _, _ := procGetStdHandle.Call(stdOutputHandle)
	if h == 0 || h == ^uintptr(0) {
		return false
	}
	t, _, _ := procGetFileType.Call(h)
	return t == fileTypeChar || t == fileTypePipe || t == fileTypeDisk
}

// pauseOnFail 由 -pause 打开：出错时停一下等回车，方便双击运行时看清原因。
var pauseOnFail bool

// fail 报告一个「没法继续」的原因。
// 有控制台就打印并以非零码退出，没有就弹窗——直接双击 exe 的人
// 看不到黑框，只能靠弹窗知道发生了什么。
func fail(msg string) {
	if hasConsole() {
		fmt.Fprintln(os.Stderr, msg)
		if pauseOnFail {
			pauseBeforeExit()
		}
		return
	}
	msgBox(msg, "CampusPortal", true)
}

// pauseBeforeExit 让「双击 exe 里跑完就退」的场景留给用户看一眼的机会。
func pauseBeforeExit() {
	if hasConsole() {
		fmt.Print("\n按回车键退出… ")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
}

// hideConsoleWindow 兜底：万一进程仍带着控制台窗口，直接藏起来。
func hideConsoleWindow() {
	if h, _, _ := procGetConsoleWindow.Call(); h != 0 {
		procShowWindow.Call(h, swHide)
	}
}

type dataBlob struct {
	cbData uint32
	pbData *byte
}

func newBlob(d []byte) *dataBlob {
	if len(d) == 0 {
		return &dataBlob{}
	}
	return &dataBlob{cbData: uint32(len(d)), pbData: &d[0]}
}

func (b *dataBlob) bytes() []byte {
	out := make([]byte, b.cbData)
	if b.cbData > 0 && b.pbData != nil {
		copy(out, unsafe.Slice(b.pbData, int(b.cbData)))
	}
	return out
}

func dpapiProtect(plain []byte) ([]byte, error) {
	in := newBlob(plain)
	var out dataBlob
	r, _, e := procProtect.Call(uintptr(unsafe.Pointer(in)), 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, e
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return out.bytes(), nil
}

func dpapiUnprotect(enc []byte) ([]byte, error) {
	in := newBlob(enc)
	var out dataBlob
	r, _, e := procUnprotect.Call(uintptr(unsafe.Pointer(in)), 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, e
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return out.bytes(), nil
}

// ------------------------------------------------------------------ RC4（复刻 H3C ac_portal 前端）

// rc4Hex 等价于 logic_new.js 的 do_encrypt_rc4(src, passwd)：
// key[i] = passwd.charCodeAt(i % passwd.length)，输出每字节 2 位小写 hex。
func rc4Hex(src, passwd string) string {
	if passwd == "" {
		return src
	}
	keyU := utf16.Encode([]rune(passwd))
	srcU := utf16.Encode([]rune(src))
	sbox := make([]int, 256)
	for i := 0; i < 256; i++ {
		sbox[i] = i
	}
	j := 0
	for i := 0; i < 256; i++ {
		j = (j + sbox[i] + int(keyU[i%len(keyU)])) % 256
		sbox[i], sbox[j] = sbox[j], sbox[i]
	}
	a, b := 0, 0
	var sb strings.Builder
	sb.Grow(len(srcU) * 2)
	for _, cu := range srcU {
		a = (a + 1) % 256
		b = (b + sbox[a]) % 256
		sbox[a], sbox[b] = sbox[b], sbox[a]
		k := (sbox[a] + sbox[b]) % 256
		sb.WriteString(fmt.Sprintf("%02x", (int(cu)^sbox[k])&0xFF))
	}
	return sb.String()
}

// ------------------------------------------------------------------ 源地址

// localAddr 把连接钉在指定源 IPv4 上。必须用 Dialer.LocalAddr，
// 不能用 RawConn + syscall.Bind —— Go 建的是 AF_INET6 双栈套接字，绑 IPv4 会报
// "An invalid argument was supplied"。
func localAddr(localIP string) net.Addr {
	if localIP == "" {
		return nil
	}
	ip := net.ParseIP(localIP).To4()
	if ip == nil {
		return nil
	}
	return &net.TCPAddr{IP: ip}
}

// autoBindIP 找本机与门户 IP 同 /8 段的 IPv4（即校园网网卡的地址）。
func autoBindIP(portalIP string) string {
	target := net.ParseIP(portalIP).To4()
	if target == nil {
		return ""
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	var cands []string
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if ip[0] == 198 && ip[1] == 18 {
				continue // Clash TUN 假地址
			}
			if ip[0] != target[0] {
				continue
			}
			cands = append(cands, ip.String())
		}
	}
	if len(cands) == 0 {
		return ""
	}
	sort.Strings(cands)
	return cands[0]
}

// ------------------------------------------------------------------ HTTP：一律直连

func baseTransport(timeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy:                 nil, // 绝不使用系统/环境变量代理
		DialContext:           (&net.Dialer{Timeout: timeout}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
	}
}

// portalTransport 把连接钉到 targetIP + 指定源地址，SNI / 证书仍按域名校验。
func portalTransport(targetIP, bindIP string, timeout time.Duration) *http.Transport {
	tr := baseTransport(timeout)
	d := &net.Dialer{Timeout: timeout, LocalAddr: localAddr(bindIP)}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if targetIP == "" {
			return d.DialContext(ctx, network, addr)
		}
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			port = "443"
		}
		return d.DialContext(ctx, network, net.JoinHostPort(targetIP, port))
	}
	return tr
}

func newClient(tr *http.Transport, timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("重定向次数过多")
			}
			return nil
		},
	}
}

// ------------------------------------------------------------------ 探测

type prober struct {
	cfg    Config
	client *http.Client
}

func (p *prober) probe() (bool, string) {
	var first string
	for i, u := range p.cfg.ProbeURLs {
		expect := ""
		if i < len(p.cfg.ProbeTexts) {
			expect = p.cfg.ProbeTexts[i]
		}
		ok, detail := p.probeOne(u, expect)
		if ok {
			return true, detail
		}
		if i == 0 {
			first = detail
		}
	}
	if first == "" {
		first = "所有探测目标均失败"
	}
	return false, first
}

func (p *prober) probeOne(rawURL, expect string) (bool, string) {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return false, "构造请求失败: " + err.Error()
	}
	req.Header.Set("Cache-Control", "no-cache, no-store")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

	resp, err := p.client.Do(req)
	if err != nil {
		return false, "连接失败: " + short(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32768))
	if resp.StatusCode != 200 {
		return false, fmt.Sprintf("%s 返回 HTTP %d（疑似被门户拦截）", hostOf(rawURL), resp.StatusCode)
	}
	text := string(body)
	if expect != "" && !strings.Contains(text, expect) {
		if strings.Contains(text, "ac_portal") || strings.Contains(text, "身份认证") {
			return false, fmt.Sprintf("%s 被重定向到认证页", hostOf(rawURL))
		}
		return false, fmt.Sprintf("%s 内容不符（可能被劫持）", hostOf(rawURL))
	}
	return true, hostOf(rawURL) + " 可达"
}

func hostOf(u string) string {
	if pu, err := url.Parse(u); err == nil {
		return pu.Host
	}
	return u
}

func short(err error) string {
	s := err.Error()
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ------------------------------------------------------------------ 登录

type loginResp struct {
	Success  bool   `json:"success"`
	Msg      string `json:"msg"`
	Action   string `json:"action"`
	Pop      int    `json:"pop"`
	UserName string `json:"userName"`
	Location string `json:"location"`
}

// loginOnce 用给定的目标 IP 打一次登录接口。
func loginOnce(cfg Config, user, pass, targetIP, bindIP string, timeout time.Duration) (bool, string, error) {
	authTag := fmt.Sprintf("%d", time.Now().UnixMilli())

	form := url.Values{}
	form.Set("opr", "pwdLogin")
	form.Set("userName", user)
	form.Set("pwd", rc4Hex(pass, authTag))
	form.Set("auth_tag", authTag)
	form.Set("rememberPwd", "1")

	client := newClient(portalTransport(targetIP, bindIP, timeout), timeout)
	req, err := http.NewRequest("POST", cfg.Portal, strings.NewReader(form.Encode()))
	if err != nil {
		return false, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))

	var lr loginResp
	if err := json.Unmarshal(bytes.TrimSpace(body), &lr); err != nil {
		return false, "响应非 JSON（可能被劫持）: " + truncate(string(body), 160), nil
	}
	if lr.Success {
		if lr.Msg != "" {
			return true, lr.Msg, nil
		}
		return true, "登录成功", nil
	}
	if lr.Msg == "" {
		lr.Msg = "登录被拒绝（无返回消息）"
	}
	return false, lr.Msg, nil
}

// isTransient 判断这次失败的成因是「链路抖动」还是「配置/账号本身有问题」。
//
// 区分这两类很重要：密码错、地址写错，重试多少次都没用；
// 而校园 WiFi 断链重连的那几秒里，报文会石沉大海，
// 表现就是 context deadline exceeded / EOF / connection reset，等几秒就好。
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, k := range []string{
		"eof",
		"timeout",
		"timed out",
		"deadline exceeded",
		"connection reset",
		"connection refused",
		"forcibly closed",
		"broken pipe",
		"network is unreachable",
		"no route to host",
		"tls handshake",
		"wsarecv",
		"wsasend",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// login 在 loginPass 外面套一层重试。
//
// 起因：2026-11-18 11:57 手动点「立即连接」时三种连法全失败，日志是
// 一次 context deadline exceeded 加两次 EOF。查 Windows 事件日志发现
// 11:58:40 正好有一次 WLAN 断开重连，也就是说失败发生在网卡断链的窗口里。
// 这种抖动过几秒就恢复，多试两次即可，没必要让用户以为程序坏了。
//
// 只对「链路类」错误重试；门户明确回了 JSON（密码错、账号锁定）就立刻收手。
// 重试有总时长上限，避免校园网长时间断着时一次登录卡上一分多钟。
func login(cfg Config, user, pass string, timeout time.Duration) (bool, string) {
	const (
		maxAttempts = 3
		totalBudget = 40 * time.Second
	)
	deadline := time.Now().Add(totalBudget)
	msg := ""
	retried := 0

	for i := 1; i <= maxAttempts; i++ {
		ok, m, transient := loginPass(cfg, user, pass, timeout)
		msg = m
		if ok || !transient {
			if retried > 0 {
				return ok, fmt.Sprintf("%s（第 %d 次尝试成功）", msg, i)
			}
			return ok, msg
		}
		if i == maxAttempts {
			break
		}
		pause := time.Duration(2*i) * time.Second // 2s、4s
		if time.Now().Add(pause).After(deadline) {
			break
		}
		retried++
		time.Sleep(pause)
	}
	if retried > 0 {
		return false, fmt.Sprintf("%s（链路抖动，已重试 %d 次仍不通）", msg, retried)
	}
	return false, msg
}

// loginPass 按“最稳妥到最宽松”的顺序尝试几种连法，取第一个拿到门户 JSON 应答的：
//  1. 固定门户 IP + 绑定源地址（绕开 Clash TUN，最干净）
//  2. 固定门户 IP + 不绑定（应对源地址挑选失准）
//  3. 域名解析 + 不绑定（应对门户换了 IP）
//
// 只要某一种拿到了 JSON（哪怕是“密码错误”），就立即返回，不再往下试。
// 第三个返回值表示「全部连法都因为链路原因失败」，调用方可据此决定要不要重试。
func loginPass(cfg Config, user, pass string, timeout time.Duration) (bool, string, bool) {
	bindIP := cfg.BindLocalIP
	if bindIP == "" && cfg.PortalIP != "" {
		bindIP = autoBindIP(cfg.PortalIP)
	}

	type attempt struct {
		name   string
		target string
		bind   string
	}
	var attempts []attempt
	if cfg.PortalIP != "" {
		if bindIP != "" {
			attempts = append(attempts, attempt{"直连门户 IP + 绑定源地址", cfg.PortalIP, bindIP})
		}
		attempts = append(attempts, attempt{"直连门户 IP", cfg.PortalIP, ""})
	}
	attempts = append(attempts, attempt{"域名解析", "", ""})

	var errs []string
	allTransient := true
	for _, a := range attempts {
		ok, msg, err := loginOnce(cfg, user, pass, a.target, a.bind, timeout)
		if err == nil {
			return ok, msg, false // 门户正常应答了，结论有效
		}
		if !isTransient(err) {
			allTransient = false
		}
		errs = append(errs, a.name+"："+short(err))
	}
	return false, "连不上门户（" + strings.Join(errs, "；") + "）", allTransient
}

// ------------------------------------------------------------------ 文件

func exeDir() string {
	p, err := os.Executable()
	if err != nil {
		wd, _ := os.Getwd()
		return wd
	}
	d := filepath.Dir(p)
	if strings.Contains(strings.ToLower(d), "go-build") {
		wd, _ := os.Getwd()
		return wd
	}
	return d
}

func loadConfig(dir string) (Config, error) {
	cfg := defaultConfig()
	path := filepath.Join(dir, cfgFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			b, _ := json.MarshalIndent(cfg, "", "  ")
			_ = os.WriteFile(path, b, 0o644)
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("配置文件格式有误: %w", err)
	}
	d := defaultConfig()
	if cfg.Portal == "" {
		cfg.Portal = d.Portal
	}
	if len(cfg.ProbeURLs) == 0 {
		cfg.ProbeURLs = d.ProbeURLs
	}
	if len(cfg.ProbeTexts) == 0 {
		cfg.ProbeTexts = d.ProbeTexts
	}
	if cfg.Interval <= 0 {
		cfg.Interval = d.Interval
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = d.Timeout
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = d.MaxBackoff
	}
	return cfg, nil
}

func saveConfig(dir string, cfg Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, cfgFile), b, 0o644)
}

func readPassword(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, secFile))
	if err != nil {
		if os.IsNotExist(err) {
			return "", errors.New("还没保存密码，请先双击 1-设置账号密码.cmd")
		}
		return "", err
	}
	plain, err := dpapiUnprotect(b)
	if err != nil {
		return "", fmt.Errorf("密码解密失败（换了 Windows 用户或电脑后需要重新设置）: %w", err)
	}
	return string(plain), nil
}

func writePassword(dir, pw string) error {
	enc, err := dpapiProtect([]byte(pw))
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, secFile), enc, 0o600)
}

// readHidden 读一行输入并在输入过程中关闭回显。
// 注意：必须用一个 Reader 读完所有输入。bufio 会预读缓冲区，
// 若中途换了新 Reader，前面已预读的行会丢，导致后续提示读到空值。
func readHidden(rd *bufio.Reader) (string, error) {
	h := syscall.Handle(os.Stdin.Fd())
	var mode uint32
	okMode, _, _ := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode)))
	if okMode != 0 {
		const enableEcho = 0x0004
		procSetConsoleMode.Call(uintptr(h), uintptr(mode&^enableEcho))
		defer func() {
			procSetConsoleMode.Call(uintptr(h), uintptr(mode))
			fmt.Println()
		}()
	}
	line, err := rd.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func singleInstance() bool {
	name, _ := syscall.UTF16PtrFromString("Local\\" + appID + "-Daemon")
	h, _, e := procCreateMutex.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		fmt.Println("创建互斥体失败:", e)
		return false
	}
	const errAlreadyExists = 183
	if se, ok := e.(syscall.Errno); ok && se == errAlreadyExists {
		return false
	}
	return true
}

func writePID(dir string) {
	_ = os.WriteFile(filepath.Join(dir, pidFile), []byte(strconv.Itoa(os.Getpid())), 0o644)
}

func backoff(cfg Config, n int) int {
	w := 10 * n
	if w > cfg.MaxBackoff {
		w = cfg.MaxBackoff
	}
	if w < 10 {
		w = 10
	}
	return w
}

// ------------------------------------------------------------------ 开机自启

// installAutostart 直接把自己写进当前用户的 Run 键（不需要管理员权限）。
// 本程序编译为 windowsgui 子系统，正常运行时没有控制台窗口，
// 所以不需要 vbs / wscript 之类的中间层。
func installAutostart(dir string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if filepath.IsAbs(exe) {
		dir = filepath.Dir(exe)
		exe = filepath.Join(dir, filepath.Base(exe))
	}
	quoted := `"` + exe + `"`
	cmd := regAddCmd(quoted)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// regAddCmd 通过 cmd /c 调用 reg，避免 Go 直接传参时的引号处理差异。
func regAddCmd(value string) *exec.Cmd {
	line := fmt.Sprintf(`reg add "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v %s /t REG_SZ /d "%s" /f`, appID, value)
	return exec.Command("cmd", "/c", line)
}

func uninstallAutostart() error {
	cmd := exec.Command("cmd", "/c", `reg delete "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v `+appID+` /f`)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		s := string(out)
		if strings.Contains(s, "unable to find") || strings.Contains(s, "找不到") ||
			strings.Contains(s, "系统找不到") {
			return nil // 本来就没设置
		}
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(s))
	}
	return nil
}

// ------------------------------------------------------------------ main

func main() {
	var (
		setupFlag  = flag.Bool("setup", false, "设置学号与密码并测试登录")
		onceFlag   = flag.Bool("once", false, "只探测/登录一次，不常驻")
		diagFlag   = flag.Bool("diag", false, "输出诊断信息")
		forceFlag  = flag.Bool("force", false, "配合 -diag：即使在线也强制调一次登录接口")
		installFlg = flag.Bool("install", false, "设置开机自动启动")
		uninstFlg  = flag.Bool("uninstall", false, "取消开机自动启动")
		noTrayFlg  = flag.Bool("notray", false, "不显示托盘图标（纯后台静默）")
		userFlag   = flag.String("u", "", "配合 -setup：学号")
		pwFlag     = flag.String("pw", "", "配合 -setup：密码（不推荐，会留在命令历史）")
		ipFlag     = flag.String("ip", "", "配合 -setup：门户真实 IP")
		bindFlag   = flag.String("bind", "", "配合 -setup：源地址")
		portalFlag = flag.String("portal", "", "配合 -setup：登录接口完整地址")
		pauseFlag  = flag.Bool("pause", false, "出错时先停下等回车，便于双击运行时看清原因")
	)
	flag.Parse()
	pauseOnFail = *pauseFlag

	// 交互类命令需要控制台输出；守护模式则隐藏窗口静默运行。
	if *setupFlag || *diagFlag || *onceFlag || *installFlg || *uninstFlg {
		ensureConsole()
	} else {
		hideConsoleWindow()
	}

	dir := exeDir()
	_ = os.Chdir(dir)

	if *uninstFlg {
		if err := uninstallAutostart(); err != nil {
			fmt.Println("取消失败:", err)
			return
		}
		fmt.Println("已取消开机自启。")
		return
	}
	if *installFlg {
		if err := installAutostart(dir); err != nil {
			fmt.Println("自动写入启动项失败:", err)
			fmt.Println()
			fmt.Println("本机装了联想电脑管家，它会把注册表 Run 项挪进 LenovoDisabled 子键，")
			fmt.Println("所以命令行方式写自启在这台机器上不可用。")
			fmt.Println("→ 请改为双击 3-设为开机自启.cmd（它往「启动」文件夹放快捷方式，联想管家不管那里）。")
			return
		}
		fmt.Println("已设置开机自启（登录 Windows 后自动在后台运行，无窗口）。")
		return
	}

	log := &Logger{path: filepath.Join(dir, logFile)}
	cfg, err := loadConfig(dir)
	if err != nil {
		fail("读取配置失败: " + err.Error())
		return
	}

	// 没配置过就双击 exe，是别人第一次用这个程序最常见的路径。
	// 不给提示直接退出等于劝退，所以这里自己开个控制台跑一遍向导。
	if strings.TrimSpace(cfg.Portal) == "" && !*setupFlag {
		ensureConsole()
		if !hasConsole() {
			fail("还没配置登录接口，没法开始守护。\n\n" +
				"请先双击本程序（不是按右键「以管理员身份运行」），\n" +
				"它会打开一个窗口，按提示填校园网认证地址、学号和密码。")
			return
		}
		fmt.Println("检测到还没有配置过，先做一次初始化。")
		fmt.Println()
		doSetup(dir, cfg, *userFlag, *pwFlag, *ipFlag, *bindFlag, *portalFlag)
		pauseBeforeExit()
		return
	}

	if *setupFlag {
		doSetup(dir, cfg, *userFlag, *pwFlag, *ipFlag, *bindFlag, *portalFlag)
		return
	}

	pass, err := readPassword(dir)
	if err != nil {
		fail("读取密码失败：" + err.Error() + "\n\n重新双击本程序可以重新设置一遍。")
		return
	}
	if strings.TrimSpace(cfg.User) == "" {
		fail("还没设置学号。\n\n双击本程序，按提示填一遍即可。")
		return
	}
	cfg.User = strings.TrimSpace(cfg.User)

	if cfg.BindLocalIP == "" && cfg.PortalIP != "" {
		cfg.BindLocalIP = autoBindIP(cfg.PortalIP)
	}

	timeout := time.Duration(cfg.Timeout) * time.Second
	p := &prober{cfg: cfg, client: newClient(baseTransport(timeout), timeout)}

	if *diagFlag {
		runDiag(p, cfg, pass, *forceFlag)
		return
	}
	if *onceFlag {
		ok, detail := p.probe()
		fmt.Println("探测:", detail)
		if ok {
			return
		}
		okLogin, msg := login(cfg, cfg.User, pass, timeout)
		fmt.Println("登录:", msg)
		if okLogin {
			time.Sleep(2 * time.Second)
			ok2, d2 := p.probe()
			fmt.Println("复检:", ok2, d2)
		}
		return
	}

	if !singleInstance() {
		// 已经有一份在后台跑了。双击的人此刻什么都没发生，得告诉他去看托盘。
		if !*diagFlag && !*onceFlag {
			fail("CampusPortal 已经在后台运行了。\n\n" +
				"看任务栏右下角的托盘区，有个 Wi-Fi 图标：\n" +
				"绿色 = 在线，红色 = 已掉线，紫色 = 正在登录。\n" +
				"右键它可以「立即连接」或查看上次检查时间。\n\n" +
				"（图标可能被 Win11 收进了 ^ 溢出区里）")
		}
		return
	}
	writePID(dir)
	defer os.Remove(filepath.Join(dir, pidFile))

	st := &status{}
	loadState(filepath.Join(dir, stateFile), st)

	var wake chan struct{}
	if !*noTrayFlg {
		wake = make(chan struct{}, 1)
		st.markBusy("启动中…")
		go func() {
			if err := runTray(st, wake, dir, log); err != nil {
				// 托盘起不来（没有 explorer / 被策略禁掉）就当作纯后台跑，别让守护一起死掉
				log.Warn("托盘图标创建失败，转为静默后台运行：%v", err)
				st.set(false, "托盘不可用")
			}
		}()
		// 给托盘一点时间先画出来，再开始第一轮探测
		time.Sleep(600 * time.Millisecond)
	}

	daemon(p, cfg, pass, log, st, wake)
}

func doSetup(dir string, cfg Config, userFlag, pwFlag, ipFlag, bindFlag, portalFlag string) {
	fmt.Println("=== 校园网自动登录 · 初始化 ===")
	rd := bufio.NewReader(os.Stdin)
	ask := func(prompt, cur string) string {
		if cur == "" {
			cur = "未设置"
		}
		fmt.Printf("%s [当前: %s]: ", prompt, cur)
		line, _ := rd.ReadString('\n')
		return strings.TrimSpace(line)
	}

	portal := strings.TrimSpace(portalFlag)
	if portal == "" && cfg.Portal == "" {
		fmt.Println()
		fmt.Println("请输入贵校校园网认证接口的完整地址。")
		fmt.Println("通常就是你平时登录时浏览器地址栏里那个 login 页面，")
		fmt.Println("例如：https://<认证服务器>/ac_portal/login.php")
		portal = ask("登录接口", "")
	}
	if portal != "" {
		if !strings.HasPrefix(portal, "http://") && !strings.HasPrefix(portal, "https://") {
			fmt.Println("地址必须以 http:// 或 https:// 开头，已退出。")
			return
		}
		cfg.Portal = portal
	}
	if cfg.Portal == "" {
		fmt.Println("必须提供登录接口地址，已退出。")
		return
	}

	user := strings.TrimSpace(userFlag)
	if user == "" {
		user = ask("学号", cfg.User)
		if user == "" {
			user = cfg.User
		}
	}
	if user == "" {
		fmt.Println("必须提供学号，已退出。")
		return
	}

	pw := pwFlag
	if pw == "" {
		fmt.Print("密码（输入时不显示）: ")
		var err error
		pw, err = readHidden(rd)
		if err != nil || pw == "" {
			fmt.Println("密码为空，已退出。")
			return
		}
	}

	if ipFlag != "" {
		cfg.PortalIP = strings.TrimSpace(ipFlag)
	}
	if bindFlag != "" {
		cfg.BindLocalIP = strings.TrimSpace(bindFlag)
	}
	if cfg.BindLocalIP == "" && cfg.PortalIP != "" {
		cfg.BindLocalIP = autoBindIP(cfg.PortalIP)
	}

	cfg.User = user
	if err := saveConfig(dir, cfg); err != nil {
		fmt.Println("保存配置失败:", err)
		return
	}
	if err := writePassword(dir, pw); err != nil {
		fmt.Println("保存密码失败:", err)
		return
	}
	fmt.Println("已保存（密码用 Windows DPAPI 加密，只有本机本用户能解）。")
	if cfg.PortalIP != "" {
		fmt.Println("门户直连 IP:", cfg.PortalIP)
	}
	if cfg.BindLocalIP != "" {
		fmt.Println("绑定源地址:", cfg.BindLocalIP)
	}

	log := &Logger{path: filepath.Join(dir, logFile)}
	timeout := time.Duration(cfg.Timeout) * time.Second
	p := &prober{cfg: cfg, client: newClient(baseTransport(timeout), timeout)}

	fmt.Print("测试登录… ")
	okLogin, msg := login(cfg, cfg.User, pw, timeout)
	if !okLogin {
		fmt.Println("失败 ✗", msg)
		log.Error("setup 测试登录失败: %s", msg)
		fmt.Println()
		fmt.Println("配置已经存下来了，但这次登录没成功。常见原因：")
		fmt.Println("  · 认证地址填错（应该是 login.php 那一层，不是首页）")
		fmt.Println("  · 密码错，或账号被临时锁定")
		fmt.Println("  · 现在连着别的网络，根本到不了校园门户")
		fmt.Println("改好之后重新双击本程序即可再来一次。")
		return
	}
	time.Sleep(2 * time.Second)
	if ok2, d2 := p.probe(); ok2 {
		fmt.Println("成功 ✓", d2)
		log.Info("setup 测试登录成功")
	} else {
		fmt.Println("接口返回:", msg, "但探测仍失败:", d2)
		log.Warn("setup 登录成功但探测失败: %s", d2)
		fmt.Println()
		fmt.Println("接口说登录成功了，但外网还是不通。多半是这台机器走了代理或 VPN，")
		fmt.Println("或者认证服务器只放行特定网段。可以先用 -diag 看看细节。")
		return
	}

	// 配通了就顺手把「开机自启 + 现在开始守护」办掉，
	// 省得用户再去翻别的手册——他双击 exe 就是想让它开始干活。
	fmt.Println()
	fmt.Print("要现在设为开机自启并在后台开始守护吗？[Y/n]: ")
	ans, _ := rd.ReadString('\n')
	ans = strings.ToLower(strings.TrimSpace(ans))
	if ans != "" && ans != "y" && ans != "yes" {
		fmt.Println("好的，没有设自启。之后想补上，可以在命令提示符里运行：")
		fmt.Println("    CampusPortal.exe -install")
		fmt.Println("现在想马上后台跑一次，直接双击本程序即可（配置已经齐了）。")
		return
	}

	if err := installAutostart(dir); err != nil {
		fmt.Println("写开机自启失败:", err)
		fmt.Println("（写入失败不影响下面的启动，下次重启后需要手动跑一次）")
	} else {
		fmt.Println("已设为开机自启。")
	}

	fmt.Println()
	fmt.Println("=== 初始化完成 ===")
	fmt.Println("程序会在后台运行，托盘区能看到一个 Wi-Fi 图标：")
	fmt.Println("  绿色 = 在线    红色 = 已掉线    紫色 = 正在登录")
	fmt.Println("右键那个图标可以「立即连接」、看上次检查时间、打开日志。")
	fmt.Println()
	fmt.Print("按回车键关闭这个窗口… ")
	rd.ReadString('\n')

	// 用 detached 的方式拉起守护进程，这样关掉这个控制台窗口它也不会跟着退出。
	if err := startDetached(); err != nil {
		fmt.Println("启动后台进程失败:", err)
		fmt.Println("可以直接双击本程序再试一次。")
		pauseBeforeExit()
	}
}

// startDetached 以脱离当前控制台的方式启动自己，进入守护模式。
func startDetached() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self)
	cmd.Dir = exeDir()
	// DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP：新进程不继承本控制台，
	// 窗口关掉也不会把它一起带走。
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createNewProcessGroup,
		HideWindow:    true,
	}
	return cmd.Start()
}

// sleepOrWake 睡 sec 秒；期间若托盘点了「立即连接」，立刻返回 true。
func sleepOrWake(wake <-chan struct{}, sec int) (woken bool) {
	if wake == nil {
		time.Sleep(time.Duration(sec) * time.Second)
		return false
	}
	t := time.NewTimer(time.Duration(sec) * time.Second)
	defer t.Stop()
	select {
	case <-t.C:
		return false
	case <-wake:
		return true
	}
}

func daemon(p *prober, cfg Config, pass string, log *Logger, st *status, wake <-chan struct{}) {
	log.Info("守护启动 v%s：账号 %s，间隔 %ds，门户 %s (%s)，源地址 %s",
		VERSION, cfg.User, cfg.Interval, cfg.Portal, cfg.PortalIP, orDash(cfg.BindLocalIP))
	timeout := time.Duration(cfg.Timeout) * time.Second
	fails, authFails := 0, 0
	rounds := 0

	// report 把一次探测结论同时写进日志、状态对象和 portal.state（重启后还能显示上次检查时间）
	report := func(online bool, detail string, writeLog bool) {
		st.set(online, detail)
		saveState(stateFile, st)
		if writeLog && online {
			log.Info("探测正常：%s", detail)
		}
	}

	for {
		// 托盘点了「立即连接」：跳过探测，无条件强制登录一次
		if st.consumeForce() {
			log.Info("收到「立即连接」，强制登录一次")
			st.markBusy("正在登录…")
			okLogin, msg := login(cfg, cfg.User, pass, timeout)
			if !okLogin {
				log.Error("手动登录失败：%s", msg)
				st.set(false, "手动登录失败："+msg)
				saveState(stateFile, st)
			} else {
				sleepOrWake(wake, 2)
				ok3, d3 := p.probe()
				if ok3 {
					log.Info("手动登录成功（%s），网络已恢复", msg)
					st.countLogin()
					fails, authFails = 0, 0
					report(true, d3, false)
				} else {
					log.Warn("手动登录接口返回成功，但探测仍失败（%s）", d3)
					st.set(false, d3)
					saveState(stateFile, st)
				}
			}
			sleepOrWake(wake, cfg.Interval)
			continue
		}

		ok, detail := p.probe()
		rounds++
		if ok {
			if fails > 0 {
				log.Info("网络已恢复正常")
			}
			fails, authFails = 0, 0
			// 心跳：每 8 轮（间隔 10 分钟时约 80 分钟）写一行，证明进程还活着
			report(true, detail, rounds%8 == 1)
			sleepOrWake(wake, cfg.Interval)
			continue
		}

		fails++
		log.Warn("探测失败(#%d)：%s", fails, detail)

		sleepOrWake(wake, 3) // 二次确认，滤掉瞬时抖动
		if ok2, d2 := p.probe(); ok2 {
			log.Info("二次探测通过，判为瞬时抖动，忽略")
			fails = 0
			report(true, d2, false)
			sleepOrWake(wake, cfg.Interval)
			continue
		}

		st.markBusy("正在登录…")
		okLogin, msg := login(cfg, cfg.User, pass, timeout)
		if !okLogin {
			authFails++
			w := backoff(cfg, authFails)
			log.Error("登录失败：%s（%d 秒后重试）", msg, w)
			st.set(false, msg)
			saveState(stateFile, st)
			// 链路抖动（WiFi 断链、漫游）和账号问题要区别对待：
			// 前者往往几十秒内自己就好，用短周期再探一次即可；
			// 后者按指数退避拖长，免得一直拿错密码去撞门户。
			resume := w
			if strings.Contains(msg, "链路抖动") {
				resume = 30
				if w < resume {
					resume = w
				}
			}
			sleepOrWake(wake, resume)
			continue
		}

		sleepOrWake(wake, 2)
		if ok3, d3 := p.probe(); ok3 {
			log.Info("自动登录成功（%s），网络已恢复", msg)
			st.countLogin()
			fails, authFails = 0, 0
			report(true, d3, false)
			sleepOrWake(wake, cfg.Interval)
		} else {
			authFails++
			w := backoff(cfg, authFails)
			log.Warn("登录接口返回成功但探测仍失败（%s），%d 秒后重试", d3, w)
			st.set(false, d3)
			saveState(stateFile, st)
			sleepOrWake(wake, w)
		}
	}
}

// saveState 记下「上次检查时间 / 是否在线」，供下次启动时托盘直接显示。
func saveState(path string, st *status) {
	online, checked, detail, _, _ := st.snapshot()
	if checked.IsZero() {
		return
	}
	// 故意不写 user：这个文件只给托盘显示「上次检查时间」用，
	// 把学号写进去只会多一个可能被误提交的泄露面。
	v := map[string]interface{}{
		"checked": checked.Format("2006-01-02 15:04:05"),
		"online":  online,
		"detail":  detail,
		"version": VERSION,
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o644)
}

// loadState 启动时把上次的检查结果读回来，避免托盘一上来显示「尚未检查」。
func loadState(path string, st *status) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var v struct {
		Checked string `json:"checked"`
		Online  bool   `json:"online"`
		Detail  string `json:"detail"`
	}
	if json.Unmarshal(b, &v) != nil {
		return
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", v.Checked, time.Local)
	if err != nil {
		return
	}
	st.mu.Lock()
	st.online, st.checked, st.detail = v.Online, t, v.Detail
	st.mu.Unlock()
}

func runDiag(p *prober, cfg Config, pass string, force bool) {
	fmt.Println("=== 诊断 ===")
	fmt.Println("版本:    ", VERSION)
	fmt.Println("账号:    ", cfg.User)
	fmt.Println("登录接口:", cfg.Portal)
	fmt.Println("门户 IP: ", orDash(cfg.PortalIP))
	fmt.Println("源地址:  ", orDash(cfg.BindLocalIP))
	fmt.Println("探测目标:", strings.Join(cfg.ProbeURLs, " , "))
	fmt.Printf("探测间隔: %d 秒\n", cfg.Interval)
	fmt.Println("RC4 自检: ", rc4Hex("test", "1234"), "(应为 712c02db)")

	ok, detail := p.probe()
	fmt.Printf("连通性:   %v  %s\n", ok, detail)

	fmt.Print("门户可达 … ")
	if err := pingPortal(cfg); err != nil {
		fmt.Println("不通:", err)
		return
	}
	fmt.Println("OK")

	if ok && !force {
		fmt.Println("→ 当前在线，跳过登录测试（加 -force 可强制测一次）。")
		return
	}
	okLogin, msg := login(cfg, cfg.User, pass, time.Duration(cfg.Timeout)*time.Second)
	fmt.Printf("登录结果: %v  %s\n", okLogin, msg)
	if okLogin {
		time.Sleep(2 * time.Second)
		ok2, d2 := p.probe()
		fmt.Println("复检:    ", ok2, d2)
	}
}

func pingPortal(cfg Config) error {
	timeout := 6 * time.Second
	pu, err := url.Parse(cfg.Portal)
	if err != nil {
		return err
	}
	host := cfg.PortalIP
	if host == "" {
		host = pu.Hostname()
	}
	port := pu.Port()
	if port == "" {
		port = "443"
	}
	bind := cfg.BindLocalIP
	if bind == "" {
		bind = autoBindIP(host)
	}
	d := &net.Dialer{Timeout: timeout, LocalAddr: localAddr(bind)}
	conn, err := d.Dial("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return err
	}
	return conn.Close()
}

func orDash(s string) string {
	if s == "" {
		return "（未设置）"
	}
	return s
}
