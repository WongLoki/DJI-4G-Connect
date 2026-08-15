//go:build darwin && cgo

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"
)

const (
	activationWaitTimeout  = 40 * time.Second
	networkServiceDHCPWait = 15 * time.Second
	networkServiceAuthWait = 90 * time.Second
)

var (
	errNetworkServiceDHCPTimeout = errors.New("macOS 网络服务等待 DHCP 超时")
)

type networkService struct {
	name     string
	port     string
	device   string
	disabled bool
	present  bool
}

type hardwarePort struct {
	name   string
	device string
}

func activateDJINetwork(output io.Writer) (string, error) {
	if output == nil {
		output = io.Discard
	}
	present, ecm := moduleUSBState()
	if !present {
		return "", errors.New("未检测到受支持的 DJI 4G 模块（USB 2ca3:4006）")
	}
	_, _ = fmt.Fprintln(output, "已检测到受支持的 DJI 4G 模块（USB 2ca3:4006）")

	cleanupResult := removeOrphanedModemServices(output)
	if device, address := activeModemLink(); device != "" {
		message := fmt.Sprintf("4G 网卡已可用：%s，IP %s", device, address)
		if cleanupResult != "" {
			message += "；" + cleanupResult
		}
		return message, nil
	}
	if ecm {
		service := currentModemNetworkService()
		if service != nil && service.present {
			device, address, repairErr := repairNetworkService(output, service, networkServiceDHCPWait)
			if repairErr == nil {
				message := fmt.Sprintf("4G 网卡已恢复：%s，IP %s", device, address)
				if cleanupResult != "" {
					message += "；" + cleanupResult
				}
				return message, nil
			}
			if !errors.Is(repairErr, errNetworkServiceDHCPTimeout) {
				return "", repairErr
			}
			_, _ = fmt.Fprintln(output, "macOS 网络服务已经启用，但尚未获得地址；将重启模块后再次尝试。")
		}
	}

	channel, err := openModemChannel()
	if err != nil {
		return "", fmt.Errorf("打开模块控制通道失败: %w", err)
	}
	defer channel.Close()

	reply, err := channel.Send(`AT+QCFG="usbnet"`, 3*time.Second)
	if err != nil {
		return "", fmt.Errorf("读取 USB 上网模式失败: %w", err)
	}
	mode := usbnetMode(reply)
	if mode != "1" {
		_, _ = fmt.Fprintf(output, "当前 usbnet=%s，正在切换到 ECM 模式…\n", modeLabel(mode))
		reply, err = channel.Send(`AT+QCFG="usbnet",1`, 5*time.Second)
		if err != nil || !responseOK(reply) {
			if err != nil {
				return "", fmt.Errorf("写入 ECM 模式失败: %w", err)
			}
			return "", fmt.Errorf("模块拒绝 ECM 模式设置: %q", reply)
		}
	} else {
		_, _ = fmt.Fprintln(output, "模块已保存 ECM 上网模式。")
	}

	_, _ = fmt.Fprintln(output, "正在重启模块，让 macOS 重新识别网卡…")
	reply, err = channel.Send("AT+CFUN=1,1", 4*time.Second)
	if err != nil && !disconnectDuringRestart(err) {
		return "", fmt.Errorf("重启模块失败: %w", err)
	}
	if err == nil && !responseOK(reply) {
		return "", fmt.Errorf("模块拒绝重启指令: %q", reply)
	}
	channel.Close()

	deadline := time.Now().Add(activationWaitTimeout)
	repairAttempted := false
	for time.Now().Before(deadline) {
		if device, address := activeModemLink(); device != "" {
			removeOrphanedModemServices(output)
			return fmt.Sprintf("连接完成：%s，IP %s", device, address), nil
		}
		if !repairAttempted {
			_, ecm = moduleUSBState()
			service := currentModemNetworkService()
			if ecm && service != nil && service.present {
				repairAttempted = true
				wait := minDuration(networkServiceDHCPWait, time.Until(deadline))
				device, address, repairErr := repairNetworkService(output, service, wait)
				if repairErr == nil {
					removeOrphanedModemServices(output)
					return fmt.Sprintf("连接完成：%s，IP %s", device, address), nil
				}
				if !errors.Is(repairErr, errNetworkServiceDHCPTimeout) {
					return "", repairErr
				}
			}
		}
		time.Sleep(time.Second)
	}
	return "", fmt.Errorf("模块已经重启，但 %s 内没有获得 ECM 网卡地址", activationWaitTimeout)
}

