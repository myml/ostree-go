package gpg

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"fmt"
	"hash"
	"math/big"
	"os"
	"time"
)

// OpenPGP packet tags used by this implementation.
const (
	tagSignature  = 2
	tagPublicKey  = 6
	tagUserID     = 13
	tagPublicSub  = 14
	sigTypeKeyRev = 0x20
	sigTypeDirect = 0x1f
)

// openPGPPacket is one raw OpenPGP packet.
type openPGPPacket struct {
	tag  int
	body []byte
}

// parsePackets splits OpenPGP data into packets.
func parsePackets(data []byte) ([]openPGPPacket, error) {
	var out []openPGPPacket
	for off := 0; off < len(data); {
		hdr := data[off]
		if hdr&0x80 == 0 {
			return nil, fmt.Errorf("invalid OpenPGP packet header")
		}
		var tag, length, hdrLen int
		if hdr&0x40 != 0 {
			// New format.
			tag = int(hdr & 0x3f)
			off++
			lenByte, err := packetLength(data, &off)
			if err != nil {
				return nil, err
			}
			length = lenByte
			hdrLen = off
		} else {
			// Old format.
			tag = int((hdr >> 2) & 0x0f)
			switch hdr & 0x03 {
			case 0:
				if off+1 >= len(data) {
					return nil, fmt.Errorf("truncated OpenPGP packet")
				}
				length = int(data[off+1])
				off += 2
			case 1:
				if off+2 >= len(data) {
					return nil, fmt.Errorf("truncated OpenPGP packet")
				}
				length = int(binary.BigEndian.Uint16(data[off+1:]))
				off += 3
			case 2:
				if off+4 >= len(data) {
					return nil, fmt.Errorf("truncated OpenPGP packet")
				}
				length = int(binary.BigEndian.Uint32(data[off+1:]))
				off += 5
			default:
				return nil, fmt.Errorf("indeterminate OpenPGP packet length")
			}
			hdrLen = off
		}
		_ = hdrLen
		if length < 0 || off+length > len(data) {
			return nil, fmt.Errorf("truncated OpenPGP packet body")
		}
		out = append(out, openPGPPacket{tag: tag, body: data[off : off+length]})
		off += length
	}
	return out, nil
}

// packetLength reads a new-format length and advances off past it.
func packetLength(data []byte, off *int) (int, error) {
	if *off >= len(data) {
		return 0, fmt.Errorf("truncated OpenPGP packet length")
	}
	b := data[*off]
	switch {
	case b < 192:
		*off++
		return int(b), nil
	case b < 224:
		if *off+1 >= len(data) {
			return 0, fmt.Errorf("truncated OpenPGP packet length")
		}
		n := (int(b)-192)<<8 + int(data[*off+1]) + 192
		*off += 2
		return n, nil
	case b == 255:
		if *off+4 >= len(data) {
			return 0, fmt.Errorf("truncated OpenPGP packet length")
		}
		n := int(binary.BigEndian.Uint32(data[*off+1:]))
		*off += 5
		return n, nil
	default:
		// Partial body length: not used by keyrings or detached signatures.
		return 0, fmt.Errorf("partial OpenPGP packet lengths are not supported")
	}
}

// ParsedKey is a public key or subkey with its MPIs and metadata.
type ParsedKey struct {
	Version     int
	Created     uint32
	Algo        uint8
	N           *big.Int
	E           *big.Int
	Fingerprint string
	KeyID       string
	Expires     uint32
	Revoked     bool
	IsSubkey    bool
}

// ParsedSignature is one decoded signature packet.
type ParsedSignature struct {
	Version      int
	SigType      uint8
	PubAlgo      uint8
	HashAlgo     uint8
	HashedHeader []byte
	IssuerKeyID  string
	IssuerFpr    string
	Left16       uint16
	SigInts      []*big.Int
	Created      uint32
}

