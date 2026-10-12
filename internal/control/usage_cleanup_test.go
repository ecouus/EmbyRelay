package control

import (
	"context"
	"embyrelay/migrations"
	"testing"
	"time"
)

func TestUsageRetentionPreservesCurrentAndUnsettledCycles(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	for _, period := range []string{"total", "month", "days"} {
		t.Run(period, func(t *testing.T) {
			store, err := testStore(t)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			account := AccessAccount{ID: "account", Username: "viewer", StartedAt: now.Add(-400 * 24 * time.Hour).Unix(), ResetAt: 1,
				Plan: AccessPlan{TrafficPeriod: period, PeriodDays: 30}}
			if err = store.put("access_account", account.ID, account, "test"); err != nil {
				t.Fatal(err)
			}
			current, _ := accountCycle(account, now)
			old := now.Add(-accessUsageRetention - time.Second).Unix()
			for cycle, retired := range map[string]int64{
				current: old, "fresh-history": 0, "recent-history": now.Add(-89 * 24 * time.Hour).Unix(),
				"expired-history": old, "unsettled-history": old,
			} {
				if _, err = store.DB.Exec("INSERT INTO access_usage(account_id,cycle,bytes,retired_at) VALUES(?,?,?,?)", account.ID, cycle, 1234, retired); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = store.DB.Exec(`INSERT INTO access_leases(id,account_id,token_hash,node_id,expires_at,is_stream,charged_cycle,charged_bytes)
				VALUES('lease',?,'hash','local',?,1,'unsettled-history',100)`, account.ID, now.Add(-time.Hour).Unix()); err != nil {
				t.Fatal(err)
			}
			if err = store.cleanupAccessUsage(now); err != nil {
				t.Fatal(err)
			}
			for cycle, wantRetired := range map[string]int64{current: 0, "fresh-history": now.Unix(), "recent-history": now.Add(-89 * 24 * time.Hour).Unix(), "unsettled-history": 0} {
				var bytes, retired int64
				if err = store.DB.QueryRow("SELECT bytes,retired_at FROM access_usage WHERE account_id=? AND cycle=?", account.ID, cycle).Scan(&bytes, &retired); err != nil || bytes != 1234 || retired != wantRetired {
					t.Fatalf("cycle %s: bytes=%d retired=%d want=%d error=%v", cycle, bytes, retired, wantRetired, err)
				}
			}
			var count int
			if err = store.DB.QueryRow("SELECT COUNT(*) FROM access_usage WHERE cycle='expired-history'").Scan(&count); err != nil || count != 0 {
				t.Fatal("expired history retained", count, err)
			}
			if _, err = store.DB.Exec("DELETE FROM access_leases WHERE id='lease'"); err != nil {
				t.Fatal(err)
			}
			if err = store.cleanupAccessUsage(now); err != nil {
				t.Fatal(err)
			}
			if err = store.cleanupAccessUsage(now.Add(accessUsageRetention)); err != nil {
				t.Fatal(err)
			}
			if err = store.DB.QueryRow("SELECT COUNT(*) FROM access_usage WHERE cycle='unsettled-history'").Scan(&count); err != nil || count != 1 {
				t.Fatal("history removed before full retention elapsed", count, err)
			}
			if err = store.cleanupAccessUsage(now.Add(accessUsageRetention + time.Second)); err != nil {
				t.Fatal(err)
			}
			if err = store.DB.QueryRow("SELECT COUNT(*) FROM access_usage WHERE cycle='unsettled-history'").Scan(&count); err != nil || count != 0 {
				t.Fatal("settled history not removed after retention", count, err)
			}
		})
	}
}

func TestUsageCleanupProtectsLegacyLeaseAndDeletedAccountSettlement(t *testing.T) {
	store, err := testStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	account := AccessAccount{ID: "account", Username: "viewer", StartedAt: now.Unix(), Plan: AccessPlan{TrafficPeriod: "total"}}
	if err = store.put("access_account", account.ID, account, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB.Exec("INSERT INTO access_usage(account_id,cycle,bytes,retired_at) VALUES(?,'old',20,?)", account.ID, now.Add(-100*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB.Exec("INSERT INTO access_leases(id,account_id,token_hash,node_id,expires_at,is_stream) VALUES('legacy',?,'hash','local',?,1)", account.ID, now.Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	store.Cleanup()
	var retired, count int64
	if err = store.DB.QueryRow("SELECT retired_at FROM access_usage WHERE account_id=?", account.ID).Scan(&retired); err != nil || retired != 0 {
		t.Fatal("legacy settlement history removed", retired, err)
	}
	if err = store.DB.QueryRow("SELECT COUNT(*) FROM access_leases WHERE id='legacy'").Scan(&count); err != nil || count != 1 {
		t.Fatal("recent expired lease removed", count, err)
	}
	if _, err = store.DB.Exec("DELETE FROM objects WHERE kind='access_account' AND id=?", account.ID); err != nil {
		t.Fatal(err)
	}
	store.Cleanup()
	if err = store.DB.QueryRow("SELECT COUNT(*) FROM access_usage WHERE account_id=?", account.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("deleted account history removed before settlement", count, err)
	}
	if _, err = store.DB.Exec("UPDATE access_leases SET expires_at=? WHERE id='legacy'", now.Add(-accessSettlementGrace-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	store.Cleanup()
	if err = store.DB.QueryRow("SELECT COUNT(*) FROM access_usage WHERE account_id=?", account.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("orphan history not removed", count, err)
	}
	if err = store.DB.QueryRow("SELECT COUNT(*) FROM access_leases WHERE id='legacy'").Scan(&count); err != nil || count != 0 {
		t.Fatal("stale lease not removed", count, err)
	}
}

func TestSubscriptionRefundUsesChargedCycleAfterReset(t *testing.T) {
	a, admin, account, prefix := subscriptionFixture(t)
	token := subscriptionLogin(t, a, prefix)
	account, err := get[AccessAccount](a.Store, "access_account", account.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldCycle, _ := accountCycle(account, time.Now())
	cmd := AccessCommand{AccountID: account.ID, KeyID: account.KeyID, Token: token, Action: "begin", Stream: true}
	lease, err := a.accessCall(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Lease, cmd.Action, cmd.Bytes = lease.Lease, "charge", 1000
	if _, err = a.accessCall(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if response := call(t, a, admin, "POST", "/api/v1/access-accounts/"+account.ID+"/reset-usage", map[string]any{}); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	account, err = get[AccessAccount](a.Store, "access_account", account.ID)
	if err != nil {
		t.Fatal(err)
	}
	newCycle, _ := accountCycle(account, time.Now())
	if newCycle == oldCycle {
		t.Fatal("cycle did not change")
	}
	if _, err = a.Store.DB.Exec("INSERT INTO access_usage(account_id,cycle,bytes) VALUES(?,?,?)", account.ID, newCycle, 500); err != nil {
		t.Fatal(err)
	}
	// Even an old retirement marker must be cleared while a charged lease is live.
	if _, err = a.Store.DB.Exec("UPDATE access_usage SET retired_at=? WHERE account_id=? AND cycle=?", time.Now().Add(-100*24*time.Hour).Unix(), account.ID, oldCycle); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Store.DB.Exec("UPDATE access_leases SET expires_at=? WHERE id=?", time.Now().Add(-time.Hour).Unix(), lease.Lease); err != nil {
		t.Fatal(err)
	}
	a.Store.Cleanup()
	cmd.Action, cmd.RefundBytes = "finish", 1001
	if _, err = a.accessCall(context.Background(), cmd); err == nil {
		t.Fatal("refund exceeded last reserved chunk")
	}
	cmd.RefundBytes = 400
	for i := 0; i < 2; i++ {
		if _, err = a.accessCall(context.Background(), cmd); err != nil {
			t.Fatal(err)
		}
	}
	for cycle, want := range map[string]int64{oldCycle: 1200, newCycle: 500} {
		var used int64
		if err = a.Store.DB.QueryRow("SELECT bytes FROM access_usage WHERE account_id=? AND cycle=?", account.ID, cycle).Scan(&used); err != nil || used != want {
			t.Fatalf("cycle %s usage=%d want=%d error=%v", cycle, used, want, err)
		}
	}
}

func TestUsageCleanupPreservesUnparseableAccount(t *testing.T) {
	store, err := testStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.DB.Exec(`INSERT INTO objects VALUES('access_account','broken','{"username":"broken","started_at":"invalid"}',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB.Exec("INSERT INTO access_usage(account_id,cycle,bytes,retired_at) VALUES('broken','old',200,1)"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB.Exec("INSERT INTO access_usage(account_id,cycle,bytes) VALUES('orphan','old',10)"); err != nil {
		t.Fatal(err)
	}
	if err = store.cleanupAccessUsage(time.Now()); err == nil {
		t.Fatal("invalid account was not reported")
	}
	var used int64
	if err = store.DB.QueryRow("SELECT bytes FROM access_usage WHERE account_id='broken'").Scan(&used); err != nil || used != 200 {
		t.Fatal("unparseable account accounting changed", used, err)
	}
	if err = store.DB.QueryRow("SELECT COUNT(*) FROM access_usage WHERE account_id='orphan'").Scan(&used); err != nil || used != 0 {
		t.Fatal("invalid account blocked unrelated cleanup", used, err)
	}
}

func TestSubscriptionRefundRejectsMissingOrStaleReservation(t *testing.T) {
	a, _, account, prefix := subscriptionFixture(t)
	token := subscriptionLogin(t, a, prefix)
	cmd := AccessCommand{AccountID: account.ID, KeyID: account.KeyID, Token: token, Action: "begin"}
	lease, err := a.accessCall(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Lease, cmd.Action, cmd.RefundBytes = lease.Lease, "finish", 1
	if _, err = a.accessCall(context.Background(), cmd); err == nil {
		t.Fatal("refund accepted without a reservation")
	}
	cmd.Action, cmd.Bytes, cmd.RefundBytes = "charge", 100, 0
	if _, err = a.accessCall(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Store.DB.Exec("UPDATE access_leases SET expires_at=? WHERE id=?", time.Now().Add(-accessSettlementGrace-time.Hour).Unix(), lease.Lease); err != nil {
		t.Fatal(err)
	}
	cmd.Action, cmd.RefundBytes = "finish", 50
	if _, err = a.accessCall(context.Background(), cmd); err == nil {
		t.Fatal("refund accepted after settlement deadline")
	}
	var used int64
	if err = a.Store.DB.QueryRow("SELECT bytes FROM access_usage WHERE account_id=?", account.ID).Scan(&used); err != nil || used != 200 {
		t.Fatal("rejected refund changed billing", used, err)
	}
}

// Exercise the same upgrade on both engines, including a real PostgreSQL
// instance when the existing test database environment variable is configured.
func TestUsageRetentionSchemaUpgrade(t *testing.T) {
	store, err := testStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Restore the shape of schema 11 inside this disposable test database.
	for _, query := range []string{
		"DROP INDEX access_usage_retired",
		"ALTER TABLE access_usage DROP COLUMN retired_at",
		"ALTER TABLE access_leases DROP COLUMN charged_cycle",
		"ALTER TABLE access_leases DROP COLUMN charged_bytes",
		"CREATE TABLE relay_reports(node_id TEXT NOT NULL,report_id TEXT NOT NULL,created_at BIGINT NOT NULL,PRIMARY KEY(node_id,report_id))",
		"DELETE FROM schema_migrations WHERE version=12",
		"INSERT INTO access_usage VALUES('account','old-cycle',123456789)",
		"INSERT INTO access_leases VALUES('legacy','account','hash','node',9999999999,1)",
		"INSERT INTO relay_reports VALUES('node','report',1)",
	} {
		if _, err = store.DB.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	apply := migrations.Apply
	if store.DB.Driver == "postgres" {
		apply = migrations.ApplyPostgres
	}
	if err = apply(store.DB.DB); err != nil {
		t.Fatal(err)
	}
	var used, retired, reserved int64
	var cycle string
	if err = store.DB.QueryRow("SELECT bytes,retired_at FROM access_usage").Scan(&used, &retired); err != nil || used != 123456789 || retired != 0 {
		t.Fatal("upgrade changed legacy accounting", used, retired, err)
	}
	if err = store.DB.QueryRow("SELECT charged_cycle,charged_bytes FROM access_leases").Scan(&cycle, &reserved); err != nil || cycle != "" || reserved != 0 {
		t.Fatal("upgrade changed legacy lease", cycle, reserved, err)
	}
	if _, err = store.DB.Exec("SELECT * FROM relay_reports"); err == nil {
		t.Fatal("obsolete reports table retained")
	}
	if err = apply(store.DB.DB); err != nil {
		t.Fatal("repeat migration", err)
	}
}
