package config

import (
	"net/url"
	"strings"
)

// validateProxyURL validates a single proxy URL. The scheme must be http,
// https, socks5, or socks5h, and the host must be non-empty.
func validateProxyURL(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return wrapf(ErrInvalidProxyURL, "", "empty proxy url")
	}
	u, err := url.Parse(s)
	if err != nil {
		return wrapf(ErrInvalidProxyURL, "", "proxy url %q: %v", s, err)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return wrapf(ErrInvalidProxyURL, "", "proxy url %q has unsupported scheme %q (want http(s):// or socks5(h)://)", s, u.Scheme)
	}
	if u.Host == "" {
		return wrapf(ErrInvalidProxyURL, "", "proxy url %q missing host", s)
	}
	return nil
}

// validateBaseURL validates a provider base_url. It must be an absolute
// http(s):// URL. The trailing slash is stripped by the caller after
// validation.
func validateBaseURL(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return wrapf(ErrInvalidBaseURL, "", "base_url is empty")
	}
	u, err := url.Parse(s)
	if err != nil {
		return wrapf(ErrInvalidBaseURL, "", "base_url %q: %v", s, err)
	}
	if !u.IsAbs() {
		return wrapf(ErrInvalidBaseURL, "", "base_url %q must be absolute", s)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return wrapf(ErrInvalidBaseURL, "", "base_url %q must be http(s)://", s)
	}
	if u.Host == "" {
		return wrapf(ErrInvalidBaseURL, "", "base_url %q missing host", s)
	}
	return nil
}
