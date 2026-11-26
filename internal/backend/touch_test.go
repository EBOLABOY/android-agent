package backend

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type mockEvdevStream struct {
	mu          sync.Mutex
	buf         bytes.Buffer
	failAfterNs int // if >0, return write error on the N-th Write call (1-indexed), then allow subsequent writes (defer UP!)
	writeCount  int
}

func (m *mockEvdevStream) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeCount++
	if m.failAfterNs > 0 && m.writeCount == m.failAfterNs {
		return 0, errors.New("injected evdev write failure")
	}
	return m.buf.Write(p)
}

func (m *mockEvdevStream) Close() error { return nil }

func decodeEvents(b []byte) []inputEvent {
	var out []inputEvent
	for off := 0; off+24 <= len(b); off += 24 {
		out = append(out, inputEvent{
			Sec:   int64(binary.LittleEndian.Uint64(b[off:])),
			Usec:  int64(binary.LittleEndian.Uint64(b[off+8:])),
			Type:  binary.LittleEndian.Uint16(b[off+16:]),
			Code:  binary.LittleEndian.Uint16(b[off+18:]),
			Value: int32(binary.LittleEndian.Uint32(b[off+20:])),
		})
	}
	return out
}

func testTouchDevice() TouchDeviceInfo {
	return TouchDeviceInfo{
		Path:             "/dev/input/event7",
		Name:             "fts",
		BusType:          0x001c,
		SysfsPath:        "/sys/devices/platform/soc/spi0/spi0.0/input/input7",
		ProtocolB:        true,
		DirectProp:       true,
		MinX:             0,
		MaxX:             10799,
		MinY:             0,
		MaxY:             23999,
		HasBtnTouch:      true,
		HasBtnToolFinger: true,
		HasTouchMajor:    true,
		MaxTouchMajor:    10800,
		Available:        true,
	}
}

func TestSelectBestTouchDeviceFiltersBandAndVirtual(t *testing.T) {
	candidates := []TouchDeviceInfo{
		{
			Path: "/dev/input/event0", Name: "uinput-goodix", BusType: 0x0000,
			SysfsPath: "/sys/devices/virtual/input/input0", ProtocolB: false,
		},
		{
			Path: "/dev/input/event11", Name: "Xiaomi Smart Band 11 2BC2", BusType: busBluetooth,
			SysfsPath: "/sys/devices/virtual/misc/uhid/0005:0000:0000.0006/input/input65",
			ProtocolB: true, DirectProp: true, MinX: 0, MaxX: 1000, MinY: 0, MaxY: 1000,
			HasBtnTouch: true,
		},
		testTouchDevice(),
	}

	best, ok := selectBestTouchDevice(candidates)
	if !ok {
		t.Fatal("expected physical touchscreen to be selected")
	}
	if best.Path != "/dev/input/event7" || best.Name != "fts" {
		t.Fatalf("expected /dev/input/event7 (fts), got %+v", best)
	}
}

func TestMapCoordsAndTapRelease(t *testing.T) {
	ev := NewEvdevTouch(testTouchDevice())
	rx, ry := ev.MapCoords(540, 1200, 1080, 2400)
	if rx < 5390 || rx > 5410 || ry < 11990 || ry > 12010 {
		t.Fatalf("unexpected 10x mapping for center: (%d,%d)", rx, ry)
	}
	rxEnd, ryEnd := ev.MapCoords(1079, 2399, 1080, 2400)
	if rxEnd != 10799 || ryEnd != 23999 {
		t.Fatalf("expected bottom-right exact edge (10799,23999), got (%d,%d)", rxEnd, ryEnd)
	}

	stream := &mockEvdevStream{}
	var slept time.Duration
	ev.openWriter = func(string) (io.WriteCloser, error) { return stream, nil }
	ev.sleepFn = func(_ context.Context, d time.Duration) error {
		slept += d
		return nil
	}

	if err := ev.Tap(context.Background(), 540, 1200, 1080, 2400); err != nil {
		t.Fatalf("Tap failed: %v", err)
	}
	if slept < 30*time.Millisecond || slept > 80*time.Millisecond {
		t.Fatalf("expected randomized tap dwell 30..80ms, got %v", slept)
	}

	events := decodeEvents(stream.buf.Bytes())
	if len(events) < 10 {
		t.Fatalf("expected complete DOWN + UP frames, got %d events", len(events))
	}
	assertCleanDownAndUp(t, events)
}

