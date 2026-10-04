//go:build windows

package util

import (
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"golang.org/x/sys/windows/registry"
)

// internetSettingsKey is where Windows keeps the signed-in user's proxy: Settings > Network & internet > Proxy and the old Internet Options dialog both write ProxyEnable, ProxyServer and ProxyOverride here, and Edge and Chrome read them from here.
const internetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// noteScript makes sure a proxy script June cannot follow is logged once, not on every request.
var noteScript sync.Once

// osProxy is the proxy Windows' internet settings name for req, read afresh on every request so a proxy switched on or off is followed without a restart (a few registry reads, against requests that each move megabytes).
// Only the manual proxy is read. A proxy auto-config script (AutoConfigURL) or automatic detection (WPAD) would need the script fetched and run, which is out of scope; with one of those and no manual proxy, June connects directly, and HTTPS_PROXY in June's env file is the way round it. Neither is WinHTTP's machine-wide proxy (netsh winhttp) read: browsers do not follow it either. A socks= entry is skipped (see proxyFor), and a proxy that answers 407 wanting a Windows sign-in (NTLM or Kerberos) is still used but refuses June, since Go's transport can send only a user name and password written into the proxy's URL. A browser gets through on all of these, so on such a network June's downloads fail where the browser's work. Output: the proxy's URL, or nil to connect directly.
func osProxy(req *http.Request) (*url.URL, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, internetSettingsKey, registry.QUERY_VALUE)
	if err != nil {
		return nil, nil
	}
	defer k.Close()
	if script, _, _ := k.GetStringValue("AutoConfigURL"); script != "" {
		noteScript.Do(func() {
			slog.Info("Windows is set to find its proxy with a script, which June does not run; if downloads fail, set HTTPS_PROXY in June's env file", "script", script)
		})
	}
	if on, _, err := k.GetIntegerValue("ProxyEnable"); err != nil || on == 0 {
		return nil, nil
	}
	server, _, _ := k.GetStringValue("ProxyServer")
	override, _, _ := k.GetStringValue("ProxyOverride")
	if bypassed(req.URL, override) {
		return nil, nil
	}
	return proxyFor(req.URL.Scheme, server), nil
}

// proxyFor picks the proxy for a scheme out of a ProxyServer value, which is either one "host:port" for every scheme or a list such as "http=host:port;https=host:port". A list that names no proxy for the scheme sends it direct, as Windows does; a "socks=" entry is skipped, since Windows speaks SOCKS4 to it and Go only SOCKS5. Output: the proxy's URL, or nil.
func proxyFor(scheme, server string) *url.URL {
	var all string
	for _, entry := range strings.FieldsFunc(server, func(r rune) bool { return r == ';' || r == ' ' }) {
		key, val, ok := strings.Cut(entry, "=")
		if !ok {
			all = entry
			continue
		}
		if strings.EqualFold(strings.TrimSpace(key), scheme) {
			return proxyURL(val)
		}
	}
	if all == "" {
		return nil
	}
	return proxyURL(all)
}

// proxyURL reads one proxy address. Windows writes it without a scheme and talks plain HTTP to it, asking it to CONNECT for an https request, which is what Go does for an http:// proxy. Output: the URL, or nil when it cannot be read.
func proxyURL(v string) *url.URL {
	v = strings.TrimSpace(v)
	if !strings.Contains(v, "://") {
		v = "http://" + v
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return nil
	}
	return u
}

// bypassed reports whether a request goes direct under a ProxyOverride value: a list split by semicolons of host names, each with "*" for any run of characters and optionally a ":port", and "<local>" for every name without a dot in it. Loopback always goes direct, as Go's own proxy handling has it.
func bypassed(u *url.URL, override string) bool {
	host := strings.ToLower(u.Hostname())
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	for _, pat := range strings.FieldsFunc(strings.ToLower(override), func(r rune) bool { return r == ';' || r == ' ' }) {
		if pat == "<local>" {
			if !strings.Contains(host, ".") {
				return true
			}
			continue
		}
		// A pattern may carry a scheme, which Windows ignores for the match.
		if _, rest, ok := strings.Cut(pat, "://"); ok {
			pat = rest
		}
		target := host
		if _, _, err := net.SplitHostPort(pat); err == nil {
			target = host + ":" + port
		}
		if wildcard(pat, target) {
			return true
		}
	}
	return false
}

// wildcard matches s against a pattern in which "*" stands for any run of characters, and nothing else is special.
func wildcard(pat, s string) bool {
	parts := strings.Split(pat, "*")
	if len(parts) == 1 {
		return pat == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(s, mid)
		if i < 0 {
			return false
		}
		s = s[i+len(mid):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}
