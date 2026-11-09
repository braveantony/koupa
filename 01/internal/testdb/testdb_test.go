package testdb_test

import (
	"context"
	"testing"

	"koupa/internal/testdb"
)

func TestNewGivesIsolatedDatabase(t *testing.T) {
	ctx := context.Background()
	a := testdb.New(t)
	b := testdb.New(t)

	var na, nb string
	if err := a.QueryRow(ctx, "SELECT current_database()").Scan(&na); err != nil {
		t.Fatal(err)
	}
	if err := b.QueryRow(ctx, "SELECT current_database()").Scan(&nb); err != nil {
		t.Fatal(err)
	}
	if na == nb {
		t.Fatalf("both pools use database %q", na)
	}

	if _, err := a.Exec(ctx, "CREATE TABLE only_in_a (id int)"); err != nil {
		t.Fatal(err)
	}
	var reg *string
	if err := b.QueryRow(ctx, "SELECT to_regclass('only_in_a')::text").Scan(&reg); err != nil {
		t.Fatal(err)
	}
	if reg != nil {
		t.Fatalf("table created in first db is visible in second: %s", *reg)
	}
}
