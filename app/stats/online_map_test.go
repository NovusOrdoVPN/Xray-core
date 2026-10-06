package stats

import "testing"

func TestOnlineMapAttr(t *testing.T) {
	om := NewOnlineMap()
	om.SetAttr("u1", "os=ios") // before AddIP: no-op
	found := false
	om.ForEachAttr(func(id string, _ int64, attr string) bool { found = true; return true })
	if found {
		t.Fatal("SetAttr before AddIP must not create an entry")
	}
	om.AddIP("u1")
	om.SetAttr("u1", "os=ios;net=cell")
	om.AddIP("u1") // second connection keeps the attr
	var got string
	om.ForEachAttr(func(id string, _ int64, attr string) bool { got = attr; return true })
	if got != "os=ios;net=cell" {
		t.Fatalf("attr not visible, got %q", got)
	}
	om.SetAttr("u1", string(make([]byte, 1000)))
	om.ForEachAttr(func(id string, _ int64, attr string) bool { got = attr; return true })
	if len(got) != 256 {
		t.Fatalf("attr must be capped at 256 bytes, got %d", len(got))
	}
	om.RemoveIP("u1")
	om.RemoveIP("u1")
	found = false
	om.ForEachAttr(func(string, int64, string) bool { found = true; return true })
	if found || om.Count() != 0 {
		t.Fatal("entry and attr must be gone at zero refcount")
	}
}
