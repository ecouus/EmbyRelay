package control

import (
	"context"
	"testing"
	"time"
)

func TestSubscriptionSettlesPreviousShortWriteBeforeNextCharge(t *testing.T) {
	a, _, c, prefix := subscriptionFixture(t)
	token := subscriptionLogin(t, a, prefix)
	c, _ = get[AccessAccount](a.Store, "access_account", c.ID)
	cmd := AccessCommand{AccountID: c.ID, KeyID: c.KeyID, Token: token, Action: "begin"}
	lease, err := a.accessCall(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Lease = lease.Lease
	cmd.Action = "charge"
	cmd.Bytes = 30000
	if _, err = a.accessCall(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	// A failed next charge must roll back the accompanying previous refund.
	cmd.Bytes = streamAccessChunk + 1
	cmd.RefundBytes = 29000
	if _, err = a.accessCall(context.Background(), cmd); err == nil {
		t.Fatal("invalid next charge allowed")
	}
	var used int64
	if err = a.Store.DB.QueryRow("SELECT bytes FROM access_usage WHERE account_id=?", c.ID).Scan(&used); err != nil || used != 60000 {
		t.Fatal("failed charge changed prior billing", used, err)
	}
	cmd.Bytes = 20000
	if _, err = a.accessCall(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	cmd.Action = "finish"
	cmd.RefundBytes = 19000
	if _, err = a.accessCall(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if _, err = a.accessCall(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if err = a.Store.DB.QueryRow("SELECT bytes FROM access_usage WHERE account_id=?", c.ID).Scan(&used); err != nil || used != 4000 {
		t.Fatal("partial refunds were lost or repeated", used, err)
	}
}

func TestChargeRefundKeepsPreviousBillingCycle(t *testing.T) {
	a, admin, c, prefix := subscriptionFixture(t)
	token := subscriptionLogin(t, a, prefix)
	c, _ = get[AccessAccount](a.Store, "access_account", c.ID)
	oldCycle, _ := accountCycle(c, time.Now())
	cmd := AccessCommand{AccountID: c.ID, KeyID: c.KeyID, Token: token, Action: "begin"}
	lease, err := a.accessCall(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Lease = lease.Lease
	cmd.Action = "charge"
	cmd.Bytes = 1000
	if _, err = a.accessCall(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if r := call(t, a, admin, "POST", "/api/v1/access-accounts/"+c.ID+"/reset-usage", map[string]any{}); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	c, _ = get[AccessAccount](a.Store, "access_account", c.ID)
	newCycle, _ := accountCycle(c, time.Now())
	if oldCycle == newCycle {
		t.Fatal("cycle unchanged")
	}
	cmd.Bytes = 500
	cmd.RefundBytes = 900
	if _, err = a.accessCall(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	var oldUsed, newUsed int64
	if err = a.Store.DB.QueryRow("SELECT bytes FROM access_usage WHERE account_id=? AND cycle=?", c.ID, oldCycle).Scan(&oldUsed); err != nil {
		t.Fatal(err)
	}
	if err = a.Store.DB.QueryRow("SELECT bytes FROM access_usage WHERE account_id=? AND cycle=?", c.ID, newCycle).Scan(&newUsed); err != nil {
		t.Fatal(err)
	}
	if oldUsed != 200 || newUsed != 1000 {
		t.Fatal("refund applied to wrong cycle", oldUsed, newUsed)
	}
}
