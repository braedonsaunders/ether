package identity

import "testing"

func FuzzGrantParsing(f *testing.F) {
	f.Add("eg1_invalid")
	f.Add("")
	f.Add("eg1_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	f.Fuzz(func(t *testing.T, token string) {
		hash, ok := grantHash(token)
		if ok && (len(hash) != 64 || !IsGrantCredential(token)) {
			t.Fatal("invalid accepted credential")
		}
	})
}
