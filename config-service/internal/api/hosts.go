package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// HostPolicy says where a workspace's own integrations may point
// (INTEGRATION_ALLOWED_HOSTS). Platform integrations aren't limited: only
// platform admins add those.
//
// Entries are separated by commas: a host name (mcp.payments.bank.internal),
// every name under a domain (*.bank.internal), or an IP range (10.20.0.0/16).
// Empty allows any host. Either way, these are always refused: this machine
// (localhost, 127.0.0.0/8, ::1), link-local addresses (169.254.0.0/16, where
// clouds keep instance credentials, and fe80::/10), cloud metadata names, and
// the platform's own services.
type HostPolicy struct {
	names    []string // exact, lower case
	suffixes []string // ".bank.internal" for *.bank.internal
	nets     []*net.IPNet
}

var deniedNames = map[string]bool{
	"localhost": true, "metadata": true, "metadata.google.internal": true, "instance-data": true,
	"instance-data.ec2.internal": true,
	// The platform's own services (infra/docker-compose.yml).
	"postgres": true, "platform-db": true, "config-service": true, "keycloak": true, "keycloak-db-init": true,
	"litellm": true, "studio": true, "voice-platform": true, "caddy": true, "agent": true,
}

// AWS's IPv6 metadata address.
var awsMetadataV6 = net.ParseIP("fd00:ec2::254")

// ParseHostPolicy reads INTEGRATION_ALLOWED_HOSTS.
func ParseHostPolicy(raw string) (HostPolicy, error) {
	var h HostPolicy
	for _, e := range strings.Split(raw, ",") {
		e = strings.ToLower(strings.TrimSpace(e))
		switch {
		case e == "":
		case strings.Contains(e, "/"):
			_, n, err := net.ParseCIDR(e)
			if err != nil {
				return h, fmt.Errorf("INTEGRATION_ALLOWED_HOSTS: '%s' isn't an IP range like 10.20.0.0/16", e)
			}
			h.nets = append(h.nets, n)
		case strings.HasPrefix(e, "*."):
			h.suffixes = append(h.suffixes, e[1:])
		case strings.ContainsAny(e, "*:/ "):
			return h, fmt.Errorf("INTEGRATION_ALLOWED_HOSTS: '%s' isn't a host name, *.domain or IP range", e)
		default:
			if ip := net.ParseIP(e); ip != nil {
				h.nets = append(h.nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(len(ip)*8, len(ip)*8)})
			} else {
				h.names = append(h.names, e)
			}
		}
	}
	return h, nil
}

// Limited reports whether an allow list is set.
func (h HostPolicy) Limited() bool { return len(h.names)+len(h.suffixes)+len(h.nets) > 0 }

// blockedIP: addresses no workspace integration may reach.
func blockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() ||
		ip.IsMulticast() || ip.Equal(awsMetadataV6)
}

func (h HostPolicy) allows(host string) bool {
	if !h.Limited() {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		for _, n := range h.nets {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	}
	for _, n := range h.names {
		if host == n {
			return true
		}
	}
	for _, s := range h.suffixes {
		if strings.HasSuffix(host, s) {
			return true
		}
	}
	return false
}

// check returns the problems with a URL a workspace integration would use.
func (h HostPolicy) check(field, raw string) []string {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil // checkURL has said so
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if ip := net.ParseIP(host); ip != nil {
		if blockedIP(ip) {
			return []string{field + ": can't point at this machine or a link-local address"}
		}
	} else if deniedNames[host] || strings.HasSuffix(host, ".localhost") {
		return []string{field + ": " + host + " is the platform's own address; point at your own server"}
	}
	if !h.allows(host) {
		return []string{fmt.Sprintf("%s: %s isn't one of the hosts this platform allows integrations to call; ask a platform admin to add it to INTEGRATION_ALLOWED_HOSTS", field, host)}
	}
	return nil
}

var errBlockedAddress = errors.New("refused: that address is this machine or a link-local address")

// outboundClient is the HTTP client for calls this service makes on a
// person's behalf (loading an MCP server's tools). restricted: refuse
// blocked addresses as actually dialled (so a name that resolves to
// 127.0.0.1 doesn't get through), bypass any proxy (which would hide the
// address), and never follow redirects.
//
// dial replaces the dialer (tests only: to reach a local server by name).
func outboundClient(restricted bool, timeout time.Duration, dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
	if restricted {
		dialer.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || blockedIP(ip) {
				return errBlockedAddress
			}
			return nil
		}
		transport.Proxy = nil
	}
	if dial != nil {
		transport.DialContext = dial
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
