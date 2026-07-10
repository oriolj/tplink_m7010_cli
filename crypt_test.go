package main

import "testing"

// Vectors generated with `openssl passwd -5 -salt <salt> <key>` (glibc's
// SHA-256 crypt, 5000 default rounds). The saltstring/"Hello world!" pair
// is also the canonical vector from Drepper's SHA-crypt spec.
func TestSha256Crypt(t *testing.T) {
	cases := []struct {
		key, salt, want string
	}{
		{"Hello world!", "saltstring",
			"$5$saltstring$5B8vYYiY.CVt1RlTTf8KbXBH3hsxY/GNooZaBBGWEc5"},
		{"correcthorse", "8cZ9zPFLRxER8bEK",
			"$5$8cZ9zPFLRxER8bEK$Hk8/9k1.Hmjo1JuAQs1NiaZQRPWnaYO4PDHUzDM/Cb1"},
		{"a", "short",
			"$5$short$26OgkZnNPH/fhIJiNDSUG8GpRVc3tG42gPClFWxDDA2"},
		// Salt longer than 16 chars must be truncated, matching crypt(3).
		{"a very long password that exceeds thirty-two bytes easily", "0123456789ABCDEFXXXX",
			"$5$0123456789ABCDEF$rlYdOSUQLhmS/isTltiAWk/9qxW79vHUrlB.xdupBq6"},
	}
	for _, c := range cases {
		if got := sha256Crypt(c.key, c.salt); got != c.want {
			t.Errorf("sha256Crypt(%q, %q) = %q, want %q", c.key, c.salt, got, c.want)
		}
	}
}
