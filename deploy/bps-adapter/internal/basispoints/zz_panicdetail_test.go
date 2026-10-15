package basispoints

import (
	"strings"
	"testing"
)

func TestPanicDetailNamesTheFrame(t *testing.T) {
	got := func() (out string) {
		defer func() {
			if r := recover(); r != nil {
				out = PanicDetail(r)
			}
		}()
		// Panic from inside a real plugin function so the frame walk has
		// something module-local to find.
		turnStatePanicForTest()
		return ""
	}()
	if got == "" {
		t.Fatal("no panic captured")
	}
	if !strings.Contains(got, "index out of range") {
		t.Errorf("lost the panic value: %q", got)
	}
	if !strings.Contains(got, ".go:") {
		t.Errorf("no file:line in %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("must stay one line: %q", got)
	}
	t.Logf("detail = %s", got)
}

func turnStatePanicForTest() {
	var empty []any
	_ = empty[3]
}

func TestPanicDetailNonRuntimeValue(t *testing.T) {
	got := func() (out string) {
		defer func() {
			if r := recover(); r != nil {
				out = PanicDetail(r)
			}
		}()
		panic("plain string")
	}()
	if !strings.Contains(got, "plain string") {
		t.Errorf("got %q", got)
	}
	t.Logf("detail = %s", got)
}
