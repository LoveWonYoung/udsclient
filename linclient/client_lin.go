package linclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/LoveWonYoung/lintp/tp"
	"github.com/LoveWonYoung/udsclient/nrc"
)

// ClientConfig holds configuration options for the UDS client.
type ClientConfig struct {
	TargetNad      byte
	DefaultTimeout time.Duration
	MaxRetries     int
}

// DefaultClientConfig returns a configuration with sensible defaults.
func DefaultClientConfig(targetNad byte) ClientConfig {
	return ClientConfig{
		TargetNad:      targetNad,
		DefaultTimeout: 2 * time.Second,
		MaxRetries:     3,
	}
}

// Client 是一个高级UDS客户端，用于与单个LIN从节点（ECU）进行诊断通信。
type Client struct {
	linMaster *tp.LinMaster
	config    ClientConfig
}

// NewClient 创建一个新的UDS客户端实例。
// 它需要一个已配置好的LIN驱动和目标ECU的NAD。
func NewClient(driver tp.Driver, targetNad byte) *Client {
	return NewClientWithConfig(driver, DefaultClientConfig(targetNad))
}

// NewClientWithConfig 使用自定义配置创建客户端。
func NewClientWithConfig(driver tp.Driver, config ClientConfig) *Client {
	master := tp.NewMaster(driver)
	return &Client{
		linMaster: master,
		config:    config,
	}
}

// Close 会优雅地关闭客户端并释放底层资源。
// 在程序结束时应该调用此方法。
func (c *Client) Close() {
	c.linMaster.Close()
}

// SendAndReceive 是客户端的核心方法。
// 它发送一个UDS请求负载，并阻塞等待响应，同时处理超时。
// 它会自动处理 "Response Pending" (NRC 0x78) 响应。
func (c *Client) SendAndReceive(payload []byte, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return c.SendAndReceiveWithContext(ctx, payload)
}

// SendAndReceiveWithContext 是带有 Context 支持的核心方法。
// 它允许外部取消正在进行的请求。
func (c *Client) SendAndReceiveWithContext(ctx context.Context, payload []byte) ([]byte, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("UDS请求负载不能为空")
	}

	// 1. 在发送前，清空接收队列中可能存在的旧消息。
	// 限制清空次数，避免无限循环
	for i := 0; i < 10; i++ {
		if c.linMaster.ReceiveDiagnostic() == nil {
			break
		}
	}

	// 2. 使用LinMaster发送诊断请求。
	// UDS负载的第一个字节是服务ID (SID)。
	sid := payload[0]
	data := payload[1:]
	c.linMaster.SendDiagnostic(c.config.TargetNad, sid, data)

	// 3. 使用 ticker 进行轮询，支持 Context 取消。
	// 调整轮询间隔为 2ms，平衡响应时间和 CPU 使用率
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			if errors.Is(context.DeadlineExceeded, ctx.Err()) {
				return nil, fmt.Errorf("等待UDS响应超时")
			}
			return nil, fmt.Errorf("操作被取消: %w", ctx.Err())
		case <-ticker.C:
			msg := c.linMaster.ReceiveDiagnostic()
			if msg == nil {
				continue
			}
			if msg.SID == 0x7F {
				if len(msg.Data) >= 2 && msg.Data[0] == sid && msg.Data[1] == 0x78 {
					// Response Pending - 使用新的子 Context 继续等待
					continue
				}
				fullNrcResponse := append([]byte{msg.SID}, msg.Data...)
				return fullNrcResponse, fmt.Errorf("server : %02X 收到否定响应 (NRC: 0x%02X - %s)", msg.SID, msg.Data[1], nrc.GetNrcString(msg.Data[1]))
			}
			if msg.SID == (sid + 0x40) {
				fullPositiveResponse := append([]byte{msg.SID}, msg.Data...)
				return fullPositiveResponse, nil
			}
		}
	}
}
