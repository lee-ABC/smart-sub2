//go:build !go1.27 || http2legacy

package adapter

func ValidateTransportRuntime() error { return nil }