func usbnetMode(reply string) string {
	compact := strings.ReplaceAll(reply, " ", "")
	needle := `+qcfg:"usbnet",`
	start := strings.Index(strings.ToLower(compact), needle)
	if start < 0 {
		return ""
	}
	tail := compact[start+len(needle):]
	for index, char := range tail {
		if char < '0' || char > '9' {
			return tail[:index]
		}
	}
	return tail
}

func modeLabel(mode string) string {
	if mode == "" {
		return "未知"
	}
	return mode
}

func disconnectDuringRestart(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToUpper(err.Error())
	return strings.Contains(message, "NO_DEVICE") || strings.Contains(message, "DISCONNECT")
}

func activeModemLink() (string, string) {
	output, err := exec.Command("networksetup", "-listallhardwareports").Output()
	if err != nil {
		return "", ""
	}
	for _, port := range parseHardwarePorts(string(output)) {
		if !modemIdentity(port.name) || port.device == "" {
			continue
		}
		addressBytes, err := exec.Command("ipconfig", "getifaddr", port.device).Output()
		address := strings.TrimSpace(string(addressBytes))
		if err != nil || !strings.HasPrefix(address, "192.168.225.") {
			continue
		}
		state, err := exec.Command("ifconfig", port.device).Output()
		if err == nil && strings.Contains(string(state), "status: active") {
			return port.device, address
		}
	}
	return "", ""
}

func currentModemNetworkService() *networkService {
	serviceBytes, serviceErr := exec.Command("networksetup", "-listnetworkserviceorder").Output()
	portBytes, portErr := exec.Command("networksetup", "-listallhardwareports").Output()
	if serviceErr != nil || portErr != nil {
		return nil
	}
	return selectCurrentModemNetworkService(
		parseNetworkServices(string(serviceBytes)),
		parseHardwarePorts(string(portBytes)),
	)
}

