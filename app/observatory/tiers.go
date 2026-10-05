package observatory

import (
	"sort"
	"strings"
)

// tierOf returns the tag prefix up to and including the second dash
// ("relay-00-01" → "relay-00-", "direct-00" → "direct-"), or the whole tag
// when it has no dash. Outbounds sharing a tier are probed together; tiers are
// probed in lexicographic order = the priority order the PriorityStrategy uses.
func tierOf(tag string) string {
	first := strings.Index(tag, "-")
	if first == -1 {
		return tag
	}
	second := strings.Index(tag[first+1:], "-")
	if second == -1 {
		return tag[:first+1]
	}
	return tag[:first+1+second+1]
}

// groupByTier splits tags into tiers (ordered by tier key, tags sorted inside).
func groupByTier(tags []string) [][]string {
	byTier := map[string][]string{}
	for _, t := range tags {
		k := tierOf(t)
		byTier[k] = append(byTier[k], t)
	}
	keys := make([]string, 0, len(byTier))
	for k := range byTier {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([][]string, 0, len(keys))
	for _, k := range keys {
		sort.Strings(byTier[k])
		out = append(out, byTier[k])
	}
	return out
}