// parsePublicKeyPacket decodes a v4 public key packet (RSA only).
func parsePublicKeyPacket(body []byte) (*ParsedKey, error) {
	if len(body) < 6 {
		return nil, fmt.Errorf("truncated public key packet")
	}
	key := &ParsedKey{Version: int(body[0])}
	switch key.Version {
	case 4:
		key.Created = binary.BigEndian.Uint32(body[1:5])
		key.Algo = body[5]
		off := 6
		if key.Algo != 1 && key.Algo != 2 && key.Algo != 3 {
			// Not RSA; still compute the fingerprint so the key can be
			// identified, but leave the MPIs empty.
			key.Fingerprint = fingerprintV4(body)
			key.KeyID = key.Fingerprint[len(key.Fingerprint)-16:]
			return key, nil
		}
		n, rest, err := readMPI(body[off:])
		if err != nil {
			return nil, err
		}
		e, _, err := readMPI(rest)
		if err != nil {
			return nil, err
		}
		key.N = n
		key.E = e
	case 3:
		if len(body) < 8 {
			return nil, fmt.Errorf("truncated v3 public key packet")
		}
		key.Created = binary.BigEndian.Uint32(body[1:5])
		key.Algo = body[7]
		// v3 keys are obsolete; fingerprint with MD5 is intentionally not
		// implemented.
		key.Fingerprint = ""
		key.KeyID = ""
		return key, nil
	default:
		key.Fingerprint = fingerprintV4(body)
		key.KeyID = key.Fingerprint[len(key.Fingerprint)-16:]
		return key, nil
	}
	key.Fingerprint = fingerprintV4(body)
	key.KeyID = key.Fingerprint[len(key.Fingerprint)-16:]
	return key, nil
}

// fingerprintV4 computes the v4 fingerprint: SHA1(0x99 || len || body).
func fingerprintV4(body []byte) string {
	h := sha1.New()
	h.Write([]byte{0x99, byte(len(body) >> 8), byte(len(body))})
	h.Write(body)
	return hexUpper(h.Sum(nil))
}

func hexUpper(data []byte) string {
	const hexdigits = "0123456789ABCDEF"
	out := make([]byte, len(data)*2)
	for i, b := range data {
		out[i*2] = hexdigits[b>>4]
		out[i*2+1] = hexdigits[b&0x0f]
	}
	return string(out)
}

// readMPI decodes an OpenPGP multiprecision integer.
func readMPI(data []byte) (*big.Int, []byte, error) {
	if len(data) < 2 {
		return nil, nil, fmt.Errorf("truncated MPI")
	}
	bits := int(binary.BigEndian.Uint16(data))
	nbytes := (bits + 7) / 8
	if len(data) < 2+nbytes {
		return nil, nil, fmt.Errorf("truncated MPI")
	}
	value := new(big.Int).SetBytes(data[2 : 2+nbytes])
	return value, data[2+nbytes:], nil
}

// parseSignaturePacket decodes a v4 signature packet.
func parseSignaturePacket(body []byte) (*ParsedSignature, error) {
	if len(body) < 1 {
		return nil, fmt.Errorf("truncated signature packet")
	}
	sig := &ParsedSignature{Version: int(body[0])}
	switch sig.Version {
	case 4:
		if len(body) < 6 {
			return nil, fmt.Errorf("truncated signature packet")
		}
		sig.SigType = body[1]
		sig.PubAlgo = body[2]
		sig.HashAlgo = body[3]
		hashedLen := int(binary.BigEndian.Uint16(body[4:6]))
		if len(body) < 6+hashedLen+2 {
			return nil, fmt.Errorf("truncated signature packet")
		}
		sig.HashedHeader = body[:6+hashedLen]
		hashed := body[6 : 6+hashedLen]
		if err := sig.parseSubpackets(hashed); err != nil {
			return nil, err
		}
		off := 6 + hashedLen
		unhashedLen := int(binary.BigEndian.Uint16(body[off : off+2]))
		off += 2
		if len(body) < off+unhashedLen+2 {
			return nil, fmt.Errorf("truncated signature packet")
		}
		if err := sig.parseSubpackets(body[off : off+unhashedLen]); err != nil {
			return nil, err
		}
		off += unhashedLen
		sig.Left16 = binary.BigEndian.Uint16(body[off : off+2])
		off += 2
		rest := body[off:]
		for len(rest) > 0 {
			mpi, next, err := readMPI(rest)
			if err != nil {
				return nil, err
			}
			sig.SigInts = append(sig.SigInts, mpi)
			rest = next
		}
	case 3:
		if len(body) < 19 {
			return nil, fmt.Errorf("truncated v3 signature packet")
		}
		if body[1] != 5 {
			return nil, fmt.Errorf("invalid v3 signature packet")
		}
		sig.SigType = body[2]
		sig.Created = binary.BigEndian.Uint32(body[3:7])
		sig.IssuerKeyID = hexUpper(body[7:15])
		sig.PubAlgo = body[15]
		sig.HashAlgo = body[16]
		sig.Left16 = binary.BigEndian.Uint16(body[17:19])
		rest := body[19:]
		for len(rest) > 0 {
			mpi, next, err := readMPI(rest)
			if err != nil {
				return nil, err
			}
			sig.SigInts = append(sig.SigInts, mpi)
			rest = next
		}
	default:
		return nil, fmt.Errorf("unsupported signature version %d", sig.Version)
	}
	return sig, nil
}

