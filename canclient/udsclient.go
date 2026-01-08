package canclient

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/LoveWonYoung/canio/drv"
	"github.com/LoveWonYoung/isotp/tp"
)

const (
	defaultTimeout        = 500 * time.Millisecond
	defaultMaxRetries     = 3
	defaultRetryDelay     = 100 * time.Millisecond
	defaultRecvPoll       = 20 * time.Millisecond
	defaultPendingTimeout = 0
)

// RequestOptions defines timeouts and retry behavior for UDS requests.
type RequestOptions struct {
	Timeout                time.Duration
	MaxRetries             int
	RetryDelay             time.Duration
	PendingResponseTimeout time.Duration
}

// UDSClient manages ISO-TP transport and basic UDS request/response flow.
type UDSClient struct {
	adapter *drv.ToomossAdapter
	tll     *tp.TransportLayerLogic
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	reqMu   sync.Mutex
	mu      sync.Mutex
	closed  bool
}

// UDSError represents a negative response (0x7F) from the ECU.
type UDSError struct {
	ServiceID byte
	NRC       byte
	Message   string
}

func (e UDSError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("uds negative response: service=0x%02X nrc=0x%02X", e.ServiceID, e.NRC)
}

func (e UDSError) ShouldRetry() bool {
	switch e.NRC {
	case 0x21:
		return true
	case 0x78:
		return true
	default:
		return false
	}
}

// NewUDSClient builds an ISO-TP stack over the provided CAN drv.
func NewUDSClient(dev drv.CANDriver, addr *tp.Address, params tp.Params) (*UDSClient, error) {
	if dev == nil {
		return nil, errors.New("CAN driver instance cannot be nil")
	}
	if addr == nil {
		return nil, errors.New("address cannot be nil")
	}
	if params.TxDataLength == 0 {
		params = tp.NewParams()
	}

	adapter, err := drv.NewToomossAdapter(dev)
	if err != nil {
		return nil, err
	}

	client := &UDSClient{
		adapter: adapter,
	}
	client.ctx, client.cancel = context.WithCancel(context.Background())
	client.tll = tp.NewTransportLayerLogic(adapter.RxFunc, adapter.TxFunc, addr, nil, &params, nil)
	client.start()
	return client, nil
}

func (c *UDSClient) start() {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for {
			select {
			case <-c.ctx.Done():
				return
			default:
			}

			sleep := c.tll.SleepTime()
			if sleep <= 0 {
				sleep = 0.001
			}
			c.tll.Process(sleep, true, true)
			time.Sleep(time.Duration(sleep * float64(time.Second)))
		}
	}()
}

// Close stops background processing and releases the drv.
func (c *UDSClient) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()

	c.cancel()
	c.wg.Wait()
	c.adapter.Close()
}

// IsClosed reports if the client has been closed.
func (c *UDSClient) IsClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Request sends a physical request with default timing.
func (c *UDSClient) Request(payload []byte) ([]byte, error) {
	return c.RequestPhysical(payload)
}

// RequestPhysical sends a physical request with default timing.
func (c *UDSClient) RequestPhysical(payload []byte) ([]byte, error) {
	return c.RequestWithContext(context.Background(), payload, RequestOptions{})
}

// RequestWithTimeout sends a physical request with a custom timeout.
func (c *UDSClient) RequestWithTimeout(payload []byte, timeout time.Duration) ([]byte, error) {
	return c.RequestPhysicalWithTimeout(payload, timeout)
}

// RequestPhysicalWithTimeout sends a physical request with a custom timeout.
func (c *UDSClient) RequestPhysicalWithTimeout(payload []byte, timeout time.Duration) ([]byte, error) {
	return c.RequestWithContext(context.Background(), payload, RequestOptions{Timeout: timeout})
}

// RequestFunctional sends a functional request with default timing.
func (c *UDSClient) RequestFunctional(payload []byte) ([]byte, error) {
	return c.requestWithContext(context.Background(), payload, RequestOptions{}, tp.Functional)
}

// RequestFunctionalWithTimeout sends a functional request with a custom timeout.
func (c *UDSClient) RequestFunctionalWithTimeout(payload []byte, timeout time.Duration) ([]byte, error) {
	return c.requestWithContext(context.Background(), payload, RequestOptions{Timeout: timeout}, tp.Functional)
}

// RequestWithContext sends a physical request with context-aware timeout and retries.
func (c *UDSClient) RequestWithContext(ctx context.Context, payload []byte, opts RequestOptions) ([]byte, error) {
	return c.requestWithContext(ctx, payload, opts, tp.Physical)
}

func (c *UDSClient) requestWithContext(ctx context.Context, payload []byte, opts RequestOptions, targetType uint32) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(payload) == 0 {
		return nil, errors.New("payload cannot be empty")
	}

	c.reqMu.Lock()
	defer c.reqMu.Unlock()

	if c.IsClosed() {
		return nil, errors.New("uds client is closed")
	}

	opts = normalizeOptions(opts)

	for attempt := 0; attempt <= opts.MaxRetries; attempt++ {
		if err := c.tll.Send(payload, targetType, opts.Timeout); err != nil {
			return nil, err
		}

		deadline := time.Now().Add(opts.Timeout)
		pendingTimeout := opts.PendingResponseTimeout
		if pendingTimeout <= 0 {
			pendingTimeout = opts.Timeout
		}

		retry := false
		for {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			remaining := time.Until(deadline)
			if remaining <= 0 {
				break
			}

			wait := remaining
			if wait > defaultRecvPoll {
				wait = defaultRecvPoll
			}
			resp, ok := c.tll.Recv(true, wait)
			if !ok {
				continue
			}
			if isNegativeResponse(resp) {
				nrc := resp[2]
				err := UDSError{
					ServiceID: resp[1],
					NRC:       nrc,
				}
				if nrc == 0x78 {
					deadline = time.Now().Add(pendingTimeout)
					continue
				}
				if err.ShouldRetry() && attempt < opts.MaxRetries {
					retry = true
					break
				}
				return nil, err
			}
			return resp, nil
		}

		if retry && attempt < opts.MaxRetries {
			if err := sleepWithContext(ctx, opts.RetryDelay); err != nil {
				return nil, err
			}
			continue
		}
		if attempt < opts.MaxRetries {
			if err := sleepWithContext(ctx, opts.RetryDelay); err != nil {
				return nil, err
			}
			continue
		}
	}

	return nil, fmt.Errorf("uds request timed out after %d attempt(s)", opts.MaxRetries+1)
}

func normalizeOptions(opts RequestOptions) RequestOptions {
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.MaxRetries < 0 {
		opts.MaxRetries = 0
	}
	if opts.MaxRetries == 0 {
		opts.MaxRetries = defaultMaxRetries
	}
	if opts.RetryDelay <= 0 {
		opts.RetryDelay = defaultRetryDelay
	}
	if opts.PendingResponseTimeout == 0 {
		opts.PendingResponseTimeout = defaultPendingTimeout
	}
	return opts
}

func isNegativeResponse(resp []byte) bool {
	return len(resp) >= 3 && resp[0] == 0x7F
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
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
