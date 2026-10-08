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
	secret := good[len(Prefix)+idLen+1:]
	cases := map[string]string{
		"empty":                "",
		"prefix only":          "booth_ak_",
		"no prefix":            strings.TrimPrefix(good, Prefix),
		"whole header value":   "Bearer " + good,
		"wrong prefix":         strings.Replace(good, Prefix, "booth_xx_", 1),
		"truncated secret":     good[:len(good)-1],
		"over-long secret":     good + "A",
		"uppercase id":         Prefix + "AAAAAAAAAAAAA_" + secret,
		"'1' is not in base32": Prefix + "aaaaaaaaaaaa1_" + secret,
		"a JWT":                "eyJhbGciOiJSUzI1NiJ9.e30.sig",
	}
	for name, bad := range cases {
		if _, _, err := parse(bad); err == nil {
			t.Errorf("%s: parse(%q) accepted", name, bad)
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
