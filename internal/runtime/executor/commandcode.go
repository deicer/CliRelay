package executor

// Command Code uses the native /alpha/generate gateway. The implementation is
// kept in commandcode_legacy_executor.go because the gateway is not part of the
// public Provider API and still returns the legacy NDJSON event stream.
//
// Note the plan boundary: Command Code's $1 "Go" plan mints a working key but
// answers 403 upgrade_required on this base URL, because that plan excludes API
// access. Every plan above it (GOAT and up) is served normally. The refusal is
// an entitlement signal, not a credential fault, so it is surfaced to the
// operator as-is instead of being retried or masked.

const commandCodeProvider = "commandcode"
