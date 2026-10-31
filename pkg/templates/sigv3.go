package templates

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// OpenPGP's version 3 signature packet (RFC 4880, section 5.2.2), which the Open Build Service
// still signs openSUSE's checksum files with and go-crypto no longer reads.
const (
	signatureTag     = 2
	signatureV3      = 3
	v3HashedLength   = 5
	sigTypeBinary    = 0x00
	pubKeyAlgoRSA    = 1
	v3HeaderLength   = 19 // version to the hash's first two bytes, before the signature's MPI
	maxV3PacketBytes = 4096
)

// v3Hashes are the hashes a version 3 signature may use: none of the broken ones.
var v3Hashes = map[byte]crypto.Hash{8: crypto.SHA256, 9: crypto.SHA384, 10: crypto.SHA512}

// v3Signature is a parsed version 3 signature over binary data.
type v3Signature struct {
	hashed  [v3HashedLength]byte // the signature type and creation time, hashed after the data
	created time.Time
	keyID   uint64
	hash    crypto.Hash
	left16  [2]byte
	rsa     []byte
}

// parseV3 reads a detached signature, armoured or binary, as a version 3 one. It is false for
// anything else, which go-crypto checks.
func parseV3(sig []byte) (*v3Signature, bool, error) {
	body := sig
	if bytes.HasPrefix(bytes.TrimSpace(sig), []byte("-----BEGIN")) {
		block, err := armor.Decode(bytes.NewReader(sig))
		if err != nil {
			return nil, false, fmt.Errorf("reading the armoured signature: %w", err)
		}
		if body, err = io.ReadAll(io.LimitReader(block.Body, maxV3PacketBytes)); err != nil {
			return nil, false, fmt.Errorf("reading the armoured signature: %w", err)
		}
	}
	p, err := signaturePacket(body)
	if err != nil || len(p) == 0 || p[0] != signatureV3 {
		// Anything but one version 3 signature is go-crypto's to read, and refuse.
		return nil, false, nil //nolint:nilerr // go-crypto reports it
	}
	if len(p) < v3HeaderLength+2 || p[1] != v3HashedLength {
		return nil, true, errors.New("malformed version 3 signature")
	}
	s := &v3Signature{keyID: binary.BigEndian.Uint64(p[7:15])}
	copy(s.hashed[:], p[2:7])
	s.created = time.Unix(int64(binary.BigEndian.Uint32(p[3:7])), 0)
	if s.hashed[0] != sigTypeBinary {
		return nil, true, fmt.Errorf("a version 3 signature of type %#x, not over binary data", s.hashed[0])
	}
	if p[15] != pubKeyAlgoRSA {
		return nil, true, fmt.Errorf("a version 3 signature with public key algorithm %d, not RSA", p[15])
	}
	hash, ok := v3Hashes[p[16]]
	if !ok {
		return nil, true, fmt.Errorf("a version 3 signature with hash algorithm %d, not SHA-2", p[16])
	}
	s.hash = hash
	copy(s.left16[:], p[17:19])
	bits := int(binary.BigEndian.Uint16(p[19:21]))
	mpi := p[21:]
	if len(mpi) != (bits+octetBits-1)/octetBits {
		return nil, true, errors.New("malformed version 3 signature value")
	}
	s.rsa = mpi
	return s, true, nil
}

// OpenPGP's packet headers (RFC 4880, section 4.2): a tag octet, then the length of the body in
// one, two or four octets. The old format keeps the length's size in the tag octet. The new one
// tells it by the length's first octet, which below newTwoOctets is the whole length, below
// newPartial the first of two, and at newFourOctets is followed by four.
const (
	packetBit        = 0x80
	newFormatBit     = 0x40
	newTagMask       = 0x3f
	oldTagShift      = 2
	oldTagMask       = 0x0f
	oldLengthMask    = 0x03
	oldIndeterminate = 3
	newTwoOctets     = 192
	newPartial       = 224
	newFourOctets    = 255
	octetBits        = 8
)

// The sizes of a packet's header: the tag octet with one, two or four octets of length, or with
// the new format's marker octet and four.
const (
	headerOneOctet   = 2
	headerTwoOctets  = 3
	headerFourOctets = 5
	headerNewFour    = 6
)

var errTruncatedPacket = errors.New("truncated OpenPGP packet")

