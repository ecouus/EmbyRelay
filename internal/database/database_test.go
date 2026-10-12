package database

import "testing"

func TestDialectSQL(t *testing.T) {
	pg := &DB{Driver: "postgres"}
	cases := map[string]string{
		"SELECT ?, '?', \"?\", 'it''s?', ? -- ?\n/* ? */":                            "SELECT $1, '?', \"?\", 'it''s?', $2 -- ?\n/* ? */",
		"SELECT SUM(MAX(0,granted-reported)),MAX(version) WHERE id=?":                "SELECT SUM(GREATEST(0,granted-reported)),MAX(version) WHERE id=$1",
		"UPDATE quota_leases SET granted=MAX(quota_leases.granted,excluded.granted)": "UPDATE quota_leases SET granted=GREATEST(quota_leases.granted,excluded.granted)",
	}
	for query, expected := range cases {
		if got := pg.SQL(query); got != expected {
			t.Fatalf("got %q, want %q", got, expected)
		}
		if got := (&DB{Driver: "sqlite"}).SQL(query); got != query {
			t.Fatal("SQLite SQL was changed")
		}
	}
}
