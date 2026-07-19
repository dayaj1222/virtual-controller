package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/godbus/dbus/v5"
	"github.com/gorilla/websocket"
	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/sys/unix"
)

// ---- uinput ioctl computation -----------------------------------------------

const (
	iocNone  = 0
	iocWrite = 1

	iocNBits = 8
	iocTBits = 8
	iocSBits = 14
	iocDBits = 2

	iocNShift = 0
	iocTShift = iocNShift + iocNBits
	iocSShift = iocTShift + iocTBits
	iocDShift = iocSShift + iocSBits
)

func ioc(dir, typ, nr, size int) uintptr {
	return uintptr(dir)<<iocDShift | uintptr(typ)<<iocTShift | uintptr(nr)<<iocNShift | uintptr(size)<<iocSShift
}
func iow(typ byte, nr, size int) uintptr { return ioc(iocWrite, int(typ), nr, size) }
// _IO(type, nr) — no data, no direction.
func ioctlIO(typ byte, nr int) uintptr { return ioc(iocNone, int(typ), nr, 0) }

// uinput ioctl numbers (computed at init via the real struct sizes)
var (
	uiDevCreate  uintptr = ioctlIO('U', 1)
	uiDevDestroy uintptr = ioctlIO('U', 2)
	uiDevSetup   uintptr = iow('U', 3, int(unsafe.Sizeof(uinputSetup{})))
	uiAbsSetup   uintptr = iow('U', 4, int(unsafe.Sizeof(uinputAbsSetup{})))
	uiSetEvbit   uintptr = iow('U', 100, int(unsafe.Sizeof(int32(0))))
	uiSetKeybit  uintptr = iow('U', 101, int(unsafe.Sizeof(int32(0))))
	uiSetAbsbit  uintptr = iow('U', 103, int(unsafe.Sizeof(int32(0))))
)

// ---- uinput structs (mirrors <linux/uinput.h>) ------------------------------

type inputID struct {
	Bustype uint16
	Vendor  uint16
	Product uint16
	Version uint16
}

type uinputSetup struct {
	ID           inputID
	Name         [80]byte
	FFEffectsMax uint32
}

type inputAbsinfo struct {
	Value      int32
	Minimum    int32
	Maximum    int32
	Fuzz       int32
	Flat       int32
	Resolution int32
}

type uinputAbsSetup struct {
	Code    uint16
	_       [2]byte // padding
	Absinfo inputAbsinfo
}

type inputEvent struct {
	Sec   int64
	Usec  int64
	Type  uint16
	Code  uint16
	Value int32
}

// ---- event codes (from <linux/input-event-codes.h>) -------------------------

const (
	EvSync = 0x00
	EvKey  = 0x01
	EvAbs  = 0x03
)

const (
	BtnA      = 0x130 // 304
	BtnB      = 0x131 // 305
	BtnX      = 0x133 // 307
	BtnY      = 0x134 // 308
	BtnTL     = 0x136 // 310 left bumper
	BtnTR     = 0x137 // 311 right bumper
	BtnTL2    = 0x138 // 312 left trigger (digital)
	BtnTR2    = 0x139 // 313 right trigger (digital)
	BtnSelect = 0x13a // 314 back
	BtnStart  = 0x13b // 315 start
	BtnMode   = 0x13c // 316 Xbox guide
	BtnThumbL = 0x13d // 317 left stick click
	BtnThumbR = 0x13e // 318 right stick click

	BtnDPadUp    = 0x220 // 544
	BtnDPadDown  = 0x221 // 545
	BtnDPadLeft  = 0x222 // 546
	BtnDPadRight = 0x223 // 547
)

const (
	AbsX     = 0x00
	AbsY     = 0x01
	AbsZ     = 0x02 // left trigger
	AbsRX    = 0x03
	AbsRY    = 0x04
	AbsRZ    = 0x05 // right trigger
	AbsHat0X = 0x10
	AbsHat0Y = 0x11
)

// ---- button/axis definitions ------------------------------------------------

var allButtons = []uint16{
	BtnA, BtnB, BtnX, BtnY,
	BtnTL, BtnTR, BtnTL2, BtnTR2,
	BtnSelect, BtnStart, BtnMode,
	BtnThumbL, BtnThumbR,
	BtnDPadUp, BtnDPadDown, BtnDPadLeft, BtnDPadRight,
}

