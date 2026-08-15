//go:build darwin && cgo

package main

import "testing"

func TestUSBNetMode(t *testing.T) {
	for input, want := range map[string]string{
		"AT+QCFG=\"usbnet\"\r\n+QCFG: \"usbnet\",0\r\nOK": "0",
		"+QCFG: \"usbnet\",1\r\nOK":                       "1",
		"ERROR":                                           "",
	} {
		if got := usbnetMode(input); got != want {
			t.Fatalf("usbnetMode(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestParseHardwarePorts(t *testing.T) {
	ports := parseHardwarePorts("Hardware Port: EG25G-QDC507\nDevice: en9\nEthernet Address: 00:00:00:00:00:00\n")
	if len(ports) != 1 || ports[0].name != "EG25G-QDC507" || ports[0].device != "en9" {
		t.Fatalf("unexpected ports: %#v", ports)
	}
}

func TestParseNetworkServices(t *testing.T) {
	input := `(1) EG25G-QDC507
(Hardware Port: EG25G-QDC507, Device: en9)

(*) Baiwang
(Hardware Port: Baiwang, Device: en11)`
	services := parseNetworkServices(input)
	if len(services) != 2 || services[0].device != "en9" || !services[1].disabled {
		t.Fatalf("unexpected services: %#v", services)
	}
}

func TestSelectCurrentModemNetworkServicePrefersPresentECMInterface(t *testing.T) {
	services := []networkService{
		{name: "Baiwang", port: "Baiwang", device: "en9"},
		{name: "EG25G-QDC507", port: "EG25G-QDC507", device: "en11", disabled: true},
	}
	ports := []hardwarePort{
		{name: "EG25G-QDC507", device: "en11"},
	}

	service := selectCurrentModemNetworkService(services, ports)
	if service == nil {
		t.Fatal("expected a network service")
	}
	if service.name != "EG25G-QDC507" || service.device != "en11" {
		t.Fatalf("unexpected service: %#v", service)
	}
	if !service.disabled || !service.present {
		t.Fatalf("service state was not preserved: %#v", service)
	}
}

func TestSelectCurrentModemNetworkServiceReturnsNilWithoutCompatibleService(t *testing.T) {
	services := []networkService{
		{name: "Wi-Fi", port: "Wi-Fi", device: "en0"},
	}
	ports := []hardwarePort{
		{name: "Wi-Fi", device: "en0"},
	}

	if service := selectCurrentModemNetworkService(services, ports); service != nil {
		t.Fatalf("unexpected service: %#v", service)
	}
}

func TestResponseParsing(t *testing.T) {
	if !responseFinished("AT\r\r\nOK\r\n") || !responseOK("AT\r\nOK") {
		t.Fatal("OK response should be complete and successful")
	}
	if !responseFinished("\r\n+CME ERROR: 3\r\n") {
		t.Fatal("CME error response should be complete")
	}
}
