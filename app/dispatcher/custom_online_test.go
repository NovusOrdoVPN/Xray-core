package dispatcher

import "testing"

func TestClientMetaPart(t *testing.T) {
	cases := map[string]string{"": "", "1.2.0": "", "1.2.0|3": "", "1.2.0|3|os=ios;via=direct-00": "os=ios;via=direct-00", "1|2|": ""}
	for in, want := range cases {
		if got := clientMetaPart(in); got != want {
			t.Errorf("clientMetaPart(%q) = %q want %q", in, got, want)
		}
	}
}
