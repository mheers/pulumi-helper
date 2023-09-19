package network

import (
	"io"
	"net/http"
	"strings"
)

// PublicIP returns the public ip of the caller
func PublicIP() (string, error) {
	// my public ip
	resp, err := http.Get("https://ipv4.icanhazip.com")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	ip := string(body)
	ip = strings.ReplaceAll(ip, "\n", "")
	return ip, nil
}
