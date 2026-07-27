package store

import "testing"

func TestDecodeBoardsMarksCurrent(t *testing.T) {
	raw := []byte(`[
	  {"name":"default","open":1,"total":1,"current":true},
	  {"name":"web","open":1,"total":4,"current":false}
	]`)
	ps, err := decodeBoards(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 {
		t.Fatalf("got %d boards, want 2", len(ps))
	}
	if ps[0].Name != "default" || !ps[0].Current {
		t.Errorf("default should be current: %+v", ps[0])
	}
	if ps[1].Current {
		t.Errorf("web should not be current: %+v", ps[1])
	}
	if ps[1].Total != 4 {
		t.Errorf("web total = %d, want 4", ps[1].Total)
	}
}
