package keys

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewKeyRoundTrips(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		id, presented, hash, err := newKey()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(presented, Prefix+id+"_") || len(id) != idLen {
			t.Fatalf("presented %q, id %q", presented, id)
		}
		gotID, secret, err := parse(presented)
		if err != nil || gotID != id {
			t.Fatalf("parse(%q) = %q, %v", presented, gotID, err)
		}
		if !matches(secret, hash) {
			t.Fatal("a freshly issued key doesn't match its own hash")
		}
		if bytes.Contains(hash, []byte(secret)) || strings.Contains(string(hash), secret) {
			t.Fatal("the stored hash contains the secret")
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestParseRejects(t *testing.T) {
	_, good, _, _ := newKey()
	for _, bad := range []string{
		"",
		"booth_ak_",
		strings.TrimPrefix(good, Prefix), // no prefix
		"Bearer " + good,                 // header value, not a key
		strings.Replace(good, Prefix, "booth_xx_", 1), // wrong prefix
		good[:len(good)-1], // truncated secret
		good + "A",         // over-long secret
		Prefix + "AAAAAAAAAAAAA_" + good[len(Prefix)+idLen+1:], // uppercase id
		Prefix + "aaaaaaaaaaaa1_" + good[len(Prefix)+idLen+1:], // '1' is not base32
		"eyJhbGciOiJSUzI1NiJ9.e30.sig",                         // a JWT
	} {
		if _, _, err := parse(bad); err == nil {
			t.Errorf("parse(%q) accepted", bad)
		}
	}
}

func TestMatchesRejectsOtherSecrets(t *testing.T) {
	_, a, hashA, _ := newKey()
	_, b, _, _ := newKey()
	_, secretA, _ := parse(a)
	_, secretB, _ := parse(b)
	if !matches(secretA, hashA) || matches(secretB, hashA) {
		t.Error("hash comparison is wrong")
	}
}
