//go:build !bpsexp

package basispoints

import "net/http"

// Experiment hooks, production build (no build tag).
//
// Every function here is the empty implementation the ordinary plugin ships.
// The experiment-only behaviour (per-request arm selection, capture, shadow
// rewrite, deterministic-repair shadow, the multi-agent plaintext switch and
// the anti-misuse self-check) lives in the files guarded by //go:build bpsexp;
// in this build the compiler inlines each of these to nothing, so the plugin
// behaves exactly as A1 does (EXP-BUILD.md). The call sites in the untagged
// code (upstream.go, service.go, stream.go, protocol.go, managementpage.go,
// identity.go, relay_options.go) reference these names and compile in both
// builds.
//
// expRetryTap is a value type with no fields so expBeginRetry and its methods
// are zero cost: the retry loop in withUpstreamRetry's callers gains nothing
// but a named local that the optimiser removes.

type expRetryTap struct{}

func expBeginRetry(ExecutorRequest, string) expRetryTap { return expRetryTap{} }

func (expRetryTap) attempt(error) {}
func (expRetryTap) done(error)    {}

func expConfigure(*Service, []byte, Config) {}

func expApplyArm(*Service, *ExecutorRequest, map[string]any, *Config) {}

func expArmMode(_ ExecutorRequest, mode identityMode) identityMode { return mode }

func expRewriteOutboundBody(ExecutorRequest, map[string]any) {}

func expRecordRequestBody(ExecutorRequest, []byte, http.Header, bool) {}

func expRecordNonStreamSSE(ExecutorRequest, []byte, error) {}

func expRecordStreamChunk(string, []byte) {}

func expRecordStreamEnd(ExecutorRequest, map[string]any, error) {}

func expWithTrial(source map[string]any, _ ExecutorRequest) map[string]any { return source }

func expObserveRepairShadow(map[string]any, map[string]any, string) {}

func expEnqueueShadowRewrite(*Service, ExecutorRequest, map[string]any, credential, error) {}

func expManagementRoute(*Service, string, string, map[string][]string) (map[string]any, bool) {
	return nil, false
}

func expStatus() map[string]any { return nil }
