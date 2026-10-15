package basispoints

// H25 S2 test switches. Tests written for rc2 assert rc2 behaviour that is now
// behind a switch; they run with that switch set explicitly (s2RC2 and
// friends) and keep their assertions, next to the restored h22 assertion for
// the default configuration.

// s2RC2 sets every S2 switch to rc2's behaviour: batch delivery of relay
// calls, enforced tool policy checks, passthrough of unsuccessful terminals.
func s2RC2(cfg *Config) {
	cfg.RelayToolDelivery = relayToolDeliveryBatch
	cfg.ToolPolicyChecks = toolPolicyChecksEnforce
	cfg.RelayIncomplete = relayIncompletePassthrough
}

func s2Batch(cfg *Config)       { cfg.RelayToolDelivery = relayToolDeliveryBatch }
func s2Enforce(cfg *Config)     { cfg.ToolPolicyChecks = toolPolicyChecksEnforce }
func s2Passthrough(cfg *Config) { cfg.RelayIncomplete = relayIncompletePassthrough }

func s2Options(configure func(*Config)) relayOptions {
	cfg := defaultConfig()
	if configure != nil {
		configure(&cfg)
	}
	return relayOptionsFromConfig(cfg)
}

// s2Source freezes configure's switches onto source, as request preparation
// does, for tests that drive the relay or the decoder directly.
func s2Source(source map[string]any, configure func(*Config)) map[string]any {
	return withRelayOptions(source, s2Options(configure))
}

type s2Mode struct {
	name      string
	configure func(*Config)
	rc2       bool
}

// s2Modes: the default configuration (= h22) and rc2's switches.
func s2Modes() []s2Mode {
	return []s2Mode{{name: "default"}, {name: "rc2", configure: s2RC2, rc2: true}}
}
