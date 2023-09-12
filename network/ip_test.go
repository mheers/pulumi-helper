package network

import "testing"

func TestPublicIP(t *testing.T) {
	ip, err := PublicIP()
	if err != nil {
		t.Error(err)
	}
	t.Log(ip)
}
