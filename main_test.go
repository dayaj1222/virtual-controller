package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func TestAxisValueTriggers(t *testing.T) {
	lt := axisByCode[AbsZ]
	cases := map[float64]int32{0: 0, 0.5: 511, 1: 1023, 2: 1023}
	for in, want := range cases {
		if got := axisValue(lt, in); got != want {
			t.Errorf("trigger %v: got %d want %d", in, got, want)
		}
	}
}

func TestAxisValueSticks(t *testing.T) {
	ls := axisByCode[AbsX]
	cases := map[float64]int32{-1: -32768, 0: 0, 1: 32767, -2: -32768}
	for in, want := range cases {
		if got := axisValue(ls, in); got != want {
			t.Errorf("stick %v: got %d want %d", in, got, want)
		}
	}
	hat := axisByCode[AbsHat0X]
	if got := axisValue(hat, 1); got != 1 {
		t.Errorf("hat +1: got %d", got)
	}
	if got := axisValue(hat, -1); got != -1 {
		t.Errorf("hat -1: got %d", got)
	}
}

func TestClampConfig(t *testing.T) {
	if got := clampFlush(0); got != time.Millisecond {
		t.Errorf("flush 0 clamped to %v, want 1ms (ticker panics on 0)", got)
	}
	if got := clampFlush(time.Second); got != maxFlushMs*time.Millisecond {
		t.Errorf("flush 1s clamped to %v", got)
	}
	if got := clampDeadzone(9); got != maxDeadzone {
		t.Errorf("deadzone clamp: %v", got)
	}
	if got := clampTrigger(0); got != minTriggerMax {
		t.Errorf("trigger clamp low: %v", got)
	}
}

func TestUpsertEnv(t *testing.T) {
	lines := []string{"LISTEN=:1", "STICK_DEADZONE=0.1"}
	upsertEnv(&lines, "STICK_DEADZONE", "0.2")
	upsertEnv(&lines, "AUTH_TOKEN", "abc")
	got := strings.Join(lines, "|")
	want := "LISTEN=:1|STICK_DEADZONE=0.2|AUTH_TOKEN=abc"
	if got != want {
		t.Errorf("upsert: got %q want %q", got, want)
	}
}

func TestAuthorized(t *testing.T) {
	authToken = "secret123"
	cases := []struct {
		url, header string
		want        bool
	}{
		{"/ws?token=secret123", "", true},
		{"/ws", "Bearer secret123", true},
		{"/ws?token=wrong", "", false},
		{"/ws", "", false},
		{"/ws?token=", "", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, c.url, nil)
		if c.header != "" {
			r.Header.Set("Authorization", c.header)
		}
		if got := authorized(r); got != c.want {
			t.Errorf("%s header=%q: got %v want %v", c.url, c.header, got, c.want)
		}
	}
}

func TestRequireAuthBlocksUnauthenticated(t *testing.T) {
	authToken = "secret123"
	called := false
	h := requireAuth(func(w http.ResponseWriter, r *http.Request) { called = true })
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if w.Code != http.StatusUnauthorized || called {
		t.Errorf("unauthenticated request: code=%d called=%v", w.Code, called)
	}
}

func TestLayoutRejectsNonObject(t *testing.T) {
	dir := t.TempDir()
	layoutPath = filepath.Join(dir, "layout.json")
	defer func() { layoutPath = "layout.json" }()

	for _, body := range []string{"null", "[1,2]", "not json", ""} {
		r := httptest.NewRequest(http.MethodPut, "/api/layout", strings.NewReader(body))
		w := httptest.NewRecorder()
		layoutHandler(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q: code %d, want 400", body, w.Code)
		}
	}
	if _, err := os.Stat(layoutPath); !os.IsNotExist(err) {
		t.Errorf("invalid layout was written to disk")
	}

	r := httptest.NewRequest(http.MethodPut, "/api/layout", strings.NewReader(`{"lb":{"x":1,"y":2,"w":3,"h":4}}`))
	w := httptest.NewRecorder()
	layoutHandler(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("valid layout: code %d", w.Code)
	}
}

func TestLanHostPort(t *testing.T) {
	cases := map[[2]string]string{
		{"192.168.1.5", ":9449"}:          "192.168.1.5:9449",
		{"192.168.1.5", "127.0.0.1:9449"}: "192.168.1.5:9449",
	}
	for in, want := range cases {
		if got := lanHostPort(in[0], in[1]); got != want {
			t.Errorf("lanHostPort(%q,%q)=%q want %q", in[0], in[1], got, want)
		}
	}
}

// Kernel ABI (64-bit): struct sizes and the ioctl numbers derived from them.
func TestFFStructLayout(t *testing.T) {
	if s := unsafe.Sizeof(ffEffect{}); s != 48 {
		t.Errorf("ff_effect size %d, want 48", s)
	}
	if s := unsafe.Sizeof(uinputFFUpload{}); s != 104 {
		t.Errorf("uinput_ff_upload size %d, want 104", s)
	}
	if s := unsafe.Sizeof(uinputFFErase{}); s != 12 {
		t.Errorf("uinput_ff_erase size %d, want 12", s)
	}
	if uiBeginFFUpload != 0xc06855c8 {
		t.Errorf("UI_BEGIN_FF_UPLOAD %#x, want 0xc06855c8", uiBeginFFUpload)
	}
	if uiEndFFUpload != 0x406855c9 {
		t.Errorf("UI_END_FF_UPLOAD %#x, want 0x406855c9", uiEndFFUpload)
	}
}
