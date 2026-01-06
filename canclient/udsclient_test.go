package udsclient

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/LoveWonYoung/canio/drv"
	"github.com/LoveWonYoung/isotp/tp"
)

func TestUDSClientHandlesPendingNRC78(t *testing.T) {
	fake := newFakeDriver()
	addr := tp.NewAddress(tp.Normal11bits, 0x7DF, 0x7E8, 0, 0, 0, 0, 0, false, false)
	params := tp.NewParams()

	client, err := NewUDSClient(fake, addr, params)
	if err != nil {
		t.Fatalf("create uds client: %v", err)
	}
	defer client.Close()

	// Arrange: on first Write, emit NRC 0x78 then a positive response.
	fake.setResponder(func() {
		fake.sendAfter(5*time.Millisecond, 0x7E8, []byte{0x03, 0x7F, 0x22, 0x78})
		fake.sendAfter(15*time.Millisecond, 0x7E8, []byte{0x04, 0x62, 0xF1, 0x90, 0x01})
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	resp, err := client.RequestWithContext(ctx, []byte{0x22, 0xF1, 0x90}, RequestOptions{
		Timeout:                200 * time.Millisecond,
		PendingResponseTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	want := []byte{0x62, 0xF1, 0x90, 0x01}
	if string(resp) != string(want) {
		t.Fatalf("unexpected response: got % X want % X", resp, want)
	}
	if fake.writeCount() == 0 {
		t.Fatalf("driver Write was not called")
	}
}

// --- Test helper fake driver ------------------------------------------------

type fakeDriver struct {
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	rxChan    chan drv.UnifiedCANMessage
	responder func()
	triggered bool
	writes    int
}

func newFakeDriver() *fakeDriver {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeDriver{
		ctx:    ctx,
		cancel: cancel,
		rxChan: make(chan drv.UnifiedCANMessage, 16),
	}
}

func (f *fakeDriver) Init() error                          { return nil }
func (f *fakeDriver) Start()                               {}
func (f *fakeDriver) Stop()                                { f.cancel(); close(f.rxChan) }
func (f *fakeDriver) RxChan() <-chan drv.UnifiedCANMessage { return f.rxChan }
func (f *fakeDriver) Context() context.Context             { return f.ctx }

func (f *fakeDriver) Write(id int32, data []byte) error {
	f.mu.Lock()
	f.writes++
	runResponder := f.responder != nil && !f.triggered
	if runResponder {
		f.triggered = true
	}
	f.mu.Unlock()

	if runResponder {
		go f.responder()
	}
	return nil
}

func (f *fakeDriver) setResponder(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responder = fn
}

func (f *fakeDriver) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes
}

func (f *fakeDriver) sendAfter(delay time.Duration, id uint32, data []byte) {
	time.AfterFunc(delay, func() {
		var buf [64]byte
		copy(buf[:], data)
		msg := drv.UnifiedCANMessage{
			ID:   id,
			DLC:  byte(len(data)),
			Data: buf,
			IsFD: false,
		}
		select {
		case <-f.ctx.Done():
			return
		case f.rxChan <- msg:
		default:
		}
	})
}