type axisDef struct {
	Code          uint16
	Min, Max      int32
	Fuzz, Flat    int32
}

// Matches the xpad kernel driver's axis layout.
var allAxes = []axisDef{
	{AbsX, -32768, 32767, 16, 128},
	{AbsY, -32768, 32767, 16, 128},
	{AbsRX, -32768, 32767, 16, 128},
	{AbsRY, -32768, 32767, 16, 128},
	{AbsZ, 0, 1023, 0, 0},
	{AbsRZ, 0, 1023, 0, 0},
	{AbsHat0X, -1, 1, 0, 0},
	{AbsHat0Y, -1, 1, 0, 0},
}

// axisByCode maps axis code → axisDef for fast lookup.
var axisByCode map[uint16]axisDef

func init() {
	axisByCode = make(map[uint16]axisDef, len(allAxes))
	for _, a := range allAxes {
		axisByCode[a.Code] = a
	}
}

// ---- WebSocket protocol -----------------------------------------------------

var keyCodeMap = map[string]uint16{
	"A":          BtnA,
	"B":          BtnB,
	"X":          BtnX,
	"Y":          BtnY,
	"LB":         BtnTL,
	"RB":         BtnTR,
	"LT":         BtnTL2,
	"RT":         BtnTR2,
	"BACK":       BtnSelect,
	"START":      BtnStart,
	"GUIDE":      BtnMode,
	"THUMBL":     BtnThumbL,
	"THUMBR":     BtnThumbR,
	"DPAD_UP":    BtnDPadUp,
	"DPAD_DOWN":  BtnDPadDown,
	"DPAD_LEFT":  BtnDPadLeft,
	"DPAD_RIGHT": BtnDPadRight,
}

var absCodeMap = map[string]uint16{
	"LX":   AbsX,
	"LY":   AbsY,
	"RX":   AbsRX,
	"RY":   AbsRY,
	"LT":   AbsZ,
	"RT":   AbsRZ,
	"HATX": AbsHat0X,
	"HATY": AbsHat0Y,
}

// Incoming JSON from the phone client.
type rawMsg struct {
	Type  string          `json:"type"`
	Code  string          `json:"code"`
	Value json.RawMessage `json:"value"`
}

// Internal event queued for the writer goroutine.
type uinputEvent struct {
	Type  uint16
	Code  uint16
	Value int32
}

// ---- uinput low-level helpers ------------------------------------------------

func ioctl(fd int, req uintptr, arg uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, arg); errno != 0 {
		return errno
	}
	return nil
}

func writeRaw(fd int, ev inputEvent) {
	syscall.Syscall(syscall.SYS_WRITE, uintptr(fd), uintptr(unsafe.Pointer(&ev)), unsafe.Sizeof(ev))
}

// ---- uinput device lifecycle ------------------------------------------------

// createUinput opens /dev/uinput, configures a virtual Xbox controller,
// and returns the fd (or -1 on error).
func createUinput() (int, error) {
	fd, err := unix.Open("/dev/uinput", unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open /dev/uinput: %w", err)
	}

	// Enable event types: keys + absolute axes
	for _, bit := range []int{EvKey, EvAbs} {
		if err := ioctl(fd, uiSetEvbit, uintptr(bit)); err != nil {
			unix.Close(fd)
			return -1, fmt.Errorf("set evbit %d: %w", bit, err)
		}
	}

	// Register all buttons
	for _, btn := range allButtons {
		if err := ioctl(fd, uiSetKeybit, uintptr(btn)); err != nil {
			unix.Close(fd)
			return -1, fmt.Errorf("set keybit 0x%x: %w", btn, err)
		}
	}

	// Register all axes + configure their ranges
	for _, ax := range allAxes {
		if err := ioctl(fd, uiSetAbsbit, uintptr(ax.Code)); err != nil {
			unix.Close(fd)
			return -1, fmt.Errorf("set absbit 0x%x: %w", ax.Code, err)
		}
		absSetup := uinputAbsSetup{
			Code: ax.Code,
			Absinfo: inputAbsinfo{
				Minimum: ax.Min,
				Maximum: ax.Max,
				Fuzz:    ax.Fuzz,
				Flat:    ax.Flat,
			},
		}
		if err := ioctl(fd, uiAbsSetup, uintptr(unsafe.Pointer(&absSetup))); err != nil {
			unix.Close(fd)
			return -1, fmt.Errorf("set absinfo 0x%x: %w", ax.Code, err)
		}
	}

	// Device identity — mimic Xbox 360 controller so Wine/games recognise it.
	var name [80]byte
	copy(name[:], []byte("Xbox 360 Controller"))
	setup := uinputSetup{
		ID: inputID{
			Bustype: unix.BUS_USB,
			Vendor:  0x045e, // Microsoft
			Product: 0x028e, // Xbox 360 wired
			Version: 0x0110,
		},
		Name: name,
	}
	if err := ioctl(fd, uiDevSetup, uintptr(unsafe.Pointer(&setup))); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("dev setup: %w", err)
	}

	// Create the virtual device — /dev/input/eventN appears now.
	if err := ioctl(fd, uiDevCreate, 0); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("dev create: %w", err)
	}

	// Give udev a moment to create device nodes.
	time.Sleep(50 * time.Millisecond)

	return fd, nil
}

