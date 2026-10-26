package delegation

import "testing"

func TestOfflineVerifierEncryptionBoundToPrincipal(t *testing.T) {
	key := make([]byte, 32)
	cipher, e := sealSecret(key, []byte("verifier"), []byte("owner"))
	if e != nil {
		t.Fatal(e)
	}
	plain, e := openSecret(key, cipher, []byte("owner"))
	if e != nil || string(plain) != "verifier" {
		t.Fatal(e)
	}
	if _, e = openSecret(key, cipher, []byte("other")); e == nil {
		t.Fatal("wrong principal decrypted")
	}
	cipher[len(cipher)-1] ^= 1
	if _, e = openSecret(key, cipher, []byte("owner")); e == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}