func TestSwipeCurvedBezierAndDeferReleaseOnErrorOrCancel(t *testing.T) {
	// Case 1: Normal swipe produces non-linear curved path and clean UP
	ev := NewEvdevTouch(testTouchDevice())
	stream := &mockEvdevStream{}
	ev.openWriter = func(string) (io.WriteCloser, error) { return stream, nil }
	ev.sleepFn = func(context.Context, time.Duration) error { return nil }

	if err := ev.Swipe(context.Background(), 540, 1900, 540, 500, 1080, 2400, 320); err != nil {
		t.Fatalf("Swipe failed: %v", err)
	}
	events := decodeEvents(stream.buf.Bytes())
	assertCleanDownAndUp(t, events)

	hasCurvedX := false
	for _, ev := range events {
		if ev.Type == evAbs && ev.Code == absMtPositionX && (ev.Value < 5380 || ev.Value > 5420) {
			hasCurvedX = true
			break
		}
	}
	if !hasCurvedX {
		t.Fatal("expected curved Bézier arc deviation on X during vertical swipe")
	}

	// Case 2: Write failure mid-swipe MUST still emit deferred release frame!
	streamErr := &mockEvdevStream{failAfterNs: 4}
	ev.openWriter = func(string) (io.WriteCloser, error) { return streamErr, nil }
	err := ev.Swipe(context.Background(), 540, 1900, 540, 500, 1080, 2400, 320)
	if err == nil {
		t.Fatal("expected injected write error")
	}
	assertEndsWithRelease(t, decodeEvents(streamErr.buf.Bytes()))

	// Case 3: Context cancellation mid-swipe MUST still emit deferred release frame!
	ctx, cancel := context.WithCancel(context.Background())
	streamCancel := &mockEvdevStream{}
	ev.openWriter = func(string) (io.WriteCloser, error) { return streamCancel, nil }
	stepSeen := 0
	ev.sleepFn = func(c context.Context, _ time.Duration) error {
		stepSeen++
		if stepSeen == 3 {
			cancel()
		}
		return c.Err()
	}
	err = ev.Swipe(ctx, 540, 1900, 540, 500, 1080, 2400, 320)
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	assertEndsWithRelease(t, decodeEvents(streamCancel.buf.Bytes()))
}

