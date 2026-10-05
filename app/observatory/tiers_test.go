package observatory

import (
	"reflect"
	"testing"
)

func TestTierOf(t *testing.T) {
	cases := map[string]string{
		"direct-00":   "direct-",
		"direct-01":   "direct-",
		"relay-00-01": "relay-00-",
		"relay-01-00": "relay-01-",
		"relay-00":    "relay-",
		"proxy":       "proxy",
	}
	for tag, want := range cases {
		if got := tierOf(tag); got != want {
			t.Errorf("tierOf(%q) = %q, want %q", tag, got, want)
		}
	}
}

func TestGroupByTierOrdersTiersAndTags(t *testing.T) {
	got := groupByTier([]string{"relay-01-00", "direct-01", "relay-00-01", "direct-00", "relay-00-00"})
	want := [][]string{{"direct-00", "direct-01"}, {"relay-00-00", "relay-00-01"}, {"relay-01-00"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("groupByTier = %v, want %v", got, want)
	}
}
