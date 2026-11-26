package backend

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	evSyn = 0x00
	evKey = 0x01
	evAbs = 0x03

	synReport = 0x00

	btnToolFinger = 0x145
	btnTouch      = 0x14a

	absMtSlot       = 0x2f
	absMtTouchMajor = 0x30
	absMtPositionX  = 0x35
	absMtPositionY  = 0x36
	absMtTrackingID = 0x39

	inputPropDirect = 0x01

	busBluetooth = 0x05
	busVirtual   = 0x06

	// Linux ioctl request codes for <linux/input.h> (little-endian ARM64/x86_64)
	ioctlEVIOCGID   = 0x80084502
	ioctlEVIOCGNAME = 0x81004506
	ioctlEVIOCGPROP = 0x80084509
	ioctlEVIOCGKEY  = 0x80604521
	ioctlEVIOCGABS  = 0x80084523
	ioctlEVIOCGInfo = 0x80184540
)

// TouchDeviceInfo describes the detected physical touchscreen evdev node.
type TouchDeviceInfo struct {
	Path             string `json:"path"`
	Name             string `json:"name"`
	BusType          uint16 `json:"bus_type"`
	SysfsPath        string `json:"sysfs_path,omitempty"`
	ProtocolB        bool   `json:"protocol_b"`
	DirectProp       bool   `json:"direct_prop"`
	MinX             int32  `json:"min_x"`
	MaxX             int32  `json:"max_x"`
	MinY             int32  `json:"min_y"`
	MaxY             int32  `json:"max_y"`
	HasBtnTouch      bool   `json:"has_btn_touch"`
	HasBtnToolFinger bool   `json:"has_btn_tool_finger"`
	HasTouchMajor    bool   `json:"has_touch_major"`
	MaxTouchMajor    int32  `json:"max_touch_major,omitempty"`
	Available        bool   `json:"available"`
	ProbeError       string `json:"probe_error,omitempty"`
}

// TouchStatusInfo exposes current evdev hardware probe & active injection method in phone_status.
type TouchStatusInfo struct {
	Device     TouchDeviceInfo `json:"device"`
	LastMethod string          `json:"last_method"`
}

type inputEvent struct {
	Sec   int64
	Usec  int64
	Type  uint16
	Code  uint16
	Value int32
}

// EvdevTouch writes Linux Multitouch Type B events directly to the physical touchscreen /dev/input/eventX.
type EvdevTouch struct {
	dev        TouchDeviceInfo
	writeMu    sync.Mutex
	trackingID int32

	openWriter func(path string) (io.WriteCloser, error)
	sleepFn    func(ctx context.Context, d time.Duration) error
}

func NewEvdevTouch(dev TouchDeviceInfo) *EvdevTouch {
	return &EvdevTouch{
		dev:        dev,
		trackingID: 100,
		openWriter: func(path string) (io.WriteCloser, error) {
			return os.OpenFile(path, os.O_WRONLY, 0)
		},
		sleepFn: sleepWithContext,
	}
}

func (e *EvdevTouch) Available() bool {
	return e != nil && e.dev.Available && e.dev.Path != "" && e.dev.MaxX > e.dev.MinX && e.dev.MaxY > e.dev.MinY
}

func (e *EvdevTouch) Device() TouchDeviceInfo {
	if e == nil {
		return TouchDeviceInfo{}
	}
	return e.dev
}

// ProbeTouchscreen scans /dev/input/event* and selects the physical hardware touchscreen,
// explicitly rejecting Bluetooth/uhid/virtual direct-touch peripherals (such as wearables).
func ProbeTouchscreen() (TouchDeviceInfo, error) {
	matches, err := filepath.Glob("/dev/input/event*")
	if err != nil {
		return TouchDeviceInfo{ProbeError: err.Error()}, err
	}
	sort.Slice(matches, func(i, j int) bool {
		return eventNum(matches[i]) < eventNum(matches[j])
	})

	var candidates []TouchDeviceInfo
	var lastErr error
	for _, p := range matches {
		info, err := inspectInputDevice(p)
		if err != nil {
			lastErr = err
			continue
		}
		candidates = append(candidates, info)
	}

	best, ok := selectBestTouchDevice(candidates)
	if !ok {
		errMsg := "no physical Multitouch Type B touchscreen found"
		if lastErr != nil && len(candidates) == 0 {
			errMsg = fmt.Sprintf("%s (%v)", errMsg, lastErr)
		}
		return TouchDeviceInfo{ProbeError: errMsg}, errors.New(errMsg)
	}
	best.Available = true
	return best, nil
}

