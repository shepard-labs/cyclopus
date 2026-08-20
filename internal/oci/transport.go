package oci

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// NewHTTPClient returns the only production HTTP client suitable for GHCR.
func NewHTTPClient(timeout time.Duration) (*http.Client, error) {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return newHTTPClient(timeout, net.DefaultResolver.LookupIP, dialer.DialContext)
}

func newHTTPClient(timeout time.Duration, lookupIP func(context.Context, string, string) ([]net.IP, error), dialContext func(context.Context, string, string) (net.Conn, error)) (*http.Client, error) {
	if timeout <= 0 {
		return nil, errors.New("oci: positive HTTP timeout is required")
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, errors.New("oci: registry address is invalid")
			}
			ips, err := lookupIP(ctx, "ip", host)
			if err != nil {
				return nil, errors.New("oci: registry address lookup failed")
			}
			if len(ips) == 0 {
				return nil, errors.New("oci: registry resolved to no addresses")
			}
			for _, ip := range ips {
				if !publicIP(ip) {
					return nil, fmt.Errorf("oci: registry resolved to non-public address")
				}
			}
			connection, err := dialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
			if err != nil {
				return nil, errors.New("oci: registry connection failed")
			}
			return connection, nil
		},
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: timeout,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   4,
		MaxConnsPerHost:       8,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

func publicIP(ip net.IP) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsUnspecified()
}
