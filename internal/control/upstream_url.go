package control

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"embyrelay/internal/proxy"
)

func normalizeUpstreamURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("请输入 Emby 地址")
	}
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	} else if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := proxy.ValidateURL(raw)
	if err != nil {
		return "", err
	}
	port := u.Port()
	if port == "" {
		if strings.HasSuffix(u.Host, ":") {
			return "", fmt.Errorf("端口不能为空")
		}
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	} else {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("端口必须为 1–65535")
		}
	}
	u.Host = net.JoinHostPort(u.Hostname(), port)
	return u.String(), nil
}
