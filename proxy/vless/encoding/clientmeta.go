package encoding

import (
	"net/url"
	"strconv"
	"strings"
)

// CUSTOM: client metadata carried in the third part of the client version string
// ("<app>|<cfg>|k=v;k=v;..."; spec 2026-10-06-client-telemetry-design §2).
// Parts 0–1 are the version checks the tower has always done; this part is
// telemetry only. Readers are tolerant: a malformed or oversized third part never
// affects anything, unknown keys are ignored, the first occurrence of a key wins.

// 200, not 256: the VLESS addons length on the wire is a single byte and the flow
// name plus protobuf framing take ~20 of the 255; see EncodeHeaderAddons.
const clientMetaMaxBytes = 200

// ClientMeta is the parsed third part. Zero value = "unknown everywhere".
type ClientMeta struct {
	OS, OSV, Net, TZ, City, Via string
	ASN                         uint32
	Bridge                      bool
}

var clientMetaWidth = map[string]int{"os": 16, "osv": 16, "net": 8, "tz": 40, "city": 64, "via": 32}

func clientMetaToken(s string, width int) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' || r == '/' || r == ':' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
		if b.Len() >= width {
			break
		}
	}
	return b.String()
}

// ParseClientMeta parses the third part of a client version string. Never panics.
func ParseClientMeta(s string) ClientMeta {
	var m ClientMeta
	if len(s) > clientMetaMaxBytes {
		s = s[:clientMetaMaxBytes]
	}
	parts := strings.SplitN(s, "|", 3)
	if len(parts) < 3 || parts[2] == "" {
		return m
	}
	seen := map[string]bool{}
	for _, pair := range strings.Split(parts[2], ";") {
		eq := strings.IndexByte(pair, '=')
		if eq <= 0 {
			continue
		}
		key := pair[:eq]
		if seen[key] {
			continue
		}
		raw := pair[eq+1:]
		if raw == "" {
			continue
		}
		val := raw
		if dec, err := url.PathUnescape(raw); err == nil {
			val = dec
		}
		switch key {
		case "os":
			m.OS = clientMetaToken(val, clientMetaWidth[key])
		case "osv":
			m.OSV = clientMetaToken(val, clientMetaWidth[key])
		case "net":
			m.Net = clientMetaToken(val, clientMetaWidth[key])
		case "tz":
			m.TZ = clientMetaToken(val, clientMetaWidth[key])
		case "via":
			m.Via = clientMetaToken(val, clientMetaWidth[key])
		case "city":
			// Cities may be non-ASCII; keep the string, bound the bytes at a rune boundary.
			c := val
			for len(c) > clientMetaWidth[key] {
				_, size := lastRune(c)
				c = c[:len(c)-size]
			}
			m.City = c
		case "asn":
			if n, err := strconv.ParseUint(val, 10, 32); err == nil && n > 0 {
				m.ASN = uint32(n)
			}
		case "bridge":
			m.Bridge = val == "1"
		default:
			continue
		}
		seen[key] = true
	}
	return m
}

func lastRune(s string) (rune, int) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i]&0xC0 != 0x80 {
			r := []rune(s[i:])
			if len(r) == 0 {
				return 0, 1
			}
			return r[0], len(s) - i
		}
	}
	return 0, 1
}

// clientVersionWithoutMeta drops the telemetry part: "a|b|k=v" → "a|b"; "a|b" / "a" unchanged.
func clientVersionWithoutMeta(v string) string {
	parts := strings.SplitN(v, "|", 3)
	if len(parts) < 3 {
		return v
	}
	return parts[0] + "|" + parts[1]
}

// AppendVia sets via=<tag> in the third part of the client version string, replacing
// any existing via. An empty string or tag is returned unchanged (an app that sends no
// version keeps sending none). The result is capped at 256 bytes by dropping the tail
// of the metadata, never the version parts and never the via itself.
func AppendVia(s, tag string) string {
	if s == "" || tag == "" {
		return s
	}
	parts := strings.SplitN(s, "|", 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	var kept []string
	for _, pair := range strings.Split(parts[2], ";") {
		if pair == "" || strings.HasPrefix(pair, "via=") {
			continue
		}
		kept = append(kept, pair)
	}
	via := "via=" + clientMetaToken(tag, clientMetaWidth["via"])
	head := parts[0] + "|" + parts[1] + "|"
	budget := clientMetaMaxBytes - len(head) - len(via) - 1 // room for the ';' before via
	meta := strings.Join(kept, ";")
	if budget < 0 {
		budget = 0
	}
	if len(meta) > budget {
		meta = meta[:budget]
		for len(meta) > 0 && meta[len(meta)-1]&0xC0 == 0x80 { // do not cut inside a UTF-8 rune
			meta = meta[:len(meta)-1]
		}
		if len(meta) > 0 && meta[len(meta)-1]&0x80 != 0 && meta[len(meta)-1]&0xC0 == 0xC0 {
			meta = meta[:len(meta)-1]
		}
	}
	if meta == "" {
		return head + via
	}
	return head + meta + ";" + via
}
