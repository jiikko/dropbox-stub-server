package main

import "testing"

func TestRequireLoopback(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:8765": true,
		"[::1]:8765":     true,
		"localhost:0":    true,
		"0.0.0.0:8765":   false,
		":8765":          false,
		"192.168.1.2:80": false,
		"example.com:80": false,
		"127.0.0.1":      false, // no port
	} {
		if err := requireLoopback(addr); (err == nil) != ok {
			t.Errorf("requireLoopback(%q) err = %v, want ok=%v", addr, err, ok)
		}
	}
}
