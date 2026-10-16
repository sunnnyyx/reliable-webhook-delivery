package security

import (
	"bytes"
	"testing"
)

func TestCredentialAndID(t *testing.T) {
	a, h, e := Token()
	if e != nil {
		t.Fatal(e)
	}
	b, _, e := Token()
	if e != nil {
		t.Fatal(e)
	}
	if a == b || !ValidToken(a) || len(h) != 32 || bytes.Equal(h, []byte(a)) {
		t.Fatal("invalid token generation")
	}
	for _, bad := range []string{"", "rf_wrong", a + "x", "xx_" + a[3:]} {
		if ValidToken(bad) {
			t.Fatal("accepted invalid token")
		}
	}
	id := ID()
	if !ValidID(id) || ValidID(id+"x") {
		t.Fatal("invalid identifier")
	}
}
func TestVaultBindsIdentityAndDetectsTampering(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	v, e := NewVault(key)
	if e != nil {
		t.Fatal(e)
	}
	plain := bytes.Repeat([]byte{11}, 32)
	cipher, nonce, e := v.Encrypt("t", "e", "s", plain)
	if e != nil {
		t.Fatal(e)
	}
	got, e := v.Decrypt("t", "e", "s", cipher, nonce)
	if e != nil || !bytes.Equal(got, plain) {
		t.Fatal("round trip failed")
	}
	_, nonce2, e := v.Encrypt("t", "e", "s", plain)
	if e != nil || bytes.Equal(nonce, nonce2) {
		t.Fatal("nonce reuse")
	}
	for _, ids := range [][3]string{{"other", "e", "s"}, {"t", "other", "s"}, {"t", "e", "other"}} {
		if _, e = v.Decrypt(ids[0], ids[1], ids[2], cipher, nonce); e == nil {
			t.Fatal("identity substitution accepted")
		}
	}
	cipher[0] ^= 1
	if _, e = v.Decrypt("t", "e", "s", cipher, nonce); e == nil {
		t.Fatal("tampering accepted")
	}
	if _, e = v.Decrypt("t", "e", "s", cipher, nonce[:2]); e == nil {
		t.Fatal("short nonce accepted")
	}
	if _, e = NewVault([]byte("short")); e == nil {
		t.Fatal("short key accepted")
	}
	if _, _, e = v.Encrypt("t", "e", "s", nil); e == nil {
		t.Fatal("short secret accepted")
	}
	other, _ := NewVault(bytes.Repeat([]byte{8}, 32))
	cipher[0] ^= 1
	if _, e = other.Decrypt("t", "e", "s", cipher, nonce); e == nil {
		t.Fatal("wrong key accepted")
	}
}
