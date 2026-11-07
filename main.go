package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
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

// Linux keyboard key codes (<linux/input-event-codes.h>)
const (
	KeyF12 = 88
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
	Code       uint16
	Min, Max   int32
	Fuzz, Flat int32
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

// axisValue converts a client value into the kernel range for def.
// Sticks and hat axes arrive as -1..1 and are centred on zero; triggers arrive
// as 0..1 and map straight onto 0..Max (0.5 is half-travel, not 75%).
func axisValue(def axisDef, fval float64) int32 {
	fval = math.Max(-1, math.Min(1, fval))
	var f float64
	if def.Min == 0 {
		f = fval * float64(def.Max)
	} else {
		f = float64(def.Min) + (fval+1)/2*(float64(def.Max)-float64(def.Min))
	}
	return int32(math.Max(float64(def.Min), math.Min(float64(def.Max), f)))
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

// kbKeyCodeMap maps client hotkey names → Linux keyboard key codes.
var kbKeyCodeMap = map[string]uint16{
	"F12": KeyF12,
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
	// Release every button and centre every axis, driven by the same tables
	// used at creation so the lists can't drift apart.
	for _, btn := range allButtons {
		writeRaw(fd, inputEvent{Type: EvKey, Code: btn, Value: 0})
	}
	for _, ax := range allAxes {
		writeRaw(fd, inputEvent{Type: EvAbs, Code: ax.Code, Value: 0})
	}
	writeRaw(fd, inputEvent{Type: EvSync, Code: 0, Value: 0})
	ioctl(fd, uiDevDestroy, 0)
	unix.Close(fd)
}

// createKeyboard opens a second uinput device configured as a keyboard,
// so hotkey presses (e.g. F12) go to a real keyboard device instead of
// polluting the gamepad device. Returns the fd (or -1 on error).
func createKeyboard() (int, error) {
	fd, err := unix.Open("/dev/uinput", unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open /dev/uinput (kb): %w", err)
	}

	// Enable key events only
	if err := ioctl(fd, uiSetEvbit, uintptr(EvKey)); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("set evbit (kb): %w", err)
	}

	// Register the F12 key
	if err := ioctl(fd, uiSetKeybit, uintptr(KeyF12)); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("set keybit F12: %w", err)
	}

	var name [80]byte
	copy(name[:], []byte("Virtual Hotkey Keyboard"))
	setup := uinputSetup{
		ID: inputID{
			Bustype: unix.BUS_USB,
			Vendor:  0x045e,
			Product: 0x0001,
			Version: 0x0110,
		},
		Name: name,
	}
	if err := ioctl(fd, uiDevSetup, uintptr(unsafe.Pointer(&setup))); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("dev setup (kb): %w", err)
	}

	if err := ioctl(fd, uiDevCreate, 0); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("dev create (kb): %w", err)
	}

	time.Sleep(50 * time.Millisecond)
	return fd, nil
}

// ---- Ordered event writer ---------------------------------------------------
// Single goroutine, single channel — total ordering guaranteed.
// Buttons flush the batch immediately (low latency).  Analog events batch
// until the ticker fires (~8 ms).

