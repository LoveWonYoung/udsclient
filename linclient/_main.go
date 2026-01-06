/*
 * UdsToolsLin - LIN 总线 UDS 诊断工具
 *
 * 本示例展示如何使用 uds_client 包进行 UDS 诊断通信。
 * 在 Darwin/Linux 环境下使用 MockDriver 进行模拟测试。
 * 在 Windows 环境下可使用 ToomossDriver 或 PcanLin 连接实际硬件。
 */
package main

import (
	"context"
	"log"
	"time"

	"github.com/LoveWonYoung/lintp/driver"

	"github.com/LoveWonYoung/lintp/tp"
	"github.com/LoveWonYoung/lintp/uds_client"
)

func main() {

	mockDriver := driver.NewMockDriver()

	// 设置模拟 ECU 的响应处理器
	setupMockECU(mockDriver)

	// 创建 UDS 客户端
	// 目标 NAD = 0x7F (广播地址，用于演示)
	client := uds_client.NewClient(mockDriver, 0x7F)
	defer client.Close()

	log.Println("\n--- 1. 诊断会话控制 (0x10) ---")
	demonstrateDiagnosticSession(client)

	log.Println("\n--- 2. 读取数据标识符 (0x22) ---")
	demonstrateReadDataByIdentifier(client)

	log.Println("\n--- 3. 带 Context 的请求 ---")
	demonstrateContextUsage(client)

	log.Println("\n--- 4. 否定响应处理 ---")
	demonstrateNegativeResponse(client)

	log.Println("\n=== 示例完成 ===")
}

// setupMockECU 配置模拟 ECU 的响应
// 这个简化版本使用 InjectEvent 来模拟响应
func setupMockECU(mockDriver *driver.MockDriver) {
	// 启动一个 goroutine 来处理请求并生成响应
	go func() {
		for {
			// 检查发送记录，解析请求并注入响应
			txLog := mockDriver.GetTxLog()
			if len(txLog) > 0 {
				lastTx := txLog[len(txLog)-1]
				if lastTx.EventID == tp.MasterDiagnosticFrameID {
					response := processUDSRequest(lastTx.EventPayload)
					if response != nil {
						mockDriver.InjectEvent(response)
					}
					mockDriver.ClearTxLog()
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

// processUDSRequest 解析 UDS 请求并生成响应
func processUDSRequest(payload []byte) *tp.LinEvent {
	if len(payload) < 3 {
		return nil
	}

	nad := payload[0]
	pci := payload[1]
	sid := payload[2]

	// 只处理单帧请求
	if (pci >> 4) != 0 {
		return nil
	}

	response := make([]byte, 8)
	for i := range response {
		response[i] = 0xFF
	}
	response[0] = nad

	switch sid {
	case 0x10: // DiagnosticSessionControl
		if len(payload) >= 4 {
			sessionType := payload[3]
			// 正响应: 0x50, sessionType, P2ServerMax, P2*ServerMax
			response[1] = 0x06 // PCI: SF, length=6
			response[2] = 0x50 // 正响应 SID
			response[3] = sessionType
			response[4] = 0x00
			response[5] = 0x32 // P2 = 50ms
			response[6] = 0x01
			response[7] = 0xF4 // P2* = 5000ms
		}

	case 0x22: // ReadDataByIdentifier
		if len(payload) >= 5 {
			did1, did2 := payload[3], payload[4]
			if did1 == 0xF1 && did2 == 0x89 {
				// 正响应
				response[1] = 0x05 // PCI: SF, length=5
				response[2] = 0x62 // 正响应 SID
				response[3] = did1
				response[4] = did2
				response[5] = 0x01 // 版本数据
				response[6] = 0x00
			} else if did1 == 0xFF && did2 == 0xFF {
				// 否定响应 - 请求超出范围
				response[1] = 0x03 // PCI: SF, length=3
				response[2] = 0x7F // NRC SID
				response[3] = sid
				response[4] = 0x31 // RequestOutOfRange
			} else {
				// 其他 DID - 正响应
				response[1] = 0x05
				response[2] = 0x62
				response[3] = did1
				response[4] = did2
				response[5] = 0xAB
				response[6] = 0xCD
			}
		}

	default:
		// 服务不支持
		response[1] = 0x03
		response[2] = 0x7F
		response[3] = sid
		response[4] = 0x11 // ServiceNotSupported
	}

	return &tp.LinEvent{
		EventID:      tp.SlaveDiagnosticFrameID,
		EventPayload: response,
		Direction:    tp.RX,
		Timestamp:    time.Now(),
	}
}

// demonstrateDiagnosticSession 演示诊断会话控制服务
func demonstrateDiagnosticSession(client *uds_client.Client) {
	// 请求切换到扩展诊断会话 (0x03)
	request := []byte{0x10, 0x03}
	log.Printf("发送请求: % 02X", request)

	response, err := client.SendAndReceive(request, 2*time.Second)
	if err != nil {
		log.Printf("❌ 请求失败: %v", err)
		return
	}

	log.Printf("✅ 收到响应: % 02X", response)
	if response[0] == 0x50 {
		log.Printf("   会话类型: 0x%02X (扩展诊断会话)", response[1])
	}
}

// demonstrateReadDataByIdentifier 演示读取数据标识符服务
func demonstrateReadDataByIdentifier(client *uds_client.Client) {
	// 读取 ECU 软件版本 (DID: 0xF189)
	request := []byte{0x22, 0xF1, 0x89}
	log.Printf("发送请求: % 02X (读取 DID 0xF189)", request)

	response, err := client.SendAndReceive(request, 2*time.Second)
	if err != nil {
		log.Printf("❌ 请求失败: %v", err)
		return
	}

	log.Printf("✅ 收到响应: % 02X", response)
	if response[0] == 0x62 {
		log.Printf("   DID: 0x%02X%02X", response[1], response[2])
		log.Printf("   数据: % 02X", response[3:])
	}
}

// demonstrateContextUsage 演示使用 Context 控制超时
func demonstrateContextUsage(client *uds_client.Client) {
	// 创建一个 500ms 超时的 Context
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	request := []byte{0x22, 0xF1, 0x90}
	log.Printf("发送请求 (500ms 超时): % 02X", request)

	response, err := client.SendAndReceiveWithContext(ctx, request)
	if err != nil {
		log.Printf("⏱️ 预期的超时或错误: %v", err)
		return
	}

	log.Printf("✅ 收到响应: % 02X", response)
}

// demonstrateNegativeResponse 演示否定响应处理
func demonstrateNegativeResponse(client *uds_client.Client) {
	// 发送一个会被拒绝的请求
	request := []byte{0x22, 0xFF, 0xFF} // 无效的 DID
	log.Printf("发送无效请求: % 02X", request)

	response, err := client.SendAndReceive(request, 2*time.Second)
	if err != nil {
		log.Printf("⚠️ 收到否定响应: %v", err)
		if response != nil {
			log.Printf("   响应数据: % 02X", response)
			if response[0] == 0x7F && len(response) >= 3 {
				log.Printf("   NRC: 0x%02X - %s", response[2], uds_client.GetNrcString(response[2]))
			}
		}
		return
	}

	log.Printf("✅ 收到响应: % 02X", response)
}
