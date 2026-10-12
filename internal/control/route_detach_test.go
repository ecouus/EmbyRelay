package control

import (
	"embyrelay/migrations"
	"reflect"
	"testing"
)

func TestDeletedRouteDetachesAccountAndPreservesUsage(t *testing.T) {
	s, err := testStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	apply := migrations.Apply
	if s.DB.Driver == "postgres" {
		apply = migrations.ApplyPostgres
	}
	c := AccessAccount{ID: "account", Username: "subscriber", RouteID: "route", KeyID: "key", Credentials: "sealed", PasswordHash: "hash", Plan: AccessPlan{Name: "Private", TrafficBytes: 1000}, ExpiresAt: 9999999999, Enabled: true}
	for kind, value := range map[string]any{"access_account": c, "route": Route{ID: "route"}, "key": Key{ID: "key", RouteID: "route"}} {
		id := kind
		if kind == "access_account" {
			id = c.ID
		}
		if kind == "route" {
			id = "route"
		}
		if kind == "key" {
			id = "key"
		}
		if err = s.put(kind, id, value, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.DB.Exec("INSERT INTO access_usage(account_id,cycle,bytes) VALUES('account','cycle',123)"); err != nil {
		t.Fatal(err)
	}
	if err = s.remove("route", "route", "admin"); err != nil {
		t.Fatal(err)
	}
	expected := c
	expected.RouteID = ""
	expected.KeyID = ""
	expected.Credentials = ""
	got, err := get[AccessAccount](s, "access_account", c.ID)
	if err != nil || !reflect.DeepEqual(got, expected) {
		t.Fatalf("account changed: %+v %v", got, err)
	}
	var used int64
	if err = s.DB.QueryRow("SELECT bytes FROM access_usage WHERE account_id='account'").Scan(&used); err != nil || used != 123 {
		t.Fatalf("usage changed: %d %v", used, err)
	}
	if _, err = get[Key](s, "key", "key"); err == nil {
		t.Fatal("route key retained")
	}
	valid := c
	valid.ID, valid.Username, valid.RouteID = "valid", "valid-subscriber", "live-route"
	if err = s.put("route", "live-route", Route{ID: "live-route"}, "admin"); err != nil {
		t.Fatal(err)
	}
	if err = s.put("access_account", valid.ID, valid, "admin"); err != nil {
		t.Fatal(err)
	}
	// Recreate a legacy dangling assignment and rerun only the new migration.
	if err = s.put("access_account", c.ID, c, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("DELETE FROM schema_migrations WHERE version=13"); err != nil {
		t.Fatal(err)
	}
	if err = apply(s.DB.DB); err != nil {
		t.Fatal(err)
	}
	got, err = get[AccessAccount](s, "access_account", c.ID)
	if err != nil || !reflect.DeepEqual(got, expected) {
		t.Fatalf("legacy account changed: %+v %v", got, err)
	}
	unchanged, err := get[AccessAccount](s, "access_account", valid.ID)
	if err != nil || !reflect.DeepEqual(unchanged, valid) {
		t.Fatalf("valid assignment changed: %+v %v", unchanged, err)
	}
	if err = apply(s.DB.DB); err != nil {
		t.Fatal(err)
	}
}
