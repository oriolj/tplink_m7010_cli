package main

// sha256Crypt is a minimal pure-Go implementation of crypt(3) with the
// "$5$" SHA-256 algorithm (Drepper, 2008). It exists because the GL.iNet
// challenge/response auth on the Mudi (and other OpenWrt-based GL devices)
// expects the password to be hashed exactly as `crypt(P, "$5$salt$")`
// would produce on the device, and Go's standard library does not ship a
// crypt() equivalent.
//
// We avoid pulling in github.com/GehirnInc/crypt or similar to keep the
// dependency surface tiny — matching the existing hand-rolled RSA in
// client.go. Output is byte-identical to `openssl passwd -5 -salt X P`.
// The algorithm spec is at https://www.akkadia.org/drepper/SHA-crypt.txt.

import (
	"crypto/sha256"
)

const sha256CryptAlphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func sha256Crypt(key, salt string) string {
	keyB := []byte(key)
	saltB := []byte(salt)
	if len(saltB) > 16 {
		saltB = saltB[:16]
	}

	// altResult = SHA256(key || salt || key)
	h := sha256.New()
	h.Write(keyB)
	h.Write(saltB)
	h.Write(keyB)
	altResult := h.Sum(nil)

	// a = SHA256(key || salt || altResult-tail || mix)
	a := sha256.New()
	a.Write(keyB)
	a.Write(saltB)
	cnt := len(keyB)
	for cnt > 32 {
		a.Write(altResult)
		cnt -= 32
	}
	a.Write(altResult[:cnt])
	for n := len(keyB); n > 0; n >>= 1 {
		if n&1 == 1 {
			a.Write(altResult)
		} else {
			a.Write(keyB)
		}
	}
	altResult = a.Sum(nil)

	// dpResult = SHA256(key repeated len(key) times)
	dp := sha256.New()
	for i := 0; i < len(keyB); i++ {
		dp.Write(keyB)
	}
	dpResult := dp.Sum(nil)

	// p = first len(key) bytes of dpResult, cyclic
	p := make([]byte, len(keyB))
	for i := 0; i < len(p); i += 32 {
		n := len(p) - i
		if n > 32 {
			n = 32
		}
		copy(p[i:i+n], dpResult[:n])
	}

	// dsResult = SHA256(salt repeated (16 + altResult[0]) times)
	ds := sha256.New()
	for i := 0; i < 16+int(altResult[0]); i++ {
		ds.Write(saltB)
	}
	dsResult := ds.Sum(nil)

	// s = first len(salt) bytes of dsResult, cyclic
	s := make([]byte, len(saltB))
	for i := 0; i < len(s); i += 32 {
		n := len(s) - i
		if n > 32 {
			n = 32
		}
		copy(s[i:i+n], dsResult[:n])
	}

	// 5000 rounds (default). The challenge response carries alg=5 with no
	// rounds= override, so we never need to honour a different round count.
	cur := altResult
	for i := 0; i < 5000; i++ {
		ctx := sha256.New()
		if i%2 != 0 {
			ctx.Write(p)
		} else {
			ctx.Write(cur)
		}
		if i%3 != 0 {
			ctx.Write(s)
		}
		if i%7 != 0 {
			ctx.Write(p)
		}
		if i%2 != 0 {
			ctx.Write(cur)
		} else {
			ctx.Write(p)
		}
		cur = ctx.Sum(nil)
	}

	// Output: 43 chars from the permuted base64-like encoding. Triples are
	// fed (b2, b1, b0) ordered to match glibc's __sha256_crypt impl.
	var out [43]byte
	idx := 0
	encode := func(b2, b1, b0 byte, n int) {
		w := (uint32(b2) << 16) | (uint32(b1) << 8) | uint32(b0)
		for j := 0; j < n; j++ {
			out[idx] = sha256CryptAlphabet[w&0x3f]
			idx++
			w >>= 6
		}
	}
	encode(cur[0], cur[10], cur[20], 4)
	encode(cur[21], cur[1], cur[11], 4)
	encode(cur[12], cur[22], cur[2], 4)
	encode(cur[3], cur[13], cur[23], 4)
	encode(cur[24], cur[4], cur[14], 4)
	encode(cur[15], cur[25], cur[5], 4)
	encode(cur[6], cur[16], cur[26], 4)
	encode(cur[27], cur[7], cur[17], 4)
	encode(cur[18], cur[28], cur[8], 4)
	encode(cur[9], cur[19], cur[29], 4)
	encode(0, cur[31], cur[30], 3)

	return "$5$" + string(saltB) + "$" + string(out[:])
}