func eventNum(path string) int {
	base := filepath.Base(path)
	s := strings.TrimPrefix(base, "event")
	n, err := strconv.Atoi(s)
	if err != nil {
		return 1 << 20
	}
	return n
}

func inspectInputDevice(path string) (TouchDeviceInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return TouchDeviceInfo{}, err
	}
	defer f.Close()
	fd := f.Fd()

	var idBuf [8]byte
	if err := ioctlBytes(fd, ioctlEVIOCGID, idBuf[:]); err != nil {
		return TouchDeviceInfo{}, err
	}
	busType := binary.LittleEndian.Uint16(idBuf[0:2])

	var nameBuf [256]byte
	if err := ioctlBytes(fd, ioctlEVIOCGNAME, nameBuf[:]); err != nil {
		return TouchDeviceInfo{}, err
	}
	name := string(bytes.TrimRight(nameBuf[:], "\x00"))

	var propBuf [8]byte
	_ = ioctlBytes(fd, ioctlEVIOCGPROP, propBuf[:])

	var keyBits [96]byte
	_ = ioctlBytes(fd, ioctlEVIOCGKEY, keyBits[:])

	var absBits [8]byte
	if err := ioctlBytes(fd, ioctlEVIOCGABS, absBits[:]); err != nil {
		return TouchDeviceInfo{}, err
	}

	sysfsTarget, _ := os.Readlink(filepath.Join("/sys/class/input", filepath.Base(path), "device"))
	if absSys, err := filepath.EvalSymlinks(filepath.Join("/sys/class/input", filepath.Base(path), "device")); err == nil {
		sysfsTarget = absSys
	}

	hasSlot := testBit(absBits[:], absMtSlot)
	hasTrack := testBit(absBits[:], absMtTrackingID)
	hasX := testBit(absBits[:], absMtPositionX)
	hasY := testBit(absBits[:], absMtPositionY)
	hasMajor := testBit(absBits[:], absMtTouchMajor)

	info := TouchDeviceInfo{
		Path:             path,
		Name:             name,
		BusType:          busType,
		SysfsPath:        sysfsTarget,
		ProtocolB:        hasSlot && hasTrack && hasX && hasY,
		DirectProp:       testBit(propBuf[:], inputPropDirect),
		HasBtnTouch:      testBit(keyBits[:], btnTouch),
		HasBtnToolFinger: testBit(keyBits[:], btnToolFinger),
		HasTouchMajor:    hasMajor,
	}

	if hasX {
		_, minX, maxX, err := readAbsInfo(fd, absMtPositionX)
		if err == nil {
			info.MinX, info.MaxX = minX, maxX
		}
	}
	if hasY {
		_, minY, maxY, err := readAbsInfo(fd, absMtPositionY)
		if err == nil {
			info.MinY, info.MaxY = minY, maxY
		}
	}
	if hasMajor {
		_, _, maxMajor, err := readAbsInfo(fd, absMtTouchMajor)
		if err == nil {
			info.MaxTouchMajor = maxMajor
		}
	}
	return info, nil
}

