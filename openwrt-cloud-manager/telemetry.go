package main

import (
	"bufio"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
)

type TelemetryPayload struct {
	Type        string  `json:"type"`
	DeviceID    string  `json:"device_id"`
	MAC         string  `json:"mac"`
	Hostname    string  `json:"hostname"`
	Model       string  `json:"model"`
	Arch        string  `json:"arch"`
	UptimeSec   int64   `json:"uptime_sec"`
	LoadAvg     string  `json:"load_avg"`
	MemFreeKB   int64   `json:"mem_free_kb"`
	MemTotalKB  int64   `json:"mem_total_kb"`
	SessionAuth string  `json:"session_auth,omitempty"`
}

func CollectTelemetry(sessionToken string) TelemetryPayload {
	mac := readPrimaryMAC()
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "OpenWrt"
	}

	return TelemetryPayload{
		Type:        "heartbeat",
		DeviceID:    strings.ReplaceAll(mac, ":", ""),
		MAC:         mac,
		Hostname:    hostname,
		Model:       readDeviceModel(),
		Arch:        readDeviceArch(),
		UptimeSec:   readUptimeSeconds(),
		LoadAvg:     readLoadAverage(),
		MemFreeKB:   readMemInfo("MemAvailable:", "MemFree:"),
		MemTotalKB:  readMemInfo("MemTotal:"),
		SessionAuth: sessionToken,
	}
}

func readPrimaryMAC() string {
	candidates := []string{"/sys/class/net/br-lan/address", "/sys/class/net/eth0/address"}
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil {
			mac := strings.TrimSpace(string(b))
			if mac != "" && mac != "00:00:00:00:00:00" {
				return strings.ToUpper(mac)
			}
		}
	}
	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			if len(iface.HardwareAddr) == 6 {
				return strings.ToUpper(iface.HardwareAddr.String())
			}
		}
	}
	return "00:11:22:33:44:55"
}

func readDeviceModel() string {
	if b, err := os.ReadFile("/tmp/sysinfo/model"); err == nil {
		if m := strings.TrimSpace(string(b)); m != "" {
			return m
		}
	}
	return "Generic OpenWrt Router"
}

func readDeviceArch() string {
	f, err := os.Open("/etc/openwrt_release")
	if err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "DISTRIB_ARCH=") {
				val := strings.TrimPrefix(line, "DISTRIB_ARCH=")
				return strings.Trim(val, "'\"")
			}
		}
	}
	return runtime.GOARCH
}

func readUptimeSeconds() int64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0
	}
	f, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return int64(f)
}

func readLoadAverage() string {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "0.00 0.00 0.00"
	}
	fields := strings.Fields(string(b))
	if len(fields) >= 3 {
		return strings.Join(fields[:3], " ")
	}
	return strings.TrimSpace(string(b))
}

func readMemInfo(keys ...string) int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	values := make(map[string]int64)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 {
			if v, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				values[fields[0]] = v
			}
		}
	}
	for _, k := range keys {
		if v, ok := values[k]; ok && v > 0 {
			return v
		}
	}
	return 0
}
