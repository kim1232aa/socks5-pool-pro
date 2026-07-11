package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxProxyIPVerifyConcurrent = 8
	proxyIPVerifyTimeout       = 12 * time.Second
	maxProxyIPVerifyBodyBytes  = 64 << 10
	maxProxyIPVerifyInputBytes = 4 << 10
	proxyIPVerifySource        = "api.090227.xyz"
)

// proxyIPVerifyEndpoint is deliberately the only replaceable part of the
// outbound target. Production always calls the fixed Cloudflare ProxyIP probe;
// tests replace this variable with an httptest server.
var proxyIPVerifyEndpoint = "https://api.090227.xyz/check"

var proxyIPVerifyHTTPClient = &http.Client{
	// A redirect would turn the otherwise-fixed outbound request into a target
	// chosen by another server. Treat it as a non-2xx result instead.
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

type proxyIPVerifyRequest struct {
	Key     string `json:"key"`
	Address string `json:"address"`
}

type proxyIPVerifyResult struct {
	Success        bool   `json:"success"`
	ResponseTimeMs int64  `json:"response_time_ms"`
	SupportsIPv4   bool   `json:"supports_ipv4"`
	SupportsIPv6   bool   `json:"supports_ipv6"`
	Source         string `json:"source"`
	CheckedAt      string `json:"checked_at"`
}

type proxyIPVerifyCall struct {
	done   chan struct{}
	result proxyIPVerifyResult
	err    error
}

type proxyIPProbeResponse struct {
	Success      *bool           `json:"success"`
	ResponseTime json.RawMessage `json:"responseTime"`
	SupportsIPv4 *bool           `json:"supports_ipv4"`
	SupportsIPv6 *bool           `json:"supports_ipv6"`
}

func (s *StatusServer) handleProxyIPVerify(w http.ResponseWriter, r *http.Request) {
	input, err := decodeProxyIPVerifyRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	key, bareIP, err := s.resolveProxyIPVerifyTarget(input)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	call, leader, beginErr := s.beginProxyIPVerify(key)
	if beginErr != nil {
		w.Header().Set("Retry-After", "2")
		writeErrCode(w, http.StatusTooManyRequests, "proxyip_verify_busy", beginErr)
		return
	}
	if !leader {
		select {
		case <-call.done:
			writeProxyIPVerifyOutcome(w, call.result, call.err)
		case <-r.Context().Done():
			writeErr(w, http.StatusRequestTimeout, fmt.Errorf("ProxyIP 验证请求已取消"))
		}
		return
	}

	result, verifyErr := s.runProxyIPVerify(r.Context(), bareIP)
	s.finishProxyIPVerify(key, call, result, verifyErr)
	writeProxyIPVerifyOutcome(w, result, verifyErr)
}

func decodeProxyIPVerifyRequest(r *http.Request) (proxyIPVerifyRequest, error) {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxProxyIPVerifyInputBytes+1))
	if err != nil {
		return proxyIPVerifyRequest{}, fmt.Errorf("读取 ProxyIP 验证请求: %w", err)
	}
	if len(body) > maxProxyIPVerifyInputBytes {
		return proxyIPVerifyRequest{}, fmt.Errorf("ProxyIP 验证请求超过 %d 字节", maxProxyIPVerifyInputBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var input proxyIPVerifyRequest
	if err := decoder.Decode(&input); err != nil {
		return proxyIPVerifyRequest{}, fmt.Errorf("解析 ProxyIP 验证请求: %w", err)
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("包含多个 JSON 值")
		}
		return proxyIPVerifyRequest{}, fmt.Errorf("解析 ProxyIP 验证请求: %w", err)
	}
	return input, nil
}

func (s *StatusServer) resolveProxyIPVerifyTarget(input proxyIPVerifyRequest) (key, bareIP string, err error) {
	keyInput := strings.TrimSpace(input.Key)
	addressInput := strings.TrimSpace(input.Address)
	if keyInput == "" && addressInput == "" {
		return "", "", fmt.Errorf("必须提供 key 或 address")
	}

	var keyAddr string
	if keyInput != "" {
		parsed, parseErr := url.Parse(keyInput)
		if parseErr != nil || parsed.Scheme != "proxyip" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", "", fmt.Errorf("key 必须是目录中的 proxyip://IP:443")
		}
		keyAddr = parsed.Host
	}
	if addressInput != "" && strings.Contains(addressInput, "://") {
		return "", "", fmt.Errorf("address 必须是目录中的 IP:443；带协议时请使用 key")
	}

	chosenAddr := addressInput
	if chosenAddr == "" {
		chosenAddr = keyAddr
	}
	canonicalAddr, ip, canonicalErr := canonicalProxyIPVerifyAddress(chosenAddr)
	if canonicalErr != nil {
		return "", "", canonicalErr
	}
	if keyAddr != "" {
		canonicalKeyAddr, _, keyErr := canonicalProxyIPVerifyAddress(keyAddr)
		if keyErr != nil {
			return "", "", keyErr
		}
		if canonicalKeyAddr != canonicalAddr {
			return "", "", fmt.Errorf("key 与 address 指向不同的 ProxyIP")
		}
	}

	canonicalKey := "proxyip://" + canonicalAddr
	if !s.proxyIPCandidateExists(canonicalAddr) {
		return "", "", fmt.Errorf("ProxyIP 不在当前候选目录中: %s", canonicalKey)
	}
	return canonicalKey, ip, nil
}