func selectBestTouchDevice(candidates []TouchDeviceInfo) (TouchDeviceInfo, bool) {
	var best TouchDeviceInfo
	bestScore := -1

	for _, c := range candidates {
		if !c.ProtocolB || c.MaxX <= c.MinX || c.MaxY <= c.MinY {
			continue
		}
		if c.BusType == busBluetooth || c.BusType == busVirtual {
			continue
		}
		lowSys := strings.ToLower(c.SysfsPath)
		if strings.Contains(lowSys, "/virtual/") || strings.Contains(lowSys, "/uhid/") {
			continue
		}
		lowName := strings.ToLower(c.Name)
		if strings.Contains(lowName, "uinput") || strings.Contains(lowName, "band") || strings.Contains(lowName, "touchpad") {
			continue
		}

		score := 10
		if c.DirectProp {
			score += 50
		}
		if c.HasBtnTouch {
			score += 10
		}
		if strings.Contains(lowSys, "/spi") || strings.Contains(lowSys, "/i2c") {
			score += 30
		}
		if strings.Contains(lowName, "fts") || strings.Contains(lowName, "goodix") || strings.Contains(lowName, "focal") || strings.Contains(lowName, "synaptics") {
			score += 20
		}
		if (c.MaxX - c.MinX) >= 1000 {
			score += 5
		}
		if score > bestScore {
			bestScore = score
			best = c
		}
	}
	return best, bestScore >= 0
}

func ioctlBytes(fd uintptr, req uintptr, buf []byte) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(byteSlicePtr(buf)))
	if errno != 0 {
		return errno
	}
	return nil
}

func readAbsInfo(fd uintptr, code uint16) (val, min, max int32, err error) {
	var buf [24]byte
	if err = ioctlBytes(fd, uintptr(ioctlEVIOCGInfo+uint32(code)), buf[:]); err != nil {
		return 0, 0, 0, err
	}
	val = int32(binary.LittleEndian.Uint32(buf[0:4]))
	min = int32(binary.LittleEndian.Uint32(buf[4:8]))
	max = int32(binary.LittleEndian.Uint32(buf[8:12]))
	return val, min, max, nil
}

func testBit(bits []byte, bit int) bool {
	idx := bit / 8
	if idx < 0 || idx >= len(bits) {
		return false
	}
	return (bits[idx] & (1 << uint(bit%8))) != 0
}

// MapCoords converts portrait physical screen coordinates [0..screenW-1, 0..screenH-1]
// into raw evdev ABS_MT_POSITION_X/Y units clamped cleanly within device bounds.
func (e *EvdevTouch) MapCoords(pxX, pxY, screenW, screenH int) (int32, int32) {
	if screenW <= 1 {
		screenW = 1080
	}
	if screenH <= 1 {
		screenH = 2400
	}
	pxX = clampInt(pxX, 0, screenW-1)
	pxY = clampInt(pxY, 0, screenH-1)

	spanX := int64(e.dev.MaxX - e.dev.MinX)
	spanY := int64(e.dev.MaxY - e.dev.MinY)
	rawX := int64(e.dev.MinX) + int64(math.Round(float64(pxX)*float64(spanX)/float64(screenW-1)))
	rawY := int64(e.dev.MinY) + int64(math.Round(float64(pxY)*float64(spanY)/float64(screenH-1)))

	if rawX < int64(e.dev.MinX) {
		rawX = int64(e.dev.MinX)
	} else if rawX > int64(e.dev.MaxX) {
		rawX = int64(e.dev.MaxX)
	}
	if rawY < int64(e.dev.MinY) {
		rawY = int64(e.dev.MinY)
	} else if rawY > int64(e.dev.MaxY) {
		rawY = int64(e.dev.MaxY)
	}
	return int32(rawX), int32(rawY)
}

func (e *EvdevTouch) nextTrackingIDLocked() int32 {
	e.trackingID++
	if e.trackingID >= 60000 || e.trackingID <= 0 {
		e.trackingID = 1
	}
	return e.trackingID
}

// Tap injects a physical Type B touch-down and release at (x, y) with randomized 30~80ms dwell.
func (e *EvdevTouch) Tap(ctx context.Context, x, y, screenW, screenH int) error {
	dwell := time.Duration(30+rand.Intn(51)) * time.Millisecond
	return e.holdAt(ctx, x, y, screenW, screenH, dwell)
}

// LongPress injects a physical Type B press held for durationMs (±15ms jitter) with guaranteed release.
func (e *EvdevTouch) LongPress(ctx context.Context, x, y, screenW, screenH, durationMs int) error {
	if durationMs <= 0 {
		durationMs = 800
	}
	jitter := rand.Intn(31) - 15
	ms := durationMs + jitter
	if ms < 80 {
		ms = 80
	}
	return e.holdAt(ctx, x, y, screenW, screenH, time.Duration(ms)*time.Millisecond)
}

