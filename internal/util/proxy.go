package util

import (
	"net/http"
	"net/url"
	"os"
)

// SystemProxy is the proxy an outgoing request to the internet should use, for an http.Transport's Proxy field: the HTTPS_PROXY/HTTP_PROXY variables where set, and on Windows otherwise the manual proxy set in Windows' own internet settings, which a browser also follows and Go's default transport does not. Every client June builds for downloads, the update check and the Gemini key check should use it. It covers a proxy typed into Settings or handed out as a fixed setting, not every network where a browser works: a proxy script (PAC) or automatic detection (WPAD), a socks= entry and a proxy that asks for a Windows sign-in (NTLM or Kerberos) are not followed, as osProxy sets out. With a script or automatic detection, HTTPS_PROXY in June's env file naming the proxy it picks is the way round; a proxy that takes only a Windows sign-in has none. Output: the proxy's URL, or nil to connect directly.
func SystemProxy(req *http.Request) (*url.URL, error) {
	// The variables win outright, NO_PROXY included, because whoever set them chose them for this program; Windows' settings are only the fallback a person who never heard of them already has.
	if envProxySet() {
		return http.ProxyFromEnvironment(req)
	}
	return osProxy(req)
}

// envProxySet reports whether any of the variables http.ProxyFromEnvironment reads a proxy from is set.
func envProxySet() bool {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}
