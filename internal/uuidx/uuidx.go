// Package uuidx derives deterministic UUIDs from stable keys.
//
// It exists because several simulated vendors identify the same real-world
// thing by different names -- B4B knows a merchant by its beneficiary id,
// the settlement stand-in knows it by its IBAN, Banking Circle knows it by
// an account id -- and the lab needs those to agree without any of them
// importing another vendor's package or inventing a random id that a
// second process could not reproduce.
//
// RFC 4122 version 5 is exactly this: a name-based UUID, SHA-1 over a
// namespace and a name, stable forever. Nothing here is secret or
// security-bearing -- SHA-1's weaknesses do not apply to generating an
// identifier -- and using the real algorithm rather than an invented one
// means a value produced here can be reproduced by any UUIDv5
// implementation, which is what makes it checkable from outside the lab.
package uuidx

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"
)

// NamespaceLab is this lab's own namespace UUID, used for every derived
// identifier in it.
//
// It is itself an obviously-synthetic value rather than a random one: a
// reader who finds a lab UUID in a database and wonders where it came from
// can re-derive it, and one who sees this constant can tell at a glance
// that it is not a real Banking Circle namespace someone copied in.
const NamespaceLab = "5f1b0000-0000-4000-8000-5f1b1abc0de0"

// Derive returns the RFC 4122 v5 UUID for name within namespace.
//
// The namespace must be a UUID; anything else is a programming error and
// panics, because a mistyped namespace would silently produce a whole
// parallel set of identifiers that look correct and match nothing.
func Derive(namespace, name string) string {
	ns, err := bytesOf(namespace)
	if err != nil {
		panic("uuidx: namespace " + namespace + ": " + err.Error())
	}
	h := sha1.New()
	h.Write(ns)
	h.Write([]byte(name))
	sum := h.Sum(nil)

	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return format(u)
}

// Valid reports whether s is a canonical lower-case UUID: eight-four-four-
// four-twelve hex digits.
//
// Deliberately strict. Real vendors reject a malformed identifier rather
// than coercing it, and a simulator that accepted "bc_acc_sga_eur" where
// the real API wants a UUID would let a client ship a configuration that
// only works against the simulator -- which is the one failure this whole
// lab exists to prevent.
func Valid(s string) bool {
	_, err := bytesOf(s)
	return err == nil
}

func bytesOf(s string) ([]byte, error) {
	if len(s) != 36 {
		return nil, fmt.Errorf("not a UUID: want 36 characters, got %d", len(s))
	}
	if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return nil, fmt.Errorf("not a UUID: hyphens must be at 8, 13, 18 and 23")
	}
	// Upper case is legal in RFC 4122 but is not what any of these APIs
	// emit, and accepting it here would mean two spellings of one id.
	if strings.ToLower(s) != s {
		return nil, fmt.Errorf("not a UUID: must be lower case")
	}
	b, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil {
		return nil, fmt.Errorf("not a UUID: %w", err)
	}
	return b, nil
}

func format(u [16]byte) string {
	h := hex.EncodeToString(u[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