func (sig *ParsedSignature) parseSubpackets(data []byte) error {
	for off := 0; off < len(data); {
		length, consumed, err := subpacketLength(data[off:])
		if err != nil {
			return err
		}
		off += consumed
		if length < 1 || off+length > len(data) {
			return fmt.Errorf("truncated signature subpacket")
		}
		subType := data[off] & 0x7f
		subBody := data[off+1 : off+length]
		off += length
		switch subType {
		case 2: // signature creation time
			if len(subBody) == 4 {
				sig.Created = binary.BigEndian.Uint32(subBody)
			}
		case 16: // issuer key ID
			if len(subBody) == 8 {
				sig.IssuerKeyID = hexUpper(subBody)
			}
		case 33: // issuer fingerprint
			if len(subBody) >= 21 && subBody[0] == 4 {
				sig.IssuerFpr = hexUpper(subBody[1:21])
			}
		}
	}
	return nil
}

func subpacketLength(data []byte) (length, consumed int, err error) {
	if len(data) < 1 {
		return 0, 0, fmt.Errorf("truncated signature subpacket")
	}
	b := int(data[0])
	switch {
	case b < 192:
		return b, 1, nil
	case b < 255:
		if len(data) < 2 {
			return 0, 0, fmt.Errorf("truncated signature subpacket")
		}
		return (b-192)<<8 + int(data[1]) + 192, 2, nil
	default:
		if len(data) < 5 {
			return 0, 0, fmt.Errorf("truncated signature subpacket")
		}
		return int(binary.BigEndian.Uint32(data[1:])), 5, nil
	}
}

// hashForAlgo returns a hash.Hash for an OpenPGP hash algorithm ID.
func hashForAlgo(algo uint8) (hash.Hash, crypto.Hash, bool) {
	switch algo {
	case 2:
		return sha1.New(), crypto.SHA1, true
	case 8:
		return sha256.New(), crypto.SHA256, true
	case 9:
		return sha512.New384(), crypto.SHA384, true
	case 10:
		return sha512.New(), crypto.SHA512, true
	case 11:
		h := sha256.New()
		return &truncatedHash{Hash: h, size: 28}, crypto.SHA224, true
	}
	return nil, 0, false
}

// truncatedHash implements SHA-224 on top of SHA-256.
type truncatedHash struct {
	hash.Hash
	size int
}

func (t *truncatedHash) Size() int { return t.size }
func (t *truncatedHash) Sum(b []byte) []byte {
	full := t.Hash.Sum(nil)
	return append(b, full[:t.size]...)
}