// destroyUinput releases all inputs and destroys the virtual device.
func destroyUinput(fd int) {
	if fd < 0 {
		return
	}
	// Release everything
	reset := []inputEvent{
		{Type: EvKey, Code: BtnA, Value: 0},
		{Type: EvKey, Code: BtnB, Value: 0},
		{Type: EvKey, Code: BtnX, Value: 0},
		{Type: EvKey, Code: BtnY, Value: 0},
		{Type: EvKey, Code: BtnTL, Value: 0},
		{Type: EvKey, Code: BtnTR, Value: 0},
		{Type: EvKey, Code: BtnSelect, Value: 0},
		{Type: EvKey, Code: BtnStart, Value: 0},
		{Type: EvKey, Code: BtnMode, Value: 0},
		{Type: EvKey, Code: BtnThumbL, Value: 0},
		{Type: EvKey, Code: BtnThumbR, Value: 0},
		{Type: EvKey, Code: BtnDPadUp, Value: 0},
		{Type: EvKey, Code: BtnDPadDown, Value: 0},
		{Type: EvKey, Code: BtnDPadLeft, Value: 0},
		{Type: EvKey, Code: BtnDPadRight, Value: 0},
		{Type: EvAbs, Code: AbsX, Value: 0},
		{Type: EvAbs, Code: AbsY, Value: 0},
		{Type: EvAbs, Code: AbsRX, Value: 0},
		{Type: EvAbs, Code: AbsRY, Value: 0},
		{Type: EvAbs, Code: AbsZ, Value: 0},
		{Type: EvAbs, Code: AbsRZ, Value: 0},
		{Type: EvAbs, Code: AbsHat0X, Value: 0},
		{Type: EvAbs, Code: AbsHat0Y, Value: 0},
	}
	for _, ev := range reset {
		writeRaw(fd, ev)
	}
	writeRaw(fd, inputEvent{Type: EvSync, Code: 0, Value: 0})
	ioctl(fd, uiDevDestroy, 0)
	unix.Close(fd)
}

// ---- Ordered event writer ---------------------------------------------------
// Single goroutine, single channel — total ordering guaranteed.
// Buttons flush the batch immediately (low latency).  Analog events batch
// until the ticker fires (~8 ms).

type orderedWriter struct {
	fd      int
	events  chan uinputEvent
	closeCh chan struct{}
	wg      sync.WaitGroup
	interval time.Duration
}

func newOrderedWriter(fd int, interval time.Duration) *orderedWriter {
	ow := &orderedWriter{
		fd:       fd,
		events:   make(chan uinputEvent, 256),
		closeCh:  make(chan struct{}),
		interval: interval,
	}
	ow.wg.Add(1)
	go ow.loop()
	return ow
}

func (ow *orderedWriter) push(ev uinputEvent) {
	select {
	case ow.events <- ev:
	default:
	}
}

func (ow *orderedWriter) close() {
	close(ow.closeCh)
	ow.wg.Wait()
}