func (e *EvdevTouch) holdAt(ctx context.Context, x, y, screenW, screenH int, dwell time.Duration) error {
	if !e.Available() {
		return errors.New("evdev touch device not available")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	w, err := e.openWriter(e.dev.Path)
	if err != nil {
		return fmt.Errorf("open %s: %w", e.dev.Path, err)
	}
	defer w.Close()

	fingerDown := false
	defer func() {
		if fingerDown {
			_ = e.emitUpLocked(w)
		}
	}()

	rawX, rawY := e.MapCoords(x, y, screenW, screenH)
	tid := e.nextTrackingIDLocked()
	major := e.pickTouchMajor(220)

	fingerDown = true
	if err := e.emitDownLocked(w, tid, rawX, rawY, major); err != nil {
		return err
	}
	if err := e.sleepFn(ctx, dwell); err != nil {
		return err
	}
	fingerDown = false
	return e.emitUpLocked(w)
}

// Swipe injects a curved minimum-jerk variable-speed Type B swipe across (x1, y1)->(x2, y2).
// Any mid-gesture write error or context cancellation triggers a deferred finger-lift frame.
func (e *EvdevTouch) Swipe(ctx context.Context, x1, y1, x2, y2, screenW, screenH, durationMs int) error {
	if !e.Available() {
		return errors.New("evdev touch device not available")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if durationMs <= 0 {
		durationMs = 320
	}

	e.writeMu.Lock()
	defer e.writeMu.Unlock()

	w, err := e.openWriter(e.dev.Path)
	if err != nil {
		return fmt.Errorf("open %s: %w", e.dev.Path, err)
	}
	defer w.Close()

	fingerDown := false
	defer func() {
		if fingerDown {
			_ = e.emitUpLocked(w)
		}
	}()

	dx := float64(x2 - x1)
	dy := float64(y2 - y1)
	dist := math.Hypot(dx, dy)

	// Build a subtle quadratic Bézier control point with natural perpendicular arc.
	ctrlX := (float64(x1) + float64(x2)) / 2
	ctrlY := (float64(y1) + float64(y2)) / 2
	if dist > 20 {
		nx, ny := -dy/dist, dx/dist
		arcMag := dist * (0.03 + rand.Float64()*0.04)
		if arcMag > 42 {
			arcMag = 42
		}
		if rand.Intn(2) == 0 {
			arcMag = -arcMag
		}
		ctrlX += nx * arcMag
		ctrlY += ny * arcMag
	}

	steps := durationMs / 8
	if steps < 10 {
		steps = 10
	} else if steps > 65 {
		steps = 65
	}
	stepDur := time.Duration(durationMs) * time.Millisecond / time.Duration(steps)
	if stepDur < 4*time.Millisecond {
		stepDur = 4 * time.Millisecond
	}

	startX, startY := e.MapCoords(x1, y1, screenW, screenH)
	tid := e.nextTrackingIDLocked()
	baseMajor := e.pickTouchMajor(240)

	fingerDown = true
	if err := e.emitDownLocked(w, tid, startX, startY, baseMajor); err != nil {
		return err
	}

	for step := 1; step <= steps; step++ {
		if err := e.sleepFn(ctx, stepDur); err != nil {
			return err
		}
		u := float64(step) / float64(steps)
		// Minimum-jerk easing s(u) = 10u^3 - 15u^4 + 6u^5 (human acceleration + deceleration)
		t := u * u * u * (10 + u*(-15+6*u))
		inv := 1 - t
		px := inv*inv*float64(x1) + 2*inv*t*ctrlX + t*t*float64(x2)
		py := inv*inv*float64(y1) + 2*inv*t*ctrlY + t*t*float64(y2)
		if step < steps {
			px += float64(rand.Intn(3) - 1)
			py += float64(rand.Intn(3) - 1)
		}
		rx, ry := e.MapCoords(int(math.Round(px)), int(math.Round(py)), screenW, screenH)
		major := e.pickTouchMajor(240 + rand.Intn(50) - 25)
		if err := e.emitMoveLocked(w, rx, ry, major); err != nil {
			return err
		}
	}

	// Brief settle before lifting finger
	if err := e.sleepFn(ctx, time.Duration(8+rand.Intn(10))*time.Millisecond); err != nil {
		return err
	}
	fingerDown = false
	return e.emitUpLocked(w)
}

func (e *EvdevTouch) pickTouchMajor(base int) int32 {
	if !e.dev.HasTouchMajor {
		return 0
	}
	if e.dev.MaxTouchMajor > 0 && base > int(e.dev.MaxTouchMajor) {
		return e.dev.MaxTouchMajor / 4
	}
	return int32(base)
}

func (e *EvdevTouch) emitDownLocked(w io.Writer, trackingID, rawX, rawY, touchMajor int32) error {
	now := time.Now()
	events := make([]inputEvent, 0, 8)
	events = append(events,
		makeEvent(now, evAbs, absMtSlot, 0),
		makeEvent(now, evAbs, absMtTrackingID, trackingID),
		makeEvent(now, evAbs, absMtPositionX, rawX),
		makeEvent(now, evAbs, absMtPositionY, rawY),
	)
	if e.dev.HasTouchMajor {
		events = append(events, makeEvent(now, evAbs, absMtTouchMajor, touchMajor))
	}
	if e.dev.HasBtnTouch {
		events = append(events, makeEvent(now, evKey, btnTouch, 1))
	}
	if e.dev.HasBtnToolFinger {
		events = append(events, makeEvent(now, evKey, btnToolFinger, 1))
	}
	events = append(events, makeEvent(now, evSyn, synReport, 0))
	return writeEvents(w, events)
}

func (e *EvdevTouch) emitMoveLocked(w io.Writer, rawX, rawY, touchMajor int32) error {
	now := time.Now()
	events := make([]inputEvent, 0, 5)
	events = append(events,
		makeEvent(now, evAbs, absMtSlot, 0),
		makeEvent(now, evAbs, absMtPositionX, rawX),
		makeEvent(now, evAbs, absMtPositionY, rawY),
	)
	if e.dev.HasTouchMajor {
		events = append(events, makeEvent(now, evAbs, absMtTouchMajor, touchMajor))
	}
	events = append(events, makeEvent(now, evSyn, synReport, 0))
	return writeEvents(w, events)
}

func (e *EvdevTouch) emitUpLocked(w io.Writer) error {
	now := time.Now()
	events := make([]inputEvent, 0, 5)
	events = append(events,
		makeEvent(now, evAbs, absMtSlot, 0),
		makeEvent(now, evAbs, absMtTrackingID, -1),
	)
	if e.dev.HasBtnTouch {
		events = append(events, makeEvent(now, evKey, btnTouch, 0))
	}
	if e.dev.HasBtnToolFinger {
		events = append(events, makeEvent(now, evKey, btnToolFinger, 0))
	}
	events = append(events, makeEvent(now, evSyn, synReport, 0))
	return writeEvents(w, events)
}

func makeEvent(t time.Time, typ, code uint16, val int32) inputEvent {
	return inputEvent{
		Sec:   t.Unix(),
		Usec:  int64(t.Nanosecond() / 1000),
		Type:  typ,
		Code:  code,
		Value: val,
	}
}

func writeEvents(w io.Writer, events []inputEvent) error {
	buf := make([]byte, len(events)*24)
	for i, ev := range events {
		off := i * 24
		binary.LittleEndian.PutUint64(buf[off:], uint64(ev.Sec))
		binary.LittleEndian.PutUint64(buf[off+8:], uint64(ev.Usec))
		binary.LittleEndian.PutUint16(buf[off+16:], ev.Type)
		binary.LittleEndian.PutUint16(buf[off+18:], ev.Code)
		binary.LittleEndian.PutUint32(buf[off+20:], uint32(ev.Value))
	}
	_, err := w.Write(buf)
	return err
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
