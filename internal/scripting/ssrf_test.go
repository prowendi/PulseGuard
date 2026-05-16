package scripting

import (
	"errors"
	"net"
	"strings"
	"testing"
)

func TestIsBlockedIP_DenyTable(t *testing.T) {
	cases := []struct {
		name string
		ip   string
		want bool
	}{
		{"loopback v4", "127.0.0.1", true},
		{"loopback v4 high", "127.255.255.255", true},
		{"loopback v6", "::1", true},
		{"link-local v4", "169.254.1.1", true},
		{"aws metadata", "169.254.169.254", true},
		{"link-local v6", "fe80::1", true},
		{"private 10/8", "10.0.0.1", true},
		{"private 172.16/12 low", "172.16.0.1", true},
		{"private 172.16/12 high", "172.31.255.254", true},
		{"private 192.168/16", "192.168.1.1", true},
		{"private fc00::/7", "fc00::1", true},
		{"unspecified v4", "0.0.0.0", true},
		{"unspecified v6", "::", true},
		{"public v4", "8.8.8.8", false},
		{"public v4 cloudflare", "1.1.1.1", false},
		{"public v6", "2001:4860:4860::8888", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isBlockedIP(net.ParseIP(tc.ip))
			if got != tc.want {
				t.Fatalf("isBlockedIP(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

func TestCheckURL_RejectsBadScheme(t *testing.T) {
	cases := []string{
		"file:///etc/passwd",
		"gopher://example.com",
		"ftp://example.com",
		"data:text/plain,hello",
	}
	for _, raw := range cases {
		_, _, err := CheckURL(raw, nil, true)
		if !errors.Is(err, ErrUnsupportedScheme) {
			t.Fatalf("CheckURL(%q) err = %v, want ErrUnsupportedScheme", raw, err)
		}
	}
}

func TestCheckURL_RejectsPrivateLiteralWithoutDNS(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/",
		"http://10.0.0.1/",
		"http://172.16.0.1/",
		"http://192.168.0.1/",
		"http://169.254.169.254/latest/",
		"http://[::1]/",
		"http://[fe80::1]/",
	}
	for _, raw := range cases {
		_, _, err := CheckURL(raw, nil, true)
		if !errors.Is(err, ErrUnsafeHost) {
			t.Fatalf("CheckURL(%q) err = %v, want ErrUnsafeHost", raw, err)
		}
	}
}

func TestCheckURL_AllowsPublic(t *testing.T) {
	originalLookup := lookupIPs
	defer func() { lookupIPs = originalLookup }()
	lookupIPs = func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil // example.com
	}
	_, _, err := CheckURL("https://example.com/", nil, true)
	if err != nil {
		t.Fatalf("CheckURL public host err = %v", err)
	}
}

func TestCheckURL_DNSFailureBlocks(t *testing.T) {
	originalLookup := lookupIPs
	defer func() { lookupIPs = originalLookup }()
	lookupIPs = func(host string) ([]net.IP, error) {
		return nil, errors.New("no such host")
	}
	_, _, err := CheckURL("https://does-not-exist.example/", nil, true)
	if !errors.Is(err, ErrUnsafeHost) {
		t.Fatalf("err = %v, want ErrUnsafeHost on DNS failure", err)
	}
	if !strings.Contains(err.Error(), "lookup") {
		t.Fatalf("error should mention 'lookup', got %v", err)
	}
}

func TestCheckURL_AllowedSchemeOverride(t *testing.T) {
	originalLookup := lookupIPs
	defer func() { lookupIPs = originalLookup }()
	lookupIPs = func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	_, _, err := CheckURL("https://example.com/", []string{"http"}, true)
	if !errors.Is(err, ErrUnsupportedScheme) {
		t.Fatalf("err = %v, want ErrUnsupportedScheme (https not in allow list)", err)
	}
}

func TestCheckURL_DenyPrivateOffStillResolves(t *testing.T) {
	// When denyPrivate=false, a private IP literal is permitted (for
	// httptest scenarios). The function still requires a parseable URL.
	_, _, err := CheckURL("http://127.0.0.1/", nil, false)
	if err != nil {
		t.Fatalf("err = %v, want nil when denyPrivate is off", err)
	}
}