// computeDigest computes the OpenPGP signature digest over data.
func (sig *ParsedSignature) computeDigest(data []byte) ([]byte, crypto.Hash, error) {
	h, hashType, ok := hashForAlgo(sig.HashAlgo)
	if !ok {
		return nil, 0, fmt.Errorf("unsupported hash algorithm %d", sig.HashAlgo)
	}
	h.Write(data)
	if sig.Version == 4 {
		h.Write(sig.HashedHeader)
		trailer := []byte{0x04, 0xff, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(trailer[2:], uint32(len(sig.HashedHeader)))
		h.Write(trailer)
	} else {
		h.Write([]byte{sig.SigType})
		var ts [4]byte
		binary.BigEndian.PutUint32(ts[:], sig.Created)
		h.Write(ts[:])
	}
	return h.Sum(nil), hashType, nil
}

// verifyRSA reports whether sig validates data with key.
func (sig *ParsedSignature) verifyRSA(data []byte, key *ParsedKey) (bool, error) {
	if key.N == nil || key.E == nil {
		return false, nil
	}
	if len(sig.SigInts) != 1 {
		return false, fmt.Errorf("invalid RSA signature packet")
	}
	digest, hashType, err := sig.computeDigest(data)
	if err != nil {
		return false, err
	}
	if len(digest) >= 2 {
		prefix := binary.BigEndian.Uint16(digest[:2])
		if prefix != sig.Left16 {
			return false, nil
		}
	}
	pub := &rsa.PublicKey{N: key.N, E: int(key.E.Int64())}
	size := (pub.N.BitLen() + 7) / 8
	sigBytes := sig.SigInts[0].Bytes()
	if len(sigBytes) < size {
		padded := make([]byte, size)
		copy(padded[size-len(sigBytes):], sigBytes)
		sigBytes = padded
	}
	if err := rsa.VerifyPKCS1v15(pub, hashType, digest, sigBytes); err != nil {
		return false, nil
	}
	return true, nil
}

// keyExpired reports whether key was expired at time t.
func keyExpired(key *ParsedKey, t time.Time) bool {
	return key.Expires != 0 && t.Unix() >= int64(key.Expires)
}

// verifyPure is the pure Go fallback for signature verification.
func verifyPure(data, sigs []byte, keyringFiles []string) (*VerifyResult, error) {
	packets, err := parsePackets(sigs)
	if err != nil {
		return nil, fmt.Errorf("no signatures found")
	}
	var keyring []byte
	for _, file := range keyringFiles {
		blob, rerr := os.ReadFile(file)
		if rerr != nil {
			continue
		}
		keyring = append(keyring, blob...)
	}
	keys, err := parseKeyringKeys(keyring)
	if err != nil {
		return nil, err
	}
	keyByID := map[string]*ParsedKey{}
	for _, key := range keys {
		if key.KeyID != "" {
			keyByID[key.KeyID] = key
		}
	}
	res := &VerifyResult{}
	haveGood := false
	var haveMissing, haveBad, haveExpired, haveRevoked bool
	now := time.Now()
	for _, packet := range packets {
		if packet.tag != tagSignature {
			continue
		}
		sig, perr := parseSignaturePacket(packet.body)
		if perr != nil {
			haveBad = true
			continue
		}
		res.Signatures++
		key := keyByID[sig.IssuerKeyID]
		if key == nil && sig.IssuerFpr != "" {
			for _, candidate := range keys {
				if candidate.Fingerprint == sig.IssuerFpr {
					key = candidate
					break
				}
			}
		}
		if key == nil {
			haveMissing = true
			continue
		}
		res.KeyIDs = append(res.KeyIDs, key.KeyID)
		if key.Revoked {
			haveRevoked = true
			continue
		}
		if keyExpired(key, now) {
			haveExpired = true
			continue
		}
		ok, verr := sig.verifyRSA(data, key)
		if verr != nil {
			continue
		}
		if ok {
			haveGood = true
			res.Valid++
		} else {
			haveBad = true
		}
	}
	if haveGood {
		return res, nil
	}
	switch {
	case haveRevoked:
		return nil, fmt.Errorf("Key revoked")
	case haveExpired:
		return nil, fmt.Errorf("Key expired")
	case haveMissing:
		return nil, fmt.Errorf("Can't check signature: public key not found")
	case haveBad:
		return nil, fmt.Errorf("BAD signature")
	case res.Signatures == 0:
		return nil, fmt.Errorf("no signatures found")
	}
	return nil, fmt.Errorf("Can't check signature: public key not found")
}

// parseKeyringKeys parses all public keys (primary and subkeys) in a binary
// keyring, including expiry and revocation state.
func parseKeyringKeys(data []byte) ([]*ParsedKey, error) {
	packets, err := parsePackets(data)
	if err != nil {
		return nil, err
	}
	var keys []*ParsedKey
	var primary *ParsedKey
	var current *ParsedKey
	var sigContext *ParsedSignature
	for _, packet := range packets {
		switch packet.tag {
		case tagPublicKey:
			key, err := parsePublicKeyPacket(packet.body)
			if err != nil {
				continue
			}
			primary = key
			current = key
			keys = append(keys, key)
			sigContext = nil
		case tagPublicSub:
			if primary == nil {
				continue
			}
			key, err := parsePublicKeyPacket(packet.body)
			if err != nil {
				continue
			}
			key.IsSubkey = true
			keys = append(keys, key)
			current = key
			sigContext = nil
		case tagUserID:
			if primary == nil {
				continue
			}
			current = primary
			sigContext = nil
		case tagSignature:
			sig, err := parseSignaturePacket(packet.body)
			if err != nil {
				sigContext = nil
				continue
			}
			sigContext = sig
			issuer := sig.IssuerKeyID
			if issuer == "" && sig.IssuerFpr != "" && len(sig.IssuerFpr) >= 16 {
				issuer = sig.IssuerFpr[len(sig.IssuerFpr)-16:]
			}
			if primary == nil {
				continue
			}
			_ = sigContext
			if sig.SigType == sigTypeKeyRev && primary.KeyID != "" && issuer == primary.KeyID {
				primary.Revoked = true
			}
			if (sig.SigType == sigTypeDirect || (sig.SigType >= 0x10 && sig.SigType <= 0x13)) &&
				primary.KeyID != "" && issuer == primary.KeyID && current == primary {
				if expires := keyExpirationSubpacket(sig); expires != 0 {
					primary.Expires = primary.Created + expires
				}
			}
			if sig.SigType == 0x18 && current != nil && current.IsSubkey && issuer == primary.KeyID {
				if expires := keyExpirationSubpacket(sig); expires != 0 {
					current.Expires = current.Created + expires
				}
			}
		}
	}
	return keys, nil
}

// keyExpirationSubpacket returns the key expiration time (seconds) from a
// signature's hashed subpackets, if present.
func keyExpirationSubpacket(sig *ParsedSignature) uint32 {
	if sig.Version != 4 {
		return 0
	}
	// The hashed header starts with 6 bytes of fixed fields followed by the
	// subpacket area, whose length is stored at offset 4.
	hashedLen := int(binary.BigEndian.Uint16(sig.HashedHeader[4:6]))
	data := sig.HashedHeader[6 : 6+hashedLen]
	for off := 0; off < len(data); {
		length, consumed, err := subpacketLength(data[off:])
		if err != nil {
			break
		}
		off += consumed
		if length < 1 || off+length > len(data) {
			break
		}
		subType := data[off] & 0x7f
		subBody := data[off+1 : off+length]
		off += length
		if subType == 9 && len(subBody) == 4 {
			return binary.BigEndian.Uint32(subBody)
		}
	}
	return 0
}

// SignatureIssuerKeyIDs returns the issuer key IDs of every signature packet
// in sigs.
func SignatureIssuerKeyIDs(sigs []byte) ([]string, error) {
	packets, err := parsePackets(sigs)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, packet := range packets {
		if packet.tag != tagSignature {
			continue
		}
		sig, err := parseSignaturePacket(packet.body)
		if err != nil {
			continue
		}
		id := sig.IssuerKeyID
		if id == "" && sig.IssuerFpr != "" && len(sig.IssuerFpr) >= 16 {
			id = sig.IssuerFpr[len(sig.IssuerFpr)-16:]
		}
		out = append(out, id)
	}
	return out, nil
}

// KeyIDMatches reports whether a signature issuer matches keyID, accepting
// short (8 hex) and long (16 hex) key IDs and a trailing '!' subkey marker.
func KeyIDMatches(issuer, keyID string) bool {
	issuer = stringsToUpper(issuer)
	id := stringsToUpper(trimBang(keyID))
	if id == "" {
		return false
	}
	return issuer == id || (len(issuer) > len(id) && issuer[len(issuer)-len(id):] == id)
}

func stringsToUpper(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

func trimBang(s string) string {
	if len(s) > 0 && s[len(s)-1] == '!' {
		return s[:len(s)-1]
	}
	return s
}
