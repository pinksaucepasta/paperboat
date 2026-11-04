package reporting

import (
	"reflect"
	"strings"
	"syscall"
	"unicode"
)

// The error-to-label primitives below are the one implementation every
// binary's fault projection (shared, cli, server, relay, tunnel) uses.

// StatusCause maps an HTTP status carried by an error to a bounded cause.
func StatusCause(status int) string {
	switch status {
	case 401, 403:
		return "permission_denied"
	case 404:
		return "not_found"
	case 409:
		return "conflict"
	case 429:
		return "rate_limited"
	case 503:
		return "service_unavailable"
	}
	if status >= 400 && status < 500 {
		return "invalid_request"
	}
	if status >= 500 && status <= 599 {
		return "upstream_failure"
	}
	return "internal"
}

// ErrnoCause maps a native errno to a bounded cause.
func ErrnoCause(value syscall.Errno) string {
	switch value {
	case syscall.ECONNREFUSED:
		return "connection_refused"
	case syscall.EHOSTUNREACH, syscall.ENETUNREACH:
		return "network_unreachable"
	case syscall.ECONNRESET:
		return "connection_reset"
	case syscall.ETIMEDOUT:
		return "network_timeout"
	case syscall.EPIPE:
		return "broken_pipe"
	case syscall.EADDRINUSE:
		return "address_in_use"
	case syscall.EINVAL:
		return "invalid_argument"
	case syscall.ENOENT:
		return "not_found"
	case syscall.EACCES, syscall.EPERM:
		return "permission_denied"
	case syscall.EEXIST:
		return "conflict"
	case syscall.ENOSPC, syscall.EMFILE, syscall.ENFILE, syscall.ENOMEM:
		return "resource_exhausted"
	}
	return "system_error"
}

// TypeName is the bounded Go type name of an error (pointers dereferenced), or "error".
func TypeName(typ reflect.Type) string { return safeErrorType(typ) }

// ValidErrorType reports whether value is a bounded type-name token.
func ValidErrorType(value string) bool { return validErrorType(value) }

// SafeSourceFunction reduces a function name to a bounded, content-free token.
func SafeSourceFunction(value string) string { return safeSourceFunction(value) }

// RequiredFaultFields is the field set every consumer's Fault must carry.
var RequiredFaultFields = []string{
	"component", "operation", "stage", "code", "cause", "error_type", "outcome", "severity",
	"support_reference", "error_chain", "errno", "http_status", "source_file", "source_function", "source_line",
}

// MissingFaultFields lists RequiredFaultFields absent from the struct type of fault
// (by json tag, else snake_case of the Go field name).
func MissingFaultFields(fault any) []string {
	typ := reflect.TypeOf(fault)
	have := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" {
			name = snake(field.Name)
		}
		have[name] = true
	}
	var missing []string
	for _, name := range RequiredFaultFields {
		if !have[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

func snake(name string) string {
	runes := []rune(name)
	var out []rune
	for i, r := range runes {
		if unicode.IsUpper(r) && i > 0 && (!unicode.IsUpper(runes[i-1]) || i+1 < len(runes) && unicode.IsLower(runes[i+1])) {
			out = append(out, '_')
		}
		out = append(out, unicode.ToLower(r))
	}
	return string(out)
}
