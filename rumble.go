package main

// Force feedback (rumble) support for the virtual controller.
//
// Games upload FF_RUMBLE effects to the uinput device through the kernel.
// The kernel reports each upload/erase request to us as an EV_UINPUT event on
// the uinput fd, and we must answer it with the matching UI_*_FF_* ioctls.
// When a game plays an effect, the kernel reports an EV_FF event on the same fd
// (code = effect id, value = repeat count; 0 means stop).

import (
	"encoding/binary"
	"log"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Event and effect codes (<linux/input-event-codes.h>, <linux/uinput.h>).
const (
	EvUinput     = 0x0101 // EV_UINPUT: kernel -> userspace requests
	UIFFUpload   = 1      // UI_FF_UPLOAD
	UIFFErase    = 2      // UI_FF_ERASE
	EvFF         = 0x15   // EV_FF: effect play/stop
	FFRumble     = 0x50   // FF_RUMBLE
	ffMaxEffects = 16
)

const (
	iocReadDir = 2
	uiSetFFBit = 107 // UI_SET_FFBIT
)

// ioctl numbers for force feedback (sizes must match the structs below).
var (
	uiSetFFBitReq   = iow('U', uiSetFFBit, int(unsafe.Sizeof(int32(0))))
	uiBeginFFUpload = ioc(iocReadDir|iocWrite, int('U'), 200, int(unsafe.Sizeof(uinputFFUpload{})))
	uiEndFFUpload   = iow('U', 201, int(unsafe.Sizeof(uinputFFUpload{})))
	uiBeginFFErase  = ioc(iocReadDir|iocWrite, int('U'), 202, int(unsafe.Sizeof(uinputFFErase{})))
	uiEndFFErase    = iow('U', 203, int(unsafe.Sizeof(uinputFFErase{})))
)

// ffEffect mirrors struct ff_effect (48 bytes on 64-bit Linux). Only the
// header and the rumble part of the union are used; the rest is opaque.
type ffEffect struct {
	Type      uint16
	ID        int16
	Direction uint16
	Trigger   [2]uint16 // button, interval
	Replay    [2]uint16 // length (ms), delay (ms)
	_         [2]byte   // padding to the 8-byte-aligned union
	Union     [32]byte  // struct ff_rumble_effect is the first 4 bytes
}

// uinputFFUpload mirrors struct uinput_ff_upload (104 bytes).
type uinputFFUpload struct {
	RequestID uint32
	Retval    int32
	Effect    ffEffect
	Old       ffEffect
}

// uinputFFErase mirrors struct uinput_ff_erase (12 bytes).
type uinputFFErase struct {
	RequestID uint32
	Retval    int32
	EffectID  uint32
}

// rumbleMsg is sent to the phone. Strong/weak are motor magnitudes 0..1.
// Duration is in ms (0 = unspecified). Zero magnitudes mean stop.
type rumbleMsg struct {
	Type     string  `json:"type"`
	Strong   float64 `json:"strong"`
	Weak     float64 `json:"weak"`
	Duration int     `json:"duration"`
}

type rumbleEffect struct {
	strong, weak uint16
	length       uint16
}

// rumbler reads force-feedback requests from the uinput fd and forwards
// rumble to the phone via send.
type rumbler struct {
	fd      int
	send    func(any)
	mu      sync.Mutex
	effects map[uint16]rumbleEffect
	done    chan struct{}
	wg      sync.WaitGroup
}

// startRumbler starts the reader goroutine. Call stop before destroying fd.
func startRumbler(fd int, send func(any)) *rumbler {
	r := &rumbler{
		fd:      fd,
		send:    send,
		effects: make(map[uint16]rumbleEffect),
		done:    make(chan struct{}),
	}
	r.wg.Add(1)
	go r.loop()
	return r
}

func (r *rumbler) stop() {
	close(r.done)
	r.wg.Wait()
}

func (r *rumbler) loop() {
	defer r.wg.Done()
	pfd := []unix.PollFd{{Fd: int32(r.fd), Events: unix.POLLIN}}
	var ev inputEvent
	buf := unsafe.Slice((*byte)(unsafe.Pointer(&ev)), unsafe.Sizeof(ev))
	for {
		select {
		case <-r.done:
			return
		default:
		}
		// Poll with a timeout so stop() is noticed promptly.
		n, err := unix.Poll(pfd, 200)
		if err != nil || n == 0 {
			if err != nil && err != unix.EINTR {
				log.Printf("rumble poll: %v", err)
				return
			}
			continue
		}
		n, err = unix.Read(r.fd, buf)
		if err != nil || n != len(buf) {
			continue
		}

		switch {
		case ev.Type == EvUinput && ev.Code == UIFFUpload:
			r.handleUpload()
		case ev.Type == EvUinput && ev.Code == UIFFErase:
			r.handleErase()
		case ev.Type == EvFF:
			r.handlePlay(ev.Code, ev.Value)
		}
	}
}

func (r *rumbler) handleUpload() {
	var up uinputFFUpload
	if err := ioctl(r.fd, uiBeginFFUpload, uintptr(unsafe.Pointer(&up))); err != nil {
		log.Printf("ff upload begin: %v", err)
		return
	}
	up.Retval = 0
	if up.Effect.Type != FFRumble {
		// Only rumble is advertised; reject anything else.
		up.Retval = -int32(unix.EINVAL)
	} else {
		r.mu.Lock()
		r.effects[uint16(up.Effect.ID)] = rumbleEffect{
			strong: binary.LittleEndian.Uint16(up.Effect.Union[0:2]),
			weak:   binary.LittleEndian.Uint16(up.Effect.Union[2:4]),
			length: up.Effect.Replay[0],
		}
		r.mu.Unlock()
	}
	if err := ioctl(r.fd, uiEndFFUpload, uintptr(unsafe.Pointer(&up))); err != nil {
		log.Printf("ff upload end: %v", err)
	}
}

func (r *rumbler) handleErase() {
	var er uinputFFErase
	if err := ioctl(r.fd, uiBeginFFErase, uintptr(unsafe.Pointer(&er))); err != nil {
		log.Printf("ff erase begin: %v", err)
		return
	}
	r.mu.Lock()
	delete(r.effects, uint16(er.EffectID))
	r.mu.Unlock()
	er.Retval = 0
	if err := ioctl(r.fd, uiEndFFErase, uintptr(unsafe.Pointer(&er))); err != nil {
		log.Printf("ff erase end: %v", err)
	}
}

func (r *rumbler) handlePlay(id uint16, count int32) {
	if count <= 0 {
		// Stop: the game released the effect.
		r.send(rumbleMsg{Type: "rumble"})
		return
	}
	r.mu.Lock()
	e, ok := r.effects[id]
	r.mu.Unlock()
	if !ok {
		return
	}
	r.send(rumbleMsg{
		Type:     "rumble",
		Strong:   float64(e.strong) / 65535,
		Weak:     float64(e.weak) / 65535,
		Duration: int(e.length),
	})
}