func canonicalProxyIPVerifyAddress(raw string) (canonicalAddr, bareIP string, err error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(raw))
	if err != nil {
		return "", "", fmt.Errorf("ProxyIP 地址必须包含端口 443")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber != 443 {
		return "", "", fmt.Errorf("ProxyIP 验证只允许端口 443")
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	if ip == nil {
		return "", "", fmt.Errorf("ProxyIP 验证只允许目录中的 IP 地址")
	}
	bareIP = ip.String()
	return net.JoinHostPort(bareIP, "443"), bareIP, nil
}

func (s *StatusServer) proxyIPCandidateExists(addr string) bool {
	snapshot := s.pool.candidates.snapshot.Load()
	if snapshot == nil {
		return false
	}
	snapshot.mu.RLock()
	defer snapshot.mu.RUnlock()
	return snapshot.find("proxyip", addr) >= 0
}

func (s *StatusServer) beginProxyIPVerify(key string) (*proxyIPVerifyCall, bool, error) {
	s.proxyIPVerifyMu.Lock()
	defer s.proxyIPVerifyMu.Unlock()
	if running := s.proxyIPVerifyRunning[key]; running != nil {
		return running, false, nil
	}
	select {
	case s.proxyIPVerifySlots <- struct{}{}:
	default:
		return nil, false, fmt.Errorf("ProxyIP 验证任务已满,请稍后重试")
	}
	call := &proxyIPVerifyCall{done: make(chan struct{})}
	s.proxyIPVerifyRunning[key] = call
	return call, true, nil
}

func (s *StatusServer) finishProxyIPVerify(key string, call *proxyIPVerifyCall, result proxyIPVerifyResult, err error) {
	s.proxyIPVerifyMu.Lock()
	call.result = result
	call.err = err
	if s.proxyIPVerifyRunning[key] == call {
		delete(s.proxyIPVerifyRunning, key)
		<-s.proxyIPVerifySlots
	}
	close(call.done)
	s.proxyIPVerifyMu.Unlock()
}

func (s *StatusServer) runProxyIPVerify(parent context.Context, bareIP string) (proxyIPVerifyResult, error) {
	ctx, cancel := context.WithTimeout(parent, proxyIPVerifyTimeout)
	defer cancel()

	endpoint, err := url.Parse(proxyIPVerifyEndpoint)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return proxyIPVerifyResult{}, fmt.Errorf("ProxyIP 验证服务地址无效")
	}
	query := endpoint.Query()
	query.Set("proxyip", bareIP)
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return proxyIPVerifyResult{}, fmt.Errorf("创建 ProxyIP 验证请求: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	response, err := proxyIPVerifyHTTPClient.Do(req)
	if err != nil {
		return proxyIPVerifyResult{}, fmt.Errorf("ProxyIP 验证服务请求失败: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return proxyIPVerifyResult{}, fmt.Errorf("ProxyIP 验证服务返回 HTTP %d", response.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return proxyIPVerifyResult{}, fmt.Errorf("ProxyIP 验证服务未返回 JSON")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxProxyIPVerifyBodyBytes+1))
	if err != nil {
		return proxyIPVerifyResult{}, fmt.Errorf("读取 ProxyIP 验证响应: %w", err)
	}
	if len(body) > maxProxyIPVerifyBodyBytes {
		return proxyIPVerifyResult{}, fmt.Errorf("ProxyIP 验证响应超过 %d 字节", maxProxyIPVerifyBodyBytes)
	}
	var payload proxyIPProbeResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return proxyIPVerifyResult{}, fmt.Errorf("ProxyIP 验证服务返回无效 JSON: %w", err)
	}
	if payload.Success == nil || payload.SupportsIPv4 == nil || payload.SupportsIPv6 == nil {
		return proxyIPVerifyResult{}, fmt.Errorf("ProxyIP 验证响应缺少必要字段")
	}
	responseTime, err := parseProxyIPResponseTime(payload.ResponseTime)
	if err != nil {
		return proxyIPVerifyResult{}, err
	}
	return proxyIPVerifyResult{
		Success: *payload.Success, ResponseTimeMs: responseTime,
		SupportsIPv4: *payload.SupportsIPv4, SupportsIPv6: *payload.SupportsIPv6,
		Source: proxyIPVerifySource, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}, nil
}

func parseProxyIPResponseTime(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, fmt.Errorf("ProxyIP 验证响应缺少 responseTime")
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		return 0, fmt.Errorf("ProxyIP 验证响应的 responseTime 无效")
	}
	value, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("ProxyIP 验证响应的 responseTime 无效")
	}
	return value, nil
}

func writeProxyIPVerifyOutcome(w http.ResponseWriter, result proxyIPVerifyResult, err error) {
	if err == nil {
		writeJSON(w, result)
		return
	}
	status := http.StatusBadGateway
	if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	} else if errors.Is(err, context.Canceled) {
		status = http.StatusRequestTimeout
	}
	writeErr(w, status, err)
}
