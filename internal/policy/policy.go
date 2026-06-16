package policy

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type Decision struct {
	Allow           bool   `json:"allow"`
	RequireApproval bool   `json:"require_approval"`
	Reason          string `json:"reason"`
}

var riskyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bgit\s+push\b`),
	regexp.MustCompile(`(?i)\brm\s+-rf\s+/`),
	regexp.MustCompile(`(?i)\bcurl\s+https?://`),
	regexp.MustCompile(`(?i)\bwget\s+https?://`),
	regexp.MustCompile(`(?i)\bssh\s+`),
	regexp.MustCompile(`(?i)\bscp\s+`),
}

func EvaluateTool(toolName string, input json.RawMessage, allowed []string) Decision {
	// MCP tools are authorized at the agent-server assignment level (agent_mcp_servers table),
	// so they bypass the allowed_tools gate here.
	if strings.HasPrefix(toolName, "mcp__") {
		return Decision{Allow: true}
	}

	if len(allowed) > 0 {
		ok := false
		for _, t := range allowed {
			if strings.TrimSpace(t) == toolName {
				ok = true
				break
			}
		}
		if !ok {
			return Decision{Allow: false, Reason: "tool denied by agent policy"}
		}
	}

	if toolName == "shell.exec" || toolName == "test.run" {
		var req struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(input, &req)
		for _, pattern := range riskyPatterns {
			if pattern.MatchString(req.Command) {
				return Decision{Allow: true, RequireApproval: true, Reason: "risky command requires approval"}
			}
		}
	}

	if toolName == "web.fetch" || toolName == "http.request" {
		var req struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(input, &req)
		decision := evaluateURLTarget(req.URL, "local network access requires approval")
		if decision.Reason != "" {
			return decision
		}
	}

	if strings.HasPrefix(toolName, "browser.") {
		switch toolName {
		case "browser.open", "browser.action", "browser.snapshot", "browser.screenshot", "browser.wait", "browser.close":
		default:
			return Decision{Allow: false, Reason: "unsupported browser tool"}
		}
		if toolName == "browser.open" {
			var req struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(input, &req)
			decision := evaluateURLTarget(req.URL, "local browser target requires approval")
			if decision.Reason != "" {
				return decision
			}
		}
	}

	return Decision{Allow: true}
}

// trustedLocalPorts lists localhost ports that host internal services and
// should be callable by agents without an operator approval gate.
var trustedLocalPorts = map[string]bool{
	"3001": true, // Zernio social-media bridge
}

func evaluateURLTarget(rawURL, approvalReason string) Decision {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme == "" {
		return Decision{Allow: false, Reason: "invalid url"}
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return Decision{Allow: false, Reason: "blocked url scheme"}
	}
	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return Decision{Allow: false, Reason: "missing url host"}
	}
	if isMetadataHost(host) {
		return Decision{Allow: false, Reason: "blocked metadata service host"}
	}
	if isLocalOrPrivateHost(host) {
		if trustedLocalPorts[u.Port()] {
			return Decision{Allow: true}
		}
		return Decision{Allow: true, RequireApproval: true, Reason: approvalReason}
	}
	return Decision{}
}

func isMetadataHost(host string) bool {
	normalized := strings.Trim(strings.ToLower(host), "[]")
	return normalized == "169.254.169.254" || normalized == "metadata.google.internal"
}

func isLocalOrPrivateHost(host string) bool {
	normalized := strings.Trim(strings.ToLower(host), "[]")
	if normalized == "localhost" || strings.HasSuffix(normalized, ".localhost") {
		return true
	}
	if ip := parseHostIP(normalized); ip.IsValid() {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified()
	}
	addrs, err := net.LookupIP(normalized)
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		if ip, err := netip.ParseAddr(addr.String()); err == nil {
			if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
				return true
			}
		}
	}
	return false
}

func parseHostIP(host string) netip.Addr {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip
	}
	if parsed := net.ParseIP(host); parsed != nil {
		if ip, err := netip.ParseAddr(parsed.String()); err == nil {
			return ip
		}
	}
	if strings.HasPrefix(host, "0x") {
		var n uint32
		if _, err := fmt.Sscanf(host, "0x%x", &n); err == nil {
			return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
		}
	}
	if n, err := strconv.ParseUint(host, 10, 32); err == nil {
		return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
	}
	return netip.Addr{}
}
