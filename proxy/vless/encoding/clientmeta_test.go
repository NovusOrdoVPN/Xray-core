package encoding

import (
	"strings"
	"testing"
)

func TestParseClientMetaTwoPart(t *testing.T) {
	m := ParseClientMeta("1.2.0|3")
	if m != (ClientMeta{}) {
		t.Fatalf("two-part string must yield zero meta, got %+v", m)
	}
	if ParseClientMeta("") != (ClientMeta{}) {
		t.Fatal("empty string must yield zero meta")
	}
}

func TestParseClientMetaThreePart(t *testing.T) {
	m := ParseClientMeta("1.3.1|3|os=ios;osv=18.6;net=cell;tz=Europe/Moscow;city=%D0%A1%D0%BE%D1%87%D0%B8;asn=31163;bridge=1;via=relay-01-00;zzz=1;os=android")
	want := ClientMeta{OS: "ios", OSV: "18.6", Net: "cell", TZ: "Europe/Moscow", City: "Сочи", ASN: 31163, Bridge: true, Via: "relay-01-00"}
	if m != want {
		t.Fatalf("got %+v want %+v", m, want)
	}
}

func TestParseClientMetaHostile(t *testing.T) {
	m := ParseClientMeta("1.3.1|3|" + strings.Repeat("x", 10000) + ";os=ios;=;net;asn=abc")
	if m.ASN != 0 {
		t.Fatalf("bad asn must be 0, got %d", m.ASN)
	}
	for _, s := range []string{"|||||", "1|2|a=%E0%A4%A", "1|2|os=" + strings.Repeat("y", 500)} {
		_ = ParseClientMeta(s) // must not panic
	}
	if got := ParseClientMeta("1|2|os=" + strings.Repeat("y", 500)).OS; len(got) > 16 {
		t.Fatalf("os must be capped at 16 bytes, got %d", len(got))
	}
}

func TestAppendVia(t *testing.T) {
	cases := map[string]string{
		"":                       "",
		"1.3.1":                  "1.3.1||via=relay-00-00",
		"1.3.1|3":                "1.3.1|3|via=relay-00-00",
		"1.3.1|3|os=ios":         "1.3.1|3|os=ios;via=relay-00-00",
		"1.3.1|3|via=old;os=ios": "1.3.1|3|os=ios;via=relay-00-00",
	}
	for in, want := range cases {
		if got := AppendVia(in, "relay-00-00"); got != want {
			t.Errorf("AppendVia(%q) = %q, want %q", in, got, want)
		}
	}
	long := "1.3.1|3|city=" + strings.Repeat("c", 300)
	got := AppendVia(long, "direct-00")
	if len(got) > 256 || !strings.HasSuffix(got, ";via=direct-00") {
		t.Fatalf("via must survive the 256-byte cap at the end, got len %d suffix %q", len(got), got[max(0, len(got)-20):])
	}
	if AppendVia("1.3.1|3", "") != "1.3.1|3" {
		t.Fatal("empty tag must not change the string")
	}
}
