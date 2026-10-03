package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// redactedError keeps the original error reachable through Unwrap while
// exposing a message with URL secrets removed.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// RedactedURLPlaceholder stands in for a URL that could not be parsed. Raw
// text cannot be redacted reliably in that case: net/url hands back a string
// truncated at the first '#' (so a credential can look like the host or a
// port), and there is no way to tell a port from a truncated password. The
// only safe option is to not echo the input at all.
const RedactedURLPlaceholder = "<redacted-url>"

// RedactURLString returns a display form of raw with userinfo, query, and
// fragment removed. A string that does not parse as a URL is replaced entirely
// by RedactedURLPlaceholder, because no part of it can be trusted.
func RedactURLString(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return RedactedURLPlaceholder
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

// RedactURLCredentials removes userinfo, query, and fragment values from any
// *url.Error in the chain. A downstream credential may be configured in the
// URL query (docs/CONFIGURATION.md documents that form), and Go's url.Error
// embeds the whole URL in its message while redacting only the userinfo
// password - so without this the proxy's own downstream secret would reach
// callers through a JSON-RPC error and the daemon log. errors.As still finds
// the wrapped transport.Error, so failure classification is unaffected.
func RedactURLCredentials(err error) error {
	if err == nil {
		return nil
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	if urlErr.Err == nil {
		return err
	}
	safe := RedactURLString(urlErr.URL)
	if safe == urlErr.URL {
		// Nothing to redact (no userinfo, query, or fragment).
		return err
	}
	if safe == RedactedURLPlaceholder {
		// The URL did not parse. Its raw text can also have contaminated the
		// reason - net/url reports a truncated credential as a port - so drop
		// the reason as well rather than half-redact the message.
		return &redactedError{msg: fmt.Sprintf("%s %s", urlErr.Op, RedactedURLPlaceholder), err: err}
	}
	// Replace the RENDERED form of the url.Error, not the raw URL: url.Error
	// renders its URL with %q, so a credential containing a quote or backslash
	// does not appear verbatim in the message and a raw-URL replacement would
	// silently leave it in place.
	rawRendered := (&url.Error{Op: urlErr.Op, URL: urlErr.URL, Err: urlErr.Err}).Error()
	redactedRendered := (&url.Error{Op: urlErr.Op, URL: safe, Err: urlErr.Err}).Error()
	message := strings.ReplaceAll(err.Error(), rawRendered, redactedRendered)
	if message == err.Error() {
		// The wrapper did not render the url.Error verbatim; fall back to the
		// raw URL, which covers the remaining shapes.
		message = strings.ReplaceAll(err.Error(), urlErr.URL, safe)
	}
	return &redactedError{msg: message, err: err}
}

func ParseRedirectURI(redirectURI string) (path string, addr string, err error) {
	// Echo a redacted form: this error is printed and logged, and the URI could
	// carry a credential in its userinfo or query. A URL that does not parse is
	// replaced entirely, since its raw text cannot be redacted reliably.
	display := RedactURLString(redirectURI)
	u, err := url.Parse(redirectURI)
	if err != nil {
		return "", "", fmt.Errorf("invalid redirect URI %q", display)
	}
	if u.Scheme != "http" {
		return "", "", fmt.Errorf("redirect URI %q must use http", display)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("redirect URI %q cannot contain user info, query, or fragment", display)
	}
	hostname := u.Hostname()
	if hostname == "" {
		return "", "", fmt.Errorf("redirect URI %q must include a host", display)
	}
	ip := net.ParseIP(hostname)
	if !strings.EqualFold(hostname, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return "", "", fmt.Errorf("redirect URI %q must use localhost or a loopback IP", display)
	}
	port := u.Port()
	if port == "" {
		return "", "", fmt.Errorf("redirect URI %q must include an explicit port", display)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", "", fmt.Errorf("redirect URI %q contains an invalid port", display)
	}
	if u.Path == "" || u.Path == "/" {
		return "", "", fmt.Errorf("redirect URI %q must include a callback path", display)
	}
	return u.Path, net.JoinHostPort(hostname, port), nil
}
