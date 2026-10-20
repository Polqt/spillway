package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const doctorBodyLimit = 4 << 10

type doctorErrorBody struct {
	Error struct {
		Type string `json:"type"`
		Code string `json:"code"`
	} `json:"error"`
}

func reportListen(ctx context.Context, status func(string, string, ...any) error, address string, options Options, noProbe bool) error {
	if err := writeDoctorSection(status, "Listen address"); err != nil {
		return err
	}
	if noProbe {
		return status("info", "probe skipped (--no-probe)")
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return status("fail", "cannot inspect configured listen address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return status("fail", "cannot inspect configured listen port")
	}
	client := doctorHTTPClient(options.ProbeTransport)
	for _, target := range doctorProbeHosts(host) {
		if strings.Contains(target, ":") && !ipv6LoopbackAvailable() {
			if err := status("info", "IPv6 loopback probe skipped: IPv6 loopback is unavailable"); err != nil {
				return err
			}
			continue
		}
		result, err := probeSpillway(ctx, client, net.JoinHostPort(target, portText), isLoopbackHost(host))
		if err != nil {
			if errors.Is(err, syscall.ECONNREFUSED) || safeNetworkError(err) == "connection refused" {
				if err := status("info", "%s: nothing listening", net.JoinHostPort(target, portText)); err != nil {
					return err
				}
			} else if writeErr := status("warn", "%s: probe failed (%s)", net.JoinHostPort(target, portText), safeNetworkError(err)); writeErr != nil {
				return writeErr
			}
			continue
		}
		if result.spillway {
			if err := status("info", "%s: answers like Spillway; compare this config's providers with the provider list in the serve startup summary", net.JoinHostPort(target, portText)); err != nil {
				return err
			}
		} else {
			if err := status("fail", "%s: not Spillway (HTTP %d, %s)", net.JoinHostPort(target, portText), result.status, result.contentType); err != nil {
				return err
			}
			if err := status("info", "%s", doctorPortGuidance(options.GOOS, port)); err != nil {
				return err
			}
		}
	}
	ln, err := net.Listen("tcp", address)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) || addressInUse(err) {
			return status("warn", "cannot bind %s: address already in use", address)
		}
		return status("warn", "cannot bind configured address: %s", safeNetworkError(err))
	}
	if err := ln.Close(); err != nil {
		return status("warn", "could not close temporary listen probe: %s", safeNetworkError(err))
	}
	return status("ok", "configured listen address can be bound")
}

func doctorHTTPClient(transport http.RoundTripper) *http.Client {
	if transport == nil {
		transport = &http.Transport{Proxy: nil, DisableKeepAlives: true, ResponseHeaderTimeout: 2 * time.Second}
	}
	return &http.Client{
		Transport:     transport,
		Timeout:       2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type probeResult struct {
	spillway    bool
	status      int
	contentType string
}

func probeSpillway(ctx context.Context, client *http.Client, address string, requireHostCheck bool) (probeResult, error) {
	base := "http:" + "/" + "/" + address
	checks := []func(context.Context) (bool, int, string, error){
		func(ctx context.Context) (bool, int, string, error) {
			response, body, err := doctorGet(ctx, client, base+"/healthz", "")
			if err != nil {
				return false, 0, "", err
			}
			return response.StatusCode == http.StatusOK && response.Header.Get("Content-Type") == "text/plain; charset=utf-8" && string(body) == "ok", response.StatusCode, response.Header.Get("Content-Type"), nil
		},
		func(ctx context.Context) (bool, int, string, error) {
			response, body, err := doctorGet(ctx, client, base+"/v1/models", "")
			if err != nil {
				return false, 0, "", err
			}
			var payload doctorErrorBody
			valid := response.StatusCode == http.StatusUnauthorized && strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "application/json") && json.Unmarshal(body, &payload) == nil && payload.Error.Type == "authentication_error" && payload.Error.Code == "invalid_api_key"
			return valid, response.StatusCode, response.Header.Get("Content-Type"), nil
		},
	}
	if requireHostCheck {
		checks = append(checks, func(ctx context.Context) (bool, int, string, error) {
			response, body, err := doctorGet(ctx, client, base+"/healthz", "doctor.invalid")
			if err != nil {
				return false, 0, "", err
			}
			var payload doctorErrorBody
			valid := response.StatusCode == http.StatusForbidden && strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "application/json") && json.Unmarshal(body, &payload) == nil && payload.Error.Code == "host_not_allowed"
			return valid, response.StatusCode, response.Header.Get("Content-Type"), nil
		})
	}
	result := probeResult{spillway: true}
	for _, check := range checks {
		valid, code, contentType, err := check(ctx)
		if err != nil {
			return probeResult{}, err
		}
		if !valid {
			result.spillway = false
			result.status = code
			result.contentType = safeContentType(contentType)
			return result, nil
		}
	}
	return result, nil
}

func doctorGet(ctx context.Context, client *http.Client, target, host string) (*http.Response, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, nil, err
	}
	if host != "" {
		request.Host = host
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, doctorBodyLimit))
	if err != nil {
		return nil, nil, err
	}
	return response, body, nil
}

func doctorProbeHosts(host string) []string {
	switch strings.Trim(host, "[]") {
	case "", "0.0.0.0", "::":
		return []string{"127.0.0.1", "::1"}
	case "127.0.0.1", "localhost":
		return []string{strings.Trim(host, "[]"), "::1"}
	default:
		return []string{strings.Trim(host, "[]")}
	}
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func ipv6LoopbackAvailable() bool {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

func mediaType(value string) string {
	value, _, _ = strings.Cut(value, ";")
	return strings.TrimSpace(value)
}

func safeContentType(value string) string {
	if value == "" {
		return "no content type"
	}
	if len(value) > 80 {
		return "unusual content type"
	}
	return value
}

func safeNetworkError(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if strings.Contains(strings.ToLower(opErr.Err.Error()), "refused") {
			return "connection refused"
		}
		if strings.Contains(strings.ToLower(opErr.Err.Error()), "timeout") {
			return "timed out"
		}
		return "network error"
	}
	return "request failed"
}

func doctorPortGuidance(goos string, port int) string {
	owner := fmt.Sprintf("ss -ltnp 'sport = :%d'", port)
	switch goos {
	case "windows":
		owner = fmt.Sprintf("Get-NetTCPConnection -LocalPort %d -State Listen", port)
	case "darwin":
		owner = fmt.Sprintf("lsof -nP -iTCP:%d -sTCP:LISTEN", port)
	}
	return fmt.Sprintf("check the listener with `%s` or `docker ps --filter publish=%d`; try `127.0.0.1:8787` and set client apps to `%s127.0.0.1:8787/v1`", owner, port, "http:"+"/"+"/")
}
