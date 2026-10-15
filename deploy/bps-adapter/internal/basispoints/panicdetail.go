package basispoints

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
)

// PanicDetail turns a recovered value into a one-line, log-safe description
// that names the plugin frame the panic came from.
//
// The exported entry points recover so a panic cannot take the whole gateway
// down with it, but "plugin callback panicked" on its own says nothing: the
// 2026-09-28 empty-input panic had to be reproduced offline against captured
// request bodies before anyone could see which line it was. The frame is
// carried in the error message, which the host logs, so the next one is
// diagnosable from the log alone.
//
// Only the innermost frame inside this module is reported, as file:line plus
// the function's own name. No arguments, request content, or credential
// material can reach this string.
func PanicDetail(recovered any) string {
	const modulePath = "cpa-plugin-oai-basispoints"
	detail := fmt.Sprintf("%v", recovered)
	var pcs [32]uintptr
	// Skip runtime.Callers, PanicDetail, and the deferred closure that calls it.
	n := runtime.Callers(3, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		// Match on the function name, not the file path: the name is always
		// fully qualified with the module path, while the path depends on
		// where the build happened and on -trimpath.
		if strings.Contains(frame.Function, modulePath) && !strings.HasSuffix(frame.File, "panicdetail.go") {
			name := frame.Function
			if index := strings.LastIndex(name, "."); index >= 0 {
				name = name[index+1:]
			}
			slash := strings.LastIndexAny(frame.File, `/\`)
			return fmt.Sprintf("%s at %s:%d (%s)", detail, frame.File[slash+1:], frame.Line, name)
		}
		if !more {
			break
		}
	}
	return detail
}

// streamCloseReason labels a stream failure so the one string the host
// receives says what kind of failure it was.
//
// host.stream.close takes no status code, so without this a client
// disconnect, an upstream timeout and a genuinely broken upstream are
// indistinguishable in the gateway's logs and accounting. The structured
// payload the audit suggested was not adopted: CPA's stream-close contract has
// not been read, and a host that fails to decode the close message leaks both
// the upstream stream and the host-side one, which is worse than losing the
// label.
func streamCloseReason(err error) string {
	var coded interface{ StatusCode() int }
	if !errors.As(err, &coded) {
		return "[upstream_error] "
	}
	switch status := coded.StatusCode(); {
	case status == 499:
		return "[client_disconnected] "
	case status == 504:
		return "[upstream_timeout] "
	case status == 500:
		return "[plugin_bug] "
	case status == 422:
		// A tool call the upstream model wrote that could not be converted.
		// "[request_error]" read as the client's fault, which it is not.
		return "[relay_tool_error] "
	case status >= 400 && status < 500:
		return "[request_error] "
	default:
		return "[upstream_error] "
	}
}
