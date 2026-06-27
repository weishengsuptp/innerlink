package group

import "crypto/rand"

// cryptoRandRead is a tiny indirection so tests in this package
// can override the RNG (via randFunc) without touching crypto/rand
// directly. Default = crypto/rand.Read, which is what production
// uses. Override only in tests that want deterministic chains.
var cryptoRandRead = rand.Read

// ResetCryptoRand restores the default RNG (for tests that called
// OverrideCryptoRand). Not concurrency-safe — only call from test
// setup / teardown.
func ResetCryptoRand() { cryptoRandRead = rand.Read }