func (ow *orderedWriter) loop() {
	defer ow.wg.Done()

	syncEv := inputEvent{Type: EvSync, Code: 0, Value: 0}
	buf := make([]inputEvent, 0, 64)
	ticker := time.NewTicker(ow.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ow.closeCh:
			if len(buf) > 0 {
				ow.flush(buf, syncEv)
			}
			return

		case ev := <-ow.events:
			// EvSync marker = immediate flush
			if ev.Type == EvSync && ev.Code == 0 && ev.Value == 0 {
				if len(buf) > 0 {
					ow.flush(buf, syncEv)
					buf = buf[:0]
				}
				continue
			}
			buf = append(buf, inputEvent{
				Type: ev.Type, Code: ev.Code, Value: ev.Value,
			})
			// Buttons flush immediately for lowest latency
			if ev.Type == EvKey || len(buf) >= 64 {
				ow.flush(buf, syncEv)
				buf = buf[:0]
			}

		case <-ticker.C:
			if len(buf) > 0 {
				ow.flush(buf, syncEv)
				buf = buf[:0]
			}
		}
	}
}

func (ow *orderedWriter) flush(buf []inputEvent, syncEv inputEvent) {
	for _, e := range buf {
		writeRaw(ow.fd, e)
	}
	writeRaw(ow.fd, syncEv)
}

// ---- WebSocket handler ------------------------------------------------------

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade: %v", err)
		return
	}
	defer conn.Close()

	// Create a virtual controller for this connection.
	fd, err := createUinput()
	if err != nil {
		log.Printf("create uinput: %v", err)
		conn.WriteMessage(websocket.TextMessage, []byte(`{"error":"`+err.Error()+`"}`))
		return
	}
	log.Printf("virtual controller created (fd=%d)", fd)

	// Start the ordered writer.
	ow := newOrderedWriter(fd, appCfg.FlushInterval)
	defer ow.close()
	defer destroyUinput(fd)
	defer log.Printf("virtual controller destroyed (fd=%d)", fd)

	// Confirm connection.
	conn.WriteMessage(websocket.TextMessage,
		[]byte(`{"status":"connected","vendor":"Microsoft","product":"Xbox 360 Controller"}`))

	// Read messages and push to the ordered writer.
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var m rawMsg
		if err := json.Unmarshal(data, &m); err != nil {
			log.Printf("bad json: %v", err)
			continue
		}

		switch m.Type {
		case "key":
			code, ok := keyCodeMap[m.Code]
			if !ok {
				log.Printf("unknown key: %s", m.Code)
				continue
			}
			var val int
			if err := json.Unmarshal(m.Value, &val); err != nil {
				log.Printf("bad key value: %v", err)
				continue
			}
			v := int32(0)
			if val > 0 {
				v = 1
			}
			ow.push(uinputEvent{Type: EvKey, Code: code, Value: v})
			lastInput.Store(time.Now().UnixNano())

		case "abs":
			code, ok := absCodeMap[m.Code]
			if !ok {
				log.Printf("unknown axis: %s", m.Code)
				continue
			}
			var fval float64
			if err := json.Unmarshal(m.Value, &fval); err != nil {
				log.Printf("bad abs value: %v", err)
				continue
			}
			// Deadzone for sticks
			isStick := m.Code == "LX" || m.Code == "LY" || m.Code == "RX" || m.Code == "RY"
			if isStick && fval >= -1 && fval <= 1 {
				if fval < 0 && fval > -appCfg.StickDeadzone {
					fval = 0
				} else if fval >= 0 && fval < appCfg.StickDeadzone {
					fval = 0
				}
			}
			var v int32
			if def, ok := axisByCode[code]; ok {
				if fval >= -1 && fval <= 1 && fval != 0 {
					rng := float64(def.Max) - float64(def.Min)
					v = int32(float64(def.Min) + (fval+1)/2*rng)
				} else {
					v = int32(fval)
				}
				if v < def.Min {
					v = def.Min
				}
				if v > def.Max {
					v = def.Max
				}
			}
			ow.push(uinputEvent{Type: EvAbs, Code: code, Value: v})
			lastInput.Store(time.Now().UnixNano())

		case "sync":
			ow.push(uinputEvent{Type: EvSync, Code: 0, Value: 0})
		}
	}
}

// ---- HTTP config API -------------------------------------------------------

func configHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}

	if r.Method == "GET" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"listen":           appCfg.ListenAddr,
			"flush_interval_ms": appCfg.FlushInterval.Milliseconds(),
			"stick_deadzone":   appCfg.StickDeadzone,
			"trigger_threshold": appCfg.TriggerMax,
		})
		return
	}

	if r.Method == "PUT" {
		var updates map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
			http.Error(w, `{"error":"bad json"}`, 400)
			return
		}

		// Read current .env, apply changes, write back.
		lines := readEnvLines(".env")
		for k, v := range updates {
			switch k {
			case "stick_deadzone":
				if f, ok := v.(float64); ok {
					appCfg.StickDeadzone = f
					upsertEnv(&lines, "STICK_DEADZONE", fmt.Sprintf("%.2f", f))
				}
			case "trigger_threshold":
				if f, ok := v.(float64); ok {
					appCfg.TriggerMax = f
					upsertEnv(&lines, "TRIGGER_THRESHOLD", fmt.Sprintf("%.2f", f))
				}
			case "flush_interval_ms":
				if f, ok := v.(float64); ok {
					appCfg.FlushInterval = time.Duration(f) * time.Millisecond
					upsertEnv(&lines, "FLUSH_INTERVAL_MS", fmt.Sprintf("%.0f", f))
				}
			case "listen":
				if s, ok := v.(string); ok {
					upsertEnv(&lines, "LISTEN", s)
					// Note: listen address requires restart
				}
			}
		}
		writeEnvLines(".env", lines)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "saved"})
		return
	}

	http.Error(w, `{"error":"method not allowed"}`, 405)
}

func readEnvLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(string(data), "\n")
}

func writeEnvLines(path string, lines []string) {
	os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644)
}

// upsertEnv replaces an existing KEY=value line or appends it.
func upsertEnv(lines *[]string, key, value string) {
	found := false
	for i, line := range *lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, key+"=") {
			(*lines)[i] = key + "=" + value
			found = true
			break
		}
	}
	if !found {
		*lines = append(*lines, key+"="+value)
	}
}

// ---- Layout API ------------------------------------------------------------

var layoutPath = "layout.json"

func layoutHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, PUT, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == "OPTIONS" { w.WriteHeader(204); return }

	if r.Method == "GET" {
		data, err := os.ReadFile(layoutPath)
		if err != nil {
			w.Write([]byte("{}"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
		return
	}

	if r.Method == "PUT" {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, `{"error":"read"}`, 400)
			return
		}
		if err := os.WriteFile(layoutPath, body, 0644); err != nil {
			http.Error(w, `{"error":"write"}`, 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status":"saved"})
		return
	}

	http.Error(w, `{"error":"method not allowed"}`, 405)
}

// ---- Sleep inhibitor -------------------------------------------------------

var lastInput atomic.Int64

func startInhibitor() {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		log.Printf("dbus: %v (idle inhibit disabled)", err)
		return
	}
	obj := conn.Object("org.freedesktop.ScreenSaver", "/org/freedesktop/ScreenSaver")

	go func() {
		for {
			time.Sleep(30 * time.Second)
			ns := lastInput.Load()
			if ns > 0 && time.Since(time.Unix(0, ns)) < 35*time.Second {
				obj.Call("org.freedesktop.ScreenSaver.SimulateUserActivity", 0)
			}
		}
	}()
	log.Print("dbus idle inhibitor started")
}

// ---- Startup helpers -------------------------------------------------------

func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return ""
}

// ---- main -------------------------------------------------------------------

var appCfg config

func main() {
	appCfg = loadConfig()
	startInhibitor()

	// Print a QR code with the LAN URL so the user can scan from their phone.
	if ip := getLocalIP(); ip != "" {
		url := fmt.Sprintf("http://%s%s", ip, appCfg.ListenAddr)
		qr, err := qrcode.New(url, qrcode.Medium)
		if err == nil {
			log.Printf("📱 Scan from your phone: %s", url)
			fmt.Println(qr.ToSmallString(false))
		} else {
			log.Printf("📱 Phone client: %s", url)
		}
	}

	addr := appCfg.ListenAddr

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler)
	mux.HandleFunc("/api/config", configHandler)
	mux.HandleFunc("/api/layout", layoutHandler)
	mux.Handle("/", http.FileServer(http.Dir("client")))

	server := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-sigCh
		log.Println("shutting down...")
		server.Close()
	}()

	log.Printf("🚀 controller-server listening on %s", addr)
	log.Printf("   Phone client:  ws://<your-pc-ip>%s/ws", addr)
	log.Printf("   Web client:    http://localhost%s", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
}
