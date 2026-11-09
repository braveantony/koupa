package store

import (
	"testing"

	"koupa/internal/testdb"
)

func TestListMenuAndSetAvailable(t *testing.T) {
	pool := testdb.New(t)
	if _, err := SetAvailable(ctx(t), pool, []int64{4, 2}, false); err != nil {
		t.Fatal(err)
	}
	menu, err := ListMenu(ctx(t), pool)
	if err != nil || len(menu) != 4 {
		t.Fatalf("menu %+v err %v", menu, err)
	}
	for _, m := range menu {
		want := m.ID == 1 || m.ID == 3
		if m.Available != want {
			t.Fatalf("item %d available=%v", m.ID, m.Available)
		}
	}
}
