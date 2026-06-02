package cli

import "net/http"

// BuildServeHandler exposes the internal serve-handler construction to external
// tests so they can exercise cmdServe's HTTP surface (both auth modes) without
// starting the long-lived serve goroutines. It builds the handler EXACTLY the
// way cmdServe does (same AuthConfig derivation from the base64 master key), so a
// test asserting on this handler is asserting on serve's real behavior.
func BuildServeHandler(masterKeyB64 string) (http.Handler, error) {
	return buildServeHandler(masterKeyB64)
}

// DeriveSessionKey exposes the HKDF session-key derivation to external tests so
// they can assert it is deterministic for a given master key.
func DeriveSessionKey(masterKeyB64 string) ([]byte, error) {
	return deriveSessionKey(masterKeyB64)
}