func selectCurrentModemNetworkService(services []networkService, ports []hardwarePort) *networkService {
	presentDevices := make(map[string]bool, len(ports))
	for _, port := range ports {
		if port.device != "" {
			presentDevices[port.device] = true
		}
	}

	type candidate struct {
		service networkService
		score   int
	}
	var candidates []candidate
	for _, service := range services {
		if !modemIdentity(service.name+" "+service.port) || service.device == "" {
			continue
		}
		service.present = presentDevices[service.device]
		score := 0
		if service.present {
			score += 100
		}
		identity := strings.ToLower(service.name + " " + service.port)
		if strings.Contains(identity, "eg25") || strings.Contains(identity, "qdc507") {
			score += 20
		}
		if !service.disabled {
			score++
		}
		candidates = append(candidates, candidate{service: service, score: score})
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.SliceStable(candidates, func(left int, right int) bool {
		if candidates[left].score == candidates[right].score {
			return candidates[left].service.name < candidates[right].service.name
		}
		return candidates[left].score > candidates[right].score
	})
	selected := candidates[0].service
	return &selected
}

func repairNetworkService(output io.Writer, service *networkService, timeout time.Duration) (string, string, error) {
	if service == nil || service.name == "" || service.device == "" {
		return "", "", errors.New("未找到当前模块对应的 macOS 网络服务")
	}
	if address := activeInterfaceAddress(service.device); address != "" && !service.disabled {
		return service.device, address, nil
	}

	action := "正在让 macOS 重新请求 DHCP 地址"
	if service.disabled {
		action = "正在启用 macOS 网络服务并请求 DHCP 地址"
	}
	_, _ = fmt.Fprintf(output, "%s：%s（%s）…\n", action, service.name, service.device)
	if err := enableNetworkService(service.name); err != nil {
		return "", "", err
	}

	deadline := time.Now().Add(timeout)
	for {
		current := currentModemNetworkService()
		if current != nil && current.name == service.name && !current.disabled {
			if address := activeInterfaceAddress(current.device); address != "" {
				return current.device, address, nil
			}
		}
		if time.Now().After(deadline) {
			return "", "", fmt.Errorf("%w：%s（%s）", errNetworkServiceDHCPTimeout, service.name, service.device)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func enableNetworkService(serviceName string) error {
	serviceName = strings.TrimSpace(serviceName)
	if serviceName == "" {
		return errors.New("macOS 网络服务名称为空")
	}
	const (
		script = `on run argv
set serviceName to item 1 of argv
do shell script "/usr/sbin/networksetup -setnetworkserviceenabled " & quoted form of serviceName & " on && /usr/sbin/networksetup -setdhcp " & quoted form of serviceName with administrator privileges
end run`
	)
	ctx, cancel := context.WithTimeout(context.Background(), networkServiceAuthWait)
	defer cancel()
	response, err := exec.CommandContext(ctx, "osascript", "-e", script, "--", serviceName).CombinedOutput()
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(response))
	if detail == "" {
		detail = err.Error()
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("等待 macOS 管理员授权超时")
	}
	return fmt.Errorf("启用 macOS 网络服务失败: %s", detail)
}

func activeInterfaceAddress(device string) string {
	addressBytes, err := exec.Command("ipconfig", "getifaddr", device).Output()
	address := strings.TrimSpace(string(addressBytes))
	if err != nil || !strings.HasPrefix(address, "192.168.225.") {
		return ""
	}
	state, err := exec.Command("ifconfig", device).Output()
	if err != nil || !strings.Contains(string(state), "status: active") {
		return ""
	}
	return address
}

func removeOrphanedModemServices(output io.Writer) string {
	serviceBytes, serviceErr := exec.Command("networksetup", "-listnetworkserviceorder").Output()
	portBytes, portErr := exec.Command("networksetup", "-listallhardwareports").Output()
	if serviceErr != nil || portErr != nil {
		_, _ = fmt.Fprintln(output, "警告：无法读取 macOS 网络服务配置。")
		return ""
	}
	present := make(map[string]bool)
	for _, port := range parseHardwarePorts(string(portBytes)) {
		if port.device != "" {
			present[port.device] = true
		}
	}
	var candidates []networkService
	for _, service := range parseNetworkServices(string(serviceBytes)) {
		if !service.disabled && modemIdentity(service.name+" "+service.port) && service.device != "" && !present[service.device] {
			candidates = append(candidates, service)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].name < candidates[j].name })

	var completed []string
	for _, service := range candidates {
		_, removeErr := exec.Command("networksetup", "-removenetworkservice", service.name).CombinedOutput()
		if removeErr == nil {
			_, _ = fmt.Fprintf(output, "已移除残留网络服务：%s（%s）\n", service.name, service.device)
			completed = append(completed, "已清理 "+service.name)
			continue
		}
		if !service.disabled {
			if _, disableErr := exec.Command("networksetup", "-setnetworkserviceenabled", service.name, "off").CombinedOutput(); disableErr == nil {
				_, _ = fmt.Fprintf(output, "已禁用无法移除的残留服务：%s（%s）\n", service.name, service.device)
				completed = append(completed, "已禁用 "+service.name)
				continue
			}
		}
		_, _ = fmt.Fprintf(output, "警告：无法处理残留网络服务 %s。\n", service.name)
	}
	return strings.Join(completed, "、")
}

func parseHardwarePorts(value string) []hardwarePort {
	var result []hardwarePort
	var current *hardwarePort
	scanner := bufio.NewScanner(strings.NewReader(value))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "Hardware Port:"):
			result = append(result, hardwarePort{name: strings.TrimSpace(strings.TrimPrefix(line, "Hardware Port:"))})
			current = &result[len(result)-1]
		case current != nil && strings.HasPrefix(line, "Device:"):
			current.device = strings.TrimSpace(strings.TrimPrefix(line, "Device:"))
		}
	}
	return result
}

func parseNetworkServices(value string) []networkService {
	var result []networkService
	var current *networkService
	scanner := bufio.NewScanner(strings.NewReader(value))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "(") && !strings.HasPrefix(line, "(Hardware Port:") {
			closeIndex := strings.Index(line, ")")
			if closeIndex > 0 {
				prefix := line[1:closeIndex]
				name := strings.TrimSpace(line[closeIndex+1:])
				if name != "" {
					result = append(result, networkService{name: name, disabled: prefix == "*"})
					current = &result[len(result)-1]
				}
			}
			continue
		}
		if current == nil || !strings.HasPrefix(line, "(Hardware Port:") {
			continue
		}
		detail := strings.TrimSuffix(strings.TrimPrefix(line, "(Hardware Port:"), ")")
		separator := strings.LastIndex(detail, ", Device:")
		if separator >= 0 {
			current.port = strings.TrimSpace(detail[:separator])
			current.device = strings.TrimSpace(detail[separator+len(", Device:"):])
		}
	}
	return result
}

func modemIdentity(value string) bool {
	value = strings.ToLower(value)
	return strings.Contains(value, "baiwang") ||
		strings.Contains(value, "eg25") ||
		strings.Contains(value, "qdc507")
}
