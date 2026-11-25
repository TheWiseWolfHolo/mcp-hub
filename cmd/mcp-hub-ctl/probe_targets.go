package main

import (
	"context"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/TheWiseWolfHolo/mcp-hub/internal/hub"
)

type probeEndpoint struct {
	url       string
	tokenFile string
	kind      string
}

func probeURL(raw string) (*url.URL, string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" || u.Opaque != "" ||
		(u.Scheme != "http" && u.Scheme != "https") || !hub.IsLoopbackHost(u.Hostname()) {
		return nil, "", false
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	if port == "0" {
		return nil, "", false
	}
	return u, net.JoinHostPort(u.Hostname(), port), true
}

// resolveLoopbackAddrs expands a validated loopback address into concrete numeric loopback
// dial candidates. For "localhost:<port>", both IPv4 (127.0.0.1) and IPv6 ([::1]) loopback
// addresses are returned so Windows IPv6-first resolution still reaches IPv4-bound listeners.
func resolveLoopbackAddrs(raw string) ([]string, error) {
	addr, err := normalizeControlAddr(raw)
	if err != nil {
		return nil, err
	}
	host, port, _ := net.SplitHostPort(addr)
	if !strings.EqualFold(host, "localhost") {
		return []string{addr}, nil
	}

	ips, lookupErr := net.LookupIP(host)
	seen := map[string]bool{}
	var addrs []string
	addIP := func(ip net.IP) {
		if ip == nil || !ip.IsLoopback() {
			return
		}
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		cand := net.JoinHostPort(ip.String(), port)
		if !seen[cand] {
			seen[cand] = true
			addrs = append(addrs, cand)
		}
	}
	if lookupErr == nil {
		for _, ip := range ips {
			addIP(ip)
		}
		if len(addrs) == 0 && len(ips) > 0 {
			return nil, &cliError{exit: exitUnreachable, code: "hub_unreachable", message: "localhost did not resolve to a loopback address"}
		}
	}
	// Guarantee dual-stack fallback between 127.0.0.1 and ::1.
	addIP(net.ParseIP("127.0.0.1"))
	addIP(net.ParseIP("::1"))
	return addrs, nil
}

// loopbackDialAddr resolves raw into a concrete numeric loopback host:port.
// For localhost, it races resolved IPv4 and IPv6 loopback candidates concurrently so whichever
// stack the hub bound answers immediately without suffering Windows IPv6-first connection errors.
func loopbackDialAddr(raw string) (string, error) {
	candidates, err := resolveLoopbackAddrs(raw)
	if err != nil {
		return "", err
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	type dialHit struct {
		addr string
		ok   bool
	}
	ch := make(chan dialHit, len(candidates))
	for _, cand := range candidates {
		go func(target string) {
			d := net.Dialer{Timeout: 350 * time.Millisecond}
			conn, dialErr := d.DialContext(ctx, "tcp", target)
			if dialErr == nil {
				_ = conn.Close()
				ch <- dialHit{addr: target, ok: true}
				return
			}
			ch <- dialHit{addr: target, ok: false}
		}(cand)
	}
	for range candidates {
		hit := <-ch
		if hit.ok {
			cancel()
			return hit.addr, nil
		}
	}
	// Neither loopback stack is listening right now; prefer IPv4 so downstream errors are predictable.
	for _, cand := range candidates {
		if strings.HasPrefix(cand, "127.") {
			return cand, nil
		}
	}
	return candidates[0], nil
}

// probeListenAddr verifies that an advertised listener matches the locally configured address
// (including localhost matching either 127.0.0.1 or ::1 on the same port) and pins the dial
// target to that verified loopback address.
func probeListenAddr(configured, advertised string) (string, bool) {
	candidates, err := resolveLoopbackAddrs(configured)
	if err != nil || len(candidates) == 0 {
		return "", false
	}
	for _, cand := range candidates {
		if sameBind(cand, advertised) {
			return cand, true
		}
	}
	if sameBind(configured, advertised) {
		addr, err := loopbackDialAddr(configured)
		if err != nil {
			return "", false
		}
		return addr, true
	}
	return "", false
}

func (p probeEndpoint) match(advertised string) (probeEndpoint, bool) {
	expected, expectedAddr, expectedOK := probeURL(p.url)
	actual, actualAddr, actualOK := probeURL(advertised)
	if !expectedOK || !actualOK || expected.Scheme != actual.Scheme ||
		expected.EscapedPath() != actual.EscapedPath() || expected.RawQuery != actual.RawQuery ||
		expected.ForceQuery != actual.ForceQuery {
		return probeEndpoint{}, false
	}
	addr, ok := probeListenAddr(expectedAddr, actualAddr)
	if !ok {
		return probeEndpoint{}, false
	}
	expected.Host = addr
	p.url = expected.String()
	return p, true
}

func configuredProbeEndpoints(cfg *hub.Config, name string) []probeEndpoint {
	if cfg == nil {
		return nil
	}
	var svc *hub.ServiceConfig
	for i := range cfg.Services {
		if cfg.Services[i].Name == name {
			svc = &cfg.Services[i]
			break
		}
	}
	var endpoints []probeEndpoint
	if g := cfg.Gateway; g != nil {
		for _, route := range g.Routes {
			if route.Service != name && !(route.Service == "" && route.Slug == name) {
				continue
			}
			path := "/s/" + route.Slug
			switch {
			case svc != nil && svc.EffectiveMode() == "stdio-bridge":
			case svc != nil && strings.TrimSpace(route.ClientPath) == "":
				path += svc.EffectiveClientPath()
			default:
				path += route.EffectiveClientPath()
			}
			endpoints = append(endpoints, probeEndpoint{
				url:       "http://" + g.ListenAddr + path,
				tokenFile: firstTokenFile(route.TokenFile, route.Tokens),
				kind:      "gateway",
			})
		}
	}
	for _, proxy := range cfg.Proxies {
		if proxy.UpstreamService != name && !(proxy.UpstreamService == "" && proxy.Name == name) {
			continue
		}
		path := proxy.EffectiveClientPath()
		if svc != nil && strings.TrimSpace(proxy.ClientPath) == "" {
			path = svc.EffectiveClientPath()
		}
		endpoints = append(endpoints, probeEndpoint{
			url:       "http://" + proxy.ListenAddr + path,
			tokenFile: firstTokenFile(proxy.TokenFile, proxy.Tokens),
			kind:      "proxy:" + proxy.Name,
		})
	}
	if svc != nil {
		if base := hub.InferServiceBaseURL(*svc); base != "" {
			endpoints = append(endpoints, probeEndpoint{
				url:       strings.TrimRight(base, "/") + svc.EffectiveClientPath(),
				tokenFile: hub.InferServiceTokenFile(*svc),
				kind:      "direct",
			})
		}
	}
	return endpoints
}

func matchProbeEndpoint(endpoints []probeEndpoint, advertised string) (probeEndpoint, bool) {
	for _, p := range endpoints {
		if endpoint, ok := p.match(advertised); ok {
			return endpoint, true
		}
	}
	return probeEndpoint{}, false
}
