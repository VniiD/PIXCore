package events

import (
	"strings"
	"testing"
)

func TestSignVerify(t *testing.T) {
	k1 := []byte(strings.Repeat("a", 32))
	k2 := []byte(strings.Repeat("b", 32))
	ring, err := NewKeyring("k2", map[string][]byte{"k1": k1, "k2": k2})
	if err != nil {
		t.Fatal(err)
	}
	p := []byte(`{"x":1}`)
	h := ring.Sign(p)
	if err := ring.Verify(p, h); err != nil {
		t.Fatal(err)
	}
	if ring.Verify([]byte(`{"x":2}`), h) == nil {
		t.Error("tampered payload accepted")
	}
	if ring.Verify(p, "kid=zz,sig=00") == nil || ring.Verify(p, "") == nil {
		t.Error("unknown kid / missing header accepted")
	}
	old, _ := NewKeyring("k1", map[string][]byte{"k1": k1})
	if err := ring.Verify(p, old.Sign(p)); err != nil {
		t.Errorf("rotation: %v", err)
	}
	if _, err := NewKeyring("k", map[string][]byte{"k": []byte("short")}); err == nil {
		t.Error("short key accepted")
	}
}

func TestDecode(t *testing.T) {
	if _, err := Decode([]byte(`{"specversion":"1.0"}`)); err == nil {
		t.Error("malformed accepted")
	}
	e, _ := New("id1", "t", "s", "sub", map[string]int{"a": 1})
	if e.ID != "id1" || string(e.Data) != `{"a":1}` {
		t.Error("new")
	}
}
