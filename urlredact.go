package main

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

const redactedURLValue = "REDACTED"

// safeSourceURL returns a URL suitable for management responses and logs. The
// fetch path must continue using the original Source.URL; this helper is only
// for presentation boundaries.
func safeSourceURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "[invalid source URL]"
	}

	// A username can itself be a secret (for example an API key used as Basic
	// auth), so replace the complete userinfo rather than only the password.
	if u.User != nil {
		u.User = url.User(redactedURLValue)
	}

	if u.RawQuery != "" {
		query, parseErr := url.ParseQuery(u.RawQuery)
		if parseErr != nil {
			// Malformed queries are rejected by many servers inconsistently. Do not
			// risk echoing a secret from a component we could not parse safely.
			u.RawQuery = redactedURLValue
		} else {
			// Presentation and log boundaries cannot reliably know whether a custom
			// provider calls its credential "token", "code", "subscription", or
			// something else. Preserve parameter names for diagnosis, but redact all
			// values instead of maintaining an inevitably incomplete secret-key list.
			for key, values := range query {
				for i := range values {
					values[i] = redactedURLValue
				}
				query[key] = values
			}
			u.RawQuery = query.Encode()
		}
	}
	if u.Fragment != "" {
		u.Fragment = redactedURLValue
	}
	return u.String()
}

// safeLogLabel makes legacy persisted labels harmless in line-oriented logs.
// New labels reject control characters at the ConfigStore boundary, but old
// configuration files may predate that validation.
func safeLogLabel(value string) string {
	value = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value))
	runes := []rune(value)
	if len(runes) > 128 {
		value = string(runes[:128]) + "..."
	}
	if value == "" {
		return "[unnamed source]"
	}
	return value
}

// isSensitiveURLQueryKey deliberately treats only unambiguous long markers or
// exact short credential names as secrets. This covers api_key, access-token,
// clientSecret and X-Amz-Signature without hiding unrelated keys such as
// "monkey" or "compass" merely because they contain "key"/"pass".
func isSensitiveURLQueryKey(key string) bool {
	isExactCredentialWord := func(value string) bool {
		switch value {
		case "key", "pass", "pwd", "auth", "sig", "user", "username", "jwt", "bearer":
			return true
		default:
			return false
		}
	}
	normalized := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, key)
	if normalized == "" {
		return false
	}
	if isExactCredentialWord(normalized) {
		return true
	}
	// Preserve separators long enough to recognize keys such as proxy_pass,
	// subscription-key, or basic.auth without treating "compass"/"monkey" as
	// credentials just because their spelling happens to end similarly.
	for _, part := range strings.FieldsFunc(strings.ToLower(key), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if isExactCredentialWord(part) {
			return true
		}
	}
	for _, marker := range []string{
		"token", "secret", "password", "passwd", "signature",
		"authorization", "credential", "apikey", "accesskey",
		"privatekey", "authkey", "signingkey", "sessionkey",
		"passphrase", "passcode", "proxypass",
	} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func safeManagementSource(source Source) Source {
	source.URL = safeSourceURL(source.URL)
	return source
}

func safeManagementSources(sources []Source) []Source {
	out := make([]Source, len(sources))
	for i, source := range sources {
		out[i] = safeManagementSource(source)
	}
	return out
}

// sourceURLSafeError preserves errors.Is/errors.As through Unwrap while its
// printable message contains only a redacted source URL.
type sourceURLSafeError struct {
	operation string
	sourceURL string
	detail    string
	cause     error
}

func (e *sourceURLSafeError) Error() string {
	if e.detail == "" {
		return fmt.Sprintf("%s %s failed", e.operation, e.sourceURL)
	}
	return fmt.Sprintf("%s %s failed: %s", e.operation, e.sourceURL, e.detail)
}

func (e *sourceURLSafeError) Unwrap() error { return e.cause }

func safeSourceURLError(operation, rawURL string, err error) error {
	if err == nil {
		return nil
	}
	detailErr := err
	// http.Client.Do normally returns *url.Error, whose Error string embeds the
	// full request URL. Retain the underlying network diagnosis without that URL.
	for {
		urlErr, ok := detailErr.(*url.Error)
		if !ok || urlErr.Err == nil {
			break
		}
		detailErr = urlErr.Err
	}
	detail := redactSourceURLInText(detailErr.Error(), rawURL)
	return &sourceURLSafeError{
		operation: operation,
		sourceURL: safeSourceURL(rawURL),
		detail:    detail,
		cause:     err,
	}
}

func redactSourceURLInText(text, rawURL string) string {
	safe := safeSourceURL(rawURL)
	candidates := []string{rawURL}
	if parsed, err := url.Parse(rawURL); err == nil {
		candidates = append(candidates, parsed.String(), parsed.Redacted())
	}
	for _, candidate := range candidates {
		if candidate != "" {
			text = strings.ReplaceAll(text, candidate, safe)
		}
	}
	return text
}
