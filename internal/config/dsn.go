package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ParseShorthand parses a connection shorthand of the form
//
//	"user:pass@host:port/db"  or  "user@host/db"  or  "host:port/db"
//
// Missing port defaults to 3306. IPv6 addresses are not supported in
// shorthand form; use the granular flags instead.
//
// The password segment is optional, and an explicitly empty one is
// meaningful: "user:@host" declares a password-less server (TiDB's default
// root) and suppresses the interactive password prompt, while "user@host"
// leaves the password to be prompted for (or taken from password_env).
func ParseShorthand(s string) (Endpoint, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Endpoint{}, errors.New("empty connection spec")
	}
	if strings.ContainsAny(s, " \t") {
		return Endpoint{}, fmt.Errorf("connection shorthand must not contain spaces: %q", s)
	}
	var ep Endpoint
	rest := s
	if i := strings.LastIndexByte(rest, '@'); i >= 0 {
		userpart := rest[:i]
		rest = rest[i+1:]
		if j := strings.IndexByte(userpart, ':'); j >= 0 {
			// A colon means the password segment is present, even when it
			// is empty ("user:@host" = password-less server).
			ep.User, ep.Password, ep.passwordSet = userpart[:j], userpart[j+1:], true
		} else {
			ep.User = userpart
		}
	}
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		ep.Database = rest[j+1:]
		rest = rest[:j]
	}
	if k := strings.LastIndexByte(rest, ':'); k >= 0 {
		ep.Host = rest[:k]
		port, err := strconv.Atoi(rest[k+1:])
		if err != nil || port <= 0 {
			return ep, fmt.Errorf("invalid port in %q", s)
		}
		ep.Port = port
	} else {
		ep.Host = rest
	}
	if ep.Host == "" {
		return ep, fmt.Errorf("missing host in %q", s)
	}
	if ep.Port == 0 {
		ep.Port = 3306
	}
	return ep, nil
}

// MaskedDSN returns a printable representation of the endpoint with the
// password always redacted. Safe for logs, error messages and JSON output.
func (e Endpoint) MaskedDSN() string {
	var b strings.Builder
	if e.User != "" {
		b.WriteString(e.User)
		if e.Password != "" || e.PasswordEnv != "" {
			b.WriteString(":***")
		}
		b.WriteString("@")
	}
	b.WriteString(e.Host)
	if e.Port != 0 {
		b.WriteString(":")
		b.WriteString(strconv.Itoa(e.Port))
	}
	if e.Database != "" {
		b.WriteString("/")
		b.WriteString(e.Database)
	}
	return b.String()
}
