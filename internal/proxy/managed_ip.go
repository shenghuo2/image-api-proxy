package proxy

import (
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
)

type archiveClientIPKey struct{}
type archiveJobIDKey struct{}

func parseTrustedProxies(cidrs []string, localHostOnly bool) ([]*net.IPNet, error) {
	if localHostOnly {
		cidrs = append(cidrs, "127.0.0.1/32", "::1/128")
		if gateway := dockerGateway(); gateway != nil {
			cidrs = append(cidrs, gateway.String()+"/32")
		}
	}
	var result []*net.IPNet
	for _, raw := range cidrs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		_, network, err := net.ParseCIDR(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy CIDR %q: %w", raw, err)
		}
		result = append(result, network)
	}
	return result, nil
}

func dockerGateway() net.IP {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[1] != "00000000" {
			continue
		}
		b, err := hex.DecodeString(fields[2])
		if err != nil || len(b) != 4 {
			continue
		}
		ip := make(net.IP, 4)
		for i := range b {
			ip[i] = b[3-i]
		}
		return ip
	}
	return nil
}

func (h *ManagedHandler) clientIP(r *http.Request) string {
	if stored, ok := r.Context().Value(archiveClientIPKey{}).(string); ok {
		return stored
	}
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remote = r.RemoteAddr
	}
	ip := net.ParseIP(remote)
	if ip == nil {
		return remote
	}
	trusted := func(candidate net.IP) bool {
		for _, network := range h.trustedProxies {
			if network.Contains(candidate) {
				return true
			}
		}
		return false
	}
	if !trusted(ip) {
		return ip.String()
	}
	forwarded := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
	if forwarded == "" {
		return ip.String()
	}
	parts := strings.Split(forwarded, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := net.ParseIP(strings.TrimSpace(parts[i]))
		if candidate == nil {
			return ip.String()
		}
		if !trusted(candidate) {
			return candidate.String()
		}
	}
	return ip.String()
}
