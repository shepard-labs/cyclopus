package oci

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNewHTTPClientRejectsNonpositiveTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		if _, err := NewHTTPClient(timeout); err == nil {
			t.Fatalf("NewHTTPClient(%v) succeeded", timeout)
		}
	}
}

func TestNewHTTPClientTransportPolicy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://user:secret@proxy.example")
	client, err := NewHTTPClient(7 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T", client.Transport)
	}
	if transport.Proxy != nil || client.Timeout != 7*time.Second || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 ||
		transport.ResponseHeaderTimeout != 7*time.Second || transport.TLSHandshakeTimeout != 5*time.Second || transport.IdleConnTimeout != 30*time.Second ||
		transport.MaxIdleConns != 8 || transport.MaxIdleConnsPerHost != 4 || transport.MaxConnsPerHost != 8 {
		t.Fatalf("unexpected transport policy: %#v", transport)
	}
}

func TestHTTPClientDialRejectsNonPublicAndMixedAddresses(t *testing.T) {
	tests := []struct {
		name string
		ips  []net.IP
	}{
		{"empty", nil},
		{"private", []net.IP{net.ParseIP("10.0.0.1")}},
		{"loopback", []net.IP{net.ParseIP("127.0.0.1")}},
		{"link_local", []net.IP{net.ParseIP("169.254.1.1")}},
		{"unspecified", []net.IP{net.ParseIP("0.0.0.0")}},
		{"mixed", []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("10.0.0.1")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dialed := false
			client, err := newHTTPClient(time.Second, func(context.Context, string, string) ([]net.IP, error) { return test.ips, nil }, func(context.Context, string, string) (net.Conn, error) {
				dialed = true
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Transport.(*http.Transport).DialContext(context.Background(), "tcp", "registry.example:443")
			if err == nil || (!strings.Contains(err.Error(), "non-public") && !strings.Contains(err.Error(), "no addresses")) || dialed {
				t.Fatalf("DialContext() error = %v, dialed=%t", err, dialed)
			}
		})
	}
}

func TestHTTPClientDialUsesFirstPublicAddress(t *testing.T) {
	var network, address string
	client, err := newHTTPClient(time.Second, func(_ context.Context, network, host string) ([]net.IP, error) {
		if network != "ip" || host != "registry.example" {
			t.Fatalf("lookup = %q %q", network, host)
		}
		return []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("1.1.1.1")}, nil
	}, func(_ context.Context, gotNetwork, gotAddress string) (net.Conn, error) {
		network, address = gotNetwork, gotAddress
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	connection, err := client.Transport.(*http.Transport).DialContext(context.Background(), "tcp", "registry.example:443")
	if err != nil || connection != nil || network != "tcp" || address != "8.8.8.8:443" {
		t.Fatalf("DialContext() connection=%v err=%v network=%q address=%q", connection, err, network, address)
	}
}

func TestHTTPClientDialErrorsDoNotLeakSensitiveResolverOrDialData(t *testing.T) {
	secret := "credential-and-resource-secret"
	for name, test := range map[string]struct {
		lookupIP    func(context.Context, string, string) ([]net.IP, error)
		dialContext func(context.Context, string, string) (net.Conn, error)
	}{
		"lookup": {func(context.Context, string, string) ([]net.IP, error) { return nil, errors.New(secret) }, nil},
		"dial":   {func(context.Context, string, string) ([]net.IP, error) { return []net.IP{net.ParseIP("8.8.8.8")}, nil }, func(context.Context, string, string) (net.Conn, error) { return nil, errors.New(secret) }},
	} {
		t.Run(name, func(t *testing.T) {
			client, err := newHTTPClient(time.Second, test.lookupIP, test.dialContext)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Transport.(*http.Transport).DialContext(context.Background(), "tcp", "registry.example:443")
			if err == nil || strings.Contains(err.Error(), secret) {
				t.Fatalf("DialContext() error = %v", err)
			}
		})
	}
	client, err := newHTTPClient(time.Second, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Transport.(*http.Transport).DialContext(context.Background(), "tcp", secret)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("DialContext() error = %v", err)
	}
}