// signaturePacket is the body of the one signature packet a detached signature holds, in OpenPGP's
// old or new packet format.
func signaturePacket(b []byte) ([]byte, error) {
	if len(b) < headerOneOctet || b[0]&packetBit == 0 {
		return nil, errors.New("not an OpenPGP packet")
	}
	read := newFormatHeader
	if b[0]&newFormatBit == 0 {
		read = oldFormatHeader
	}
	h, err := read(b)
	if err != nil {
		return nil, err
	}
	if h.tag != signatureTag {
		return nil, fmt.Errorf("OpenPGP packet of tag %d, not a signature", h.tag)
	}
	if len(b) != h.size+h.length {
		return nil, errors.New("a detached signature must be exactly one signature packet")
	}
	return b[h.size:], nil
}

// packetHeader is what a packet's header says: its tag and the body's length, and its own size.
type packetHeader struct {
	tag, length, size int
}

func oldFormatHeader(b []byte) (packetHeader, error) {
	lengthType := b[0] & oldLengthMask
	if lengthType == oldIndeterminate {
		return packetHeader{}, errors.New("an OpenPGP packet of indeterminate length")
	}
	n := [...]int{headerOneOctet, headerTwoOctets, headerFourOctets}[lengthType]
	if len(b) < n {
		return packetHeader{}, errTruncatedPacket
	}
	return packetHeader{tag: int(b[0]>>oldTagShift) & oldTagMask, length: bigEndian(b[1:n]), size: n}, nil
}

func newFormatHeader(b []byte) (packetHeader, error) {
	tag := int(b[0] & newTagMask)
	switch first := b[1]; {
	case first < newTwoOctets:
		return packetHeader{tag: tag, length: int(first), size: headerOneOctet}, nil
	case first < newPartial:
		if len(b) < headerTwoOctets {
			return packetHeader{}, errTruncatedPacket
		}
		return packetHeader{tag: tag, length: bigEndian([]byte{first - newTwoOctets, b[2]}) + newTwoOctets, size: headerTwoOctets}, nil
	case first == newFourOctets:
		if len(b) < headerNewFour {
			return packetHeader{}, errTruncatedPacket
		}
		return packetHeader{tag: tag, length: bigEndian(b[2:headerNewFour]), size: headerNewFour}, nil
	}
	return packetHeader{}, errors.New("a partial-length OpenPGP packet")
}

// bigEndian reads at most four octets as an unsigned big-endian number.
func bigEndian(b []byte) int {
	var v int
	for _, o := range b {
		v = v<<octetBits | int(o)
	}
	return v
}

// verify checks the signature over data with the primary key of e, which must have been valid when
// the signature was made and not have been revoked since.
func (s *v3Signature) verify(e *openpgp.Entity, data []byte, now time.Time) error {
	if e.PrimaryKey.KeyId != s.keyID {
		return fmt.Errorf("signed by key %016X, not %016X", s.keyID, e.PrimaryKey.KeyId)
	}
	pub, ok := e.PrimaryKey.PublicKey.(*rsa.PublicKey)
	if !ok || e.PrimaryKey.PubKeyAlgo != packet.PubKeyAlgoRSA {
		return errors.New("the key is no RSA key")
	}
	if e.Revoked(now) {
		return errors.New("the key is revoked")
	}
	identity := e.PrimaryIdentity()
	if identity == nil || identity.SelfSignature == nil {
		return errors.New("the key has no self-signature")
	}
	if s.created.Before(e.PrimaryKey.CreationTime) || e.PrimaryKey.KeyExpired(identity.SelfSignature, s.created) || s.created.After(now.Add(time.Hour)) {
		return fmt.Errorf("signed at %s, outside the key's lifetime", s.created.UTC().Format(time.RFC3339))
	}
	h := s.hash.New()
	h.Write(data)
	h.Write(s.hashed[:])
	digest := h.Sum(nil)
	if !bytes.Equal(digest[:2], s.left16[:]) {
		return errors.New("the signature is not over this data")
	}
	value := make([]byte, pub.Size())
	if len(s.rsa) > len(value) {
		return errors.New("the signature value is longer than the key")
	}
	copy(value[len(value)-len(s.rsa):], s.rsa)
	if err := rsa.VerifyPKCS1v15(pub, s.hash, digest, value); err != nil {
		return fmt.Errorf("the signature does not check out: %w", err)
	}
	return nil
}
