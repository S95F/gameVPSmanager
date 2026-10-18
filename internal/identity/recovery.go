package identity

import "strings"

// Recovery codes are grouped Crockford base32 (admin-security-14): an operator
// reads one off a screen and types it back, sometimes days later, so the
// encoding is the one built for exactly that — no I/L/O/U to confuse with
// 1/0, case-insensitive, and tolerant of the dashes gvm prints for grouping.

// crockfordAlphabet is Crockford's base32, excluding I, L, O and U.
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// RecoveryCodeBits is the entropy in a recovery code. 80 bits encodes to exactly
// 16 Crockford characters (5 bits each), with nothing left to pad.
const RecoveryCodeBits = 80

// crockfordEncode encodes b as Crockford base32, five bits at a time, big-endian.
// It is written out rather than reached for from encoding/base32 because that
// package implements RFC 4648's alphabet, not Crockford's.
func crockfordEncode(b []byte) string {
	var sb strings.Builder
	var acc uint32
	var bits int
	for _, c := range b {
		acc = acc<<8 | uint32(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			sb.WriteByte(crockfordAlphabet[(acc>>uint(bits))&0x1f])
		}
	}
	if bits > 0 {
		sb.WriteByte(crockfordAlphabet[(acc<<uint(5-bits))&0x1f])
	}
	return sb.String()
}

// groupRecoveryCode inserts a dash every four characters, so a 16-character code
// reads as XXXX-XXXX-XXXX-XXXX.
func groupRecoveryCode(s string) string {
	var sb strings.Builder
	for i, r := range s {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// NormalizeRecoveryCode canonicalises an operator-typed code so a comparison
// does not depend on how they typed it. It strips dashes and whitespace,
// uppercases, and applies Crockford's decode mappings (O→0, I/L→1). It returns
// "" for anything that is not the expected number of valid characters, so a
// garbled code fails as "wrong code" rather than matching by accident.
func NormalizeRecoveryCode(s string) string {
	want := (RecoveryCodeBits + 4) / 5 // 16 for 80 bits
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r == '-' || r == ' ' || r == '\t':
			continue
		case r >= 'a' && r <= 'z':
			r -= 'a' - 'A'
		}
		switch r {
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		if !strings.ContainsRune(crockfordAlphabet, r) {
			return ""
		}
		sb.WriteRune(r)
	}
	if sb.Len() != want {
		return ""
	}
	return sb.String()
}
