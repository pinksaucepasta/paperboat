package main

import (
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"syscall"

	edgeruntime "github.com/pinksaucepasta/paperboat/edge/internal/runtime"
	"github.com/pinksaucepasta/paperboat/shared/reporting"
)

// maxDiagnosticNodes bounds the error-tree walk, as reporting.Project does.
const maxDiagnosticNodes = 16

// maxDetailRunes bounds the leaf text a start failure may print.
const maxDetailRunes = 200

// secretWords make a leaf text unprintable: a start failure's text goes to the journal, which must
// hold no credentials, tokens or key material (AGENTS.md, Security and privacy).
var secretWords = []string{"secret", "token", "password", "passwd", "bearer", "credential", "authorization", "private key", "begin "}

// serviceDiagnostic is the operator-readable summary of a service failure. Besides the content-free
// projection it names the failing components of a start failure, and the text of a leaf error whose
// type is known to hold only addresses, paths and syscall errors (oracle, 2026-11-05: three failed
// handovers showed only "StartError, StartError, errorString").
func serviceDiagnostic(err error) string {
	cause, errorType, httpStatus := reporting.SafeFailureDetails(err)
	detail := fmt.Sprintf("cause=%s error_type=%s", cause, errorType)
	if httpStatus != 0 {
		detail += fmt.Sprintf(" http_status=%d", httpStatus)
	}
	var components []string
	var text string
	walkDiagnostic(err, func(node error) bool {
		if start, ok := node.(*edgeruntime.StartError); ok {
			if name := safeComponentName(start.Component); name != "" {
				components = append(components, name)
			}
		}
		if text == "" {
			text = safeLeafText(node)
		}
		return true
	})
	if len(components) > 0 {
		detail += " components=" + strings.Join(components, ">")
	}
	if text != "" {
		detail += " detail=" + strconv.Quote(text)
	}
	return detail
}

// walkDiagnostic visits the error tree depth first, at most maxDiagnosticNodes nodes.
func walkDiagnostic(err error, visit func(error) bool) {
	count := 0
	var walk func(error)
	walk = func(node error) {
		if node == nil || count >= maxDiagnosticNodes {
			return
		}
		count++
		visit(node)
		// Only a start failure's own chain is walked (StartError and errors.Join); any other wrapper
		// (a control request failure, say) is a leaf, so what it wraps never reaches the line.
		switch wrapped := node.(type) {
		case *edgeruntime.StartError:
			walk(wrapped.Unwrap())
		case interface{ Unwrap() []error }:
			if reflect.TypeOf(node).String() == "*errors.joinError" {
				for _, child := range wrapped.Unwrap() {
					walk(child)
				}
			}
		}
	}
	walk(err)
}

func safeComponentName(name string) string {
	if name == "" || len(name) > 64 || strings.Trim(name, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_.*") != "" {
		return ""
	}
	return name
}

// safeLeafText is the Error text of a node whose type only carries code-authored or syscall text:
// network operations (address, errno) and plain errors.New/fmt.Errorf values. File paths stay out.
func safeLeafText(node error) string {
	switch node.(type) {
	case *net.OpError, syscall.Errno:
	default:
		if reflect.TypeOf(node).String() != "*errors.errorString" {
			return ""
		}
	}
	text := strings.TrimSpace(node.Error())
	lower := strings.ToLower(text)
	for _, word := range secretWords {
		if strings.Contains(lower, word) {
			return ""
		}
	}
	if runes := []rune(text); len(runes) > maxDetailRunes {
		text = string(runes[:maxDetailRunes])
	}
	return text
}
