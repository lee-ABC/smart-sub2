//go:build go1.27 && !http2legacy

package adapter

import "errors"

// ValidateTransportRuntime fails at startup rather than running CPA's connection
// retirement assumptions against Go 1.27's different HTTP/2 wrapper.
func ValidateTransportRuntime() error {
	return errors.New("BPS adapter requires -tags=http2legacy on Go 1.27+; use its Dockerfile or make build")
}
