//go:build metric_legacy_fixture

package metric

// Only reference-fixture CI may construct historical mutating stores. Never
// supply this tag to the panel, release or real end-to-end builds.
func retireLegacyConfig(_ *Config) {}