type orderedWriter struct {
	fd       int
	events   chan uinputEvent
	closeCh  chan struct{}
	wg       sync.WaitGroup
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

// push blocks rather than dropping: a lost key-up would leave the button held
// down on the PC. Only the read loop pushes, and it stops before close().
func (ow *orderedWriter) push(ev uinputEvent) {
	ow.events <- ev
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

	// Create a virtual keyboard for hotkeys (F12 etc).
	kbfd, kerr := createKeyboard()
	if kerr != nil {
		log.Printf("create keyboard: %v", kerr)
	}
	defer func() {
		if kbfd >= 0 {
			ioctl(kbfd, uiDevDestroy, 0)
			unix.Close(kbfd)
		}
	}()

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
			// Triggers: TRIGGER_THRESHOLD sets how far the finger must travel for a full press.
			if code == AbsZ || code == AbsRZ {
				fval = math.Min(1, fval/appCfg.TriggerMax)
			}
			def, ok := axisByCode[code]
			if !ok {
				continue
			}
			ow.push(uinputEvent{Type: EvAbs, Code: code, Value: axisValue(def, fval)})
			lastInput.Store(time.Now().UnixNano())

		case "kbkey":
			// Keyboard hotkey (e.g. F12). Value: 1=press, 0=release.
			if kbfd < 0 {
				log.Printf("kbkey: no keyboard device")
				continue
			}
			code, ok := kbKeyCodeMap[m.Code]
			if !ok {
				log.Printf("unknown kbkey: %s", m.Code)
				continue
			}
			var val int
			if err := json.Unmarshal(m.Value, &val); err != nil {
				log.Printf("bad kbkey value: %v", err)
				continue
			}
			v := int32(0)
			if val > 0 {
				v = 1
			}
			writeRaw(kbfd, inputEvent{Type: EvKey, Code: code, Value: v})
			writeRaw(kbfd, inputEvent{Type: EvSync, Code: 0, Value: 0})
			lastInput.Store(time.Now().UnixNano())

		case "sync":
			ow.push(uinputEvent{Type: EvSync, Code: 0, Value: 0})

		case "ping":
			// Reply so the client can measure round-trip latency.
			conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"pong"}`))
		}
	}
}

// ---- HTTP config API -------------------------------------------------------

func configHandler(w http.ResponseWriter, r *http.Request) {
	// Same-origin only: no CORS headers, so other sites can't read or write config.
	if r.Method == "GET" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"listen":            appCfg.ListenAddr,
			"flush_interval_ms": appCfg.FlushInterval.Milliseconds(),
			"stick_deadzone":    appCfg.StickDeadzone,
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
					appCfg.StickDeadzone = clampDeadzone(f)
					upsertEnv(&lines, "STICK_DEADZONE", fmt.Sprintf("%.2f", appCfg.StickDeadzone))
				}
			case "trigger_threshold":
				if f, ok := v.(float64); ok {
					appCfg.TriggerMax = clampTrigger(f)
					upsertEnv(&lines, "TRIGGER_THRESHOLD", fmt.Sprintf("%.2f", appCfg.TriggerMax))
				}
			case "flush_interval_ms":
				if f, ok := v.(float64); ok {
					// Read by each new connection's writer; a zero here would panic the ticker.
					appCfg.FlushInterval = clampFlush(time.Duration(f) * time.Millisecond)
					upsertEnv(&lines, "FLUSH_INTERVAL_MS", fmt.Sprintf("%d", appCfg.FlushInterval.Milliseconds()))
				}
			case "listen":
				if s, ok := v.(string); ok && s != "" {
					upsertEnv(&lines, "LISTEN", s)
					// Note: listen address requires restart
				}
			}
		}
		if err := writeEnvLines(".env", lines); err != nil {
			log.Printf("config: save .env: %v", err)
			http.Error(w, `{"error":"write"}`, 500)
			return
		}

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

// writeEnvLines holds the auth token, so the file stays owner-only.
func writeEnvLines(path string, lines []string) error {
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0600)
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

const maxLayoutBytes = 64 << 10

func layoutHandler(w http.ResponseWriter, r *http.Request) {
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
		body, err := io.ReadAll(io.LimitReader(r.Body, maxLayoutBytes+1))
		if err != nil || len(body) > maxLayoutBytes {
			http.Error(w, `{"error":"read or too large"}`, 400)
			return
		}
		// Must be a JSON object; the client reads it as a map of control positions.
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(body, &probe); err != nil || probe == nil {
			http.Error(w, `{"error":"layout must be a JSON object"}`, 400)
			return
		}
		if err := os.WriteFile(layoutPath, body, 0644); err != nil {
			http.Error(w, `{"error":"write"}`, 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "saved"})
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

// ---- Pairing token ---------------------------------------------------------
// Anyone on the LAN could otherwise inject input or rewrite config. Every
// endpoint requires the token: the QR code and startup URL carry it as ?token=.

var authToken string

// ensureAuthToken returns the configured AUTH_TOKEN, or generates one and
// stores it in .env on first run so the QR code stays valid across restarts.
func ensureAuthToken() string {
	if appCfg.AuthToken != "" {
		return appCfg.AuthToken
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("auth token: %v", err)
	}
	tok := hex.EncodeToString(b)
	lines := readEnvLines(".env")
	upsertEnv(&lines, "AUTH_TOKEN", tok)
	if err := writeEnvLines(".env", lines); err != nil {
		log.Fatalf("auth token: save .env: %v", err)
	}
	log.Printf("generated AUTH_TOKEN and saved it to .env")
	return tok
}

func authorized(r *http.Request) bool {
	got := r.URL.Query().Get("token")
	if got == "" {
		got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(authToken)) == 1
}

func requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
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

// lanHostPort joins the LAN IP with the port from LISTEN. LISTEN may be ":9449"
// or a host:port such as "127.0.0.1:9449", so only the port is kept.
func lanHostPort(ip, listen string) string {
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		return ip + listen[i:]
	}
	return ip + ":" + listen
}

// noCache wraps an http.Handler to prevent caching of the served files.
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		h.ServeHTTP(w, r)
	})
}

// ---- main -------------------------------------------------------------------

var appCfg config

// version is set at release time with -ldflags "-X main.version=vX.Y.Z".
var version = "dev"

func main() {
	appCfg = loadConfig()
	authToken = ensureAuthToken()
	startInhibitor()
	log.Printf("controller-server %s", version)

	addr := appCfg.ListenAddr

	// Print a QR code with the LAN URL (token included) so the user can scan from their phone.
	if ip := getLocalIP(); ip != "" {
		url := fmt.Sprintf("http://%s/?token=%s", lanHostPort(ip, addr), authToken)
		qr, err := qrcode.New(url, qrcode.Medium)
		if err == nil {
			log.Printf("📱 Scan from your phone: %s", url)
			fmt.Println(qr.ToSmallString(false))
		} else {
			log.Printf("📱 Phone client: %s", url)
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", requireAuth(wsHandler))
	mux.HandleFunc("/api/config", requireAuth(configHandler))
	mux.HandleFunc("/api/layout", requireAuth(layoutHandler))
	mux.Handle("/", noCache(http.FileServer(http.Dir("client"))))

	server := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-sigCh
		log.Println("shutting down...")
		server.Close()
	}()

	log.Printf("🚀 controller-server listening on %s", addr)
	log.Printf("   Phone client:  ws://%s/ws?token=%s", lanHostPort(getLocalIP(), addr), authToken)
	log.Printf("   Web client:    http://localhost%s", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
}
