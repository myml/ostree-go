package gpg

import (
	"crypto/sha1"
	"strings"
)

// zbase32Alphabet is the z-base-32 alphabet from RFC 6189.
const zbase32Alphabet = "ybndrfg8ejkmcpqxot1uwisza345h769"

// zbase32Encode encodes data using the z-base-32 encoding.
func zbase32Encode(data []byte) string {
	var b strings.Builder
	bits := 0
	buffer := 0
	for _, c := range data {
		buffer = (buffer << 8) | int(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			b.WriteByte(zbase32Alphabet[(buffer>>bits)&0x1f])
		}
	}
	if bits > 0 {
		b.WriteByte(zbase32Alphabet[(buffer<<(5-bits))&0x1f])
	}
	return b.String()
}

// encodeWKDLocal hashes the lowercased local part of an email address with
// SHA-1 and encodes it with z-base-32.
func encodeWKDLocal(local string) string {
	digest := sha1.Sum([]byte(local))
	return zbase32Encode(digest[:])
}

// URLPathEscape escapes a string the way g_uri_escape_string() does for a
// path component.  The characters used in email local parts are all
// unreserved except for '+', which GLib keeps unescaped in path mode.
func urlPathEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == '-', c == '.', c == '_', c == '~', c == '+':
			b.WriteByte(c)
		default:
			const hex = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}

// WKDURLs returns the advanced and direct Web Key Directory URLs for an
// email address, matching ot_gpg_wkd_urls().
func WKDURLs(email string) (advanced, direct string) {
	parts := strings.Split(email, "@")
	if len(parts) != 2 {
		return "", ""
	}
	localLower := strings.ToLower(parts[0])
	domainLower := strings.ToLower(parts[1])
	localEncoded := encodeWKDLocal(localLower)
	localEscaped := urlPathEscape(parts[0])
	advanced = "https://openpgpkey." + parts[1] +
		"/.well-known/openpgpkey" +
		"/" + domainLower + "/hu/" + localEncoded + "?l=" + localEscaped
	direct = "https://" + parts[1] +
		"/.well-known/openpgpkey" +
		"/hu/" + localEncoded + "?l=" + localEscaped
	return advanced, direct
}