func TestEvdevConcurrentSerialization(t *testing.T) {
	ev := NewEvdevTouch(testTouchDevice())
	var streams []*mockEvdevStream
	var mu sync.Mutex
	ev.openWriter = func(string) (io.WriteCloser, error) {
		s := &mockEvdevStream{}
		mu.Lock()
		streams = append(streams, s)
		mu.Unlock()
		return s, nil
	}
	ev.sleepFn = func(_ context.Context, d time.Duration) error {
		time.Sleep(2 * time.Millisecond)
		return nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if err := ev.Tap(context.Background(), 200+idx*100, 800, 1080, 2400); err != nil {
				t.Errorf("concurrent Tap %d failed: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	if len(streams) != 4 {
		t.Fatalf("expected 4 serialized gesture streams, got %d", len(streams))
	}
	for i, s := range streams {
		assertCleanDownAndUp(t, decodeEvents(s.buf.Bytes()))
		_ = i
	}
}

func assertCleanDownAndUp(t *testing.T, events []inputEvent) {
	t.Helper()
	hasTrackDown := false
	for _, ev := range events {
		if ev.Type == evAbs && ev.Code == absMtTrackingID && ev.Value > 0 {
			hasTrackDown = true
			break
		}
	}
	if !hasTrackDown {
		t.Fatal("missing initial ABS_MT_TRACKING_ID > 0")
	}
	assertEndsWithRelease(t, events)
}

func assertEndsWithRelease(t *testing.T, events []inputEvent) {
	t.Helper()
	hasTrackRelease := false
	hasBtnRelease := false
	lastIdx := len(events) - 1
	if lastIdx < 0 || events[lastIdx].Type != evSyn || events[lastIdx].Code != synReport {
		t.Fatalf("expected final event to be SYN_REPORT, got %+v", events)
	}
	for _, ev := range events {
		if ev.Type == evAbs && ev.Code == absMtTrackingID && ev.Value == -1 {
			hasTrackRelease = true
		}
		if ev.Type == evKey && ev.Code == btnTouch && ev.Value == 0 {
			hasBtnRelease = true
		}
	}
	if !hasTrackRelease || !hasBtnRelease {
		t.Fatalf("missing release events (track=-1:%v, btn_touch=0:%v)", hasTrackRelease, hasBtnRelease)
	}
}

func TestEvdevHighSlotSelectionAndDrain(t *testing.T) {
	dev := testTouchDevice()
	dev.MaxSlot = 9
	ev := NewEvdevTouch(dev)
	if ev.Slot() != 8 || ev.Device().Slot != 8 {
		t.Fatalf("expected high touch slot 8 for MaxSlot=9, got slot=%d", ev.Slot())
	}

	stream := &mockEvdevStream{}
	inSleep := make(chan struct{})
	releaseSleep := make(chan struct{})
	ev.openWriter = func(string) (io.WriteCloser, error) { return stream, nil }
	ev.sleepFn = func(_ context.Context, _ time.Duration) error {
		close(inSleep)
		<-releaseSleep
		return nil
	}

	doneTap := make(chan error, 1)
	go func() {
		doneTap <- ev.Tap(context.Background(), 300, 600, 1080, 2400)
	}()

	<-inSleep
	drained := make(chan struct{})
	go func() {
		ev.Drain()
		close(drained)
	}()

	select {
	case <-drained:
		t.Fatal("Drain() returned before in-flight gesture released")
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseSleep)
	if err := <-doneTap; err != nil {
		t.Fatalf("Tap failed: %v", err)
	}
	<-drained

	events := decodeEvents(stream.buf.Bytes())
	assertCleanDownAndUp(t, events)
	sawSlot8 := false
	for _, e := range events {
		if e.Type == evAbs && e.Code == absMtSlot {
			if e.Value != 8 {
				t.Fatalf("expected all ABS_MT_SLOT events to use slot 8, got %d", e.Value)
			}
			sawSlot8 = true
		}
	}
	if !sawSlot8 {
		t.Fatal("expected ABS_MT_SLOT events in output")
	}
}

func TestShellBackendScreenOffAndLockedGate(t *testing.T) {
	if err := parseScreenInteractivePolicy("screenState=SCREEN_STATE_OFF\nshowing=false\n"); err == nil {
		t.Fatal("expected SCREEN_STATE_OFF to be rejected")
	}
	if err := parseScreenInteractivePolicy("screenState=SCREEN_STATE_ON\nKeyguardServiceDelegate\n  showing=true\n"); err == nil {
		t.Fatal("expected keyguard showing=true to be rejected")
	}
	if err := parseScreenInteractivePolicy("screenState=SCREEN_STATE_ON\nKeyguardServiceDelegate\n  showing=false\n"); err != nil {
		t.Fatalf("expected unlocked screen-on policy to pass, got %v", err)
	}

	be := NewShellBackend("", time.Second)
	be.screenGateFunc = func(context.Context) error {
		return errors.New("device is locked (keyguard showing); refusing touch injection")
	}
	if err := be.Tap(context.Background(), 100, 200); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("expected Tap to be blocked when locked, got %v", err)
	}
	if err := be.LongPress(context.Background(), 100, 200, 500); err == nil {
		t.Fatal("expected LongPress to be blocked when locked")
	}
	if err := be.Swipe(context.Background(), 100, 200, 100, 400, 300); err == nil {
		t.Fatal("expected Swipe to be blocked when locked")
	}
}
