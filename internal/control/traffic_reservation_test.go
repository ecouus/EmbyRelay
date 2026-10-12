package control

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTrafficExcludesConcurrentUnusedStreamCredits(t *testing.T) {
	a, admin := testApp(t)
	route, _ := configured(t, a, admin, "http://127.0.0.1:8096")
	writers := []*meterWriter{}
	for i := 0; i < 2; i++ {
		w := &meterWriter{ResponseWriter: httptest.NewRecorder(), app: a, route: route, usage: &routeRuntime{}, ctx: context.Background(), stream: true}
		writers = append(writers, w)
		if _, err := w.Write(bytes.Repeat([]byte("x"), 1<<20)); err != nil {
			t.Fatal(err)
		}
	}
	assertSent := func(expected int64) {
		t.Helper()
		traffic, err := a.Store.Traffic()
		if err != nil || len(traffic) != 1 || traffic[0].Egress != expected {
			t.Fatalf("actual traffic: %+v %v; want %d", traffic, err, expected)
		}
	}
	assertSent(2 << 20)
	// Outstanding credits remain charged for quota, even though heartbeat omits them.
	if err := a.Store.Charge(route.ID, 20<<20, 0, 1); !errors.Is(err, ErrQuota) {
		t.Fatalf("pending credit escaped quota: %v", err)
	}
	writers[0].finishRouteCredit()
	assertSent(2 << 20)
	writers[1].finishRouteCredit()
	assertSent(2 << 20)
	if len(a.Store.pendingTraffic) != 0 {
		t.Fatal("finished credit retained")
	}
}

func TestStreamCreditRefundUsesReservationDay(t *testing.T) {
	s, err := testStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	yesterday := time.Now().UTC().Add(-24 * time.Hour).Format("2006-01-02")
	if _, err = s.DB.Exec("INSERT INTO traffic_daily VALUES(?,?,?,?)", yesterday, "route", 100, 10<<20); err != nil {
		t.Fatal(err)
	}
	s.pendingTraffic = map[string]int64{yesterday + "/route": 8 << 20}
	if err = s.refundStreamTraffic("route", yesterday, 8<<20); err != nil {
		t.Fatal(err)
	}
	traffic, err := s.Traffic()
	if err != nil || len(traffic) != 1 || traffic[0].Egress != 2<<20 {
		t.Fatalf("cross-day refund: %+v %v", traffic, err)
	}
}

func TestActualHeartbeatReconcilesLegacyCountersOnlyOnce(t *testing.T) {
	a, admin := testApp(t)
	route, _ := configured(t, a, admin, "http://127.0.0.1:8096")
	node := Node{ID: "node", Enabled: true}
	if err := a.Store.put("node", node.ID, node, "test"); err != nil {
		t.Fatal(err)
	}
	route.RelayID = node.ID
	if err := a.Store.put("route", route.ID, route, "test"); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().Format("2006-01-02")
	legacy := Heartbeat{Traffic: []Traffic{{Date: day, RouteID: route.ID, Ingress: 100, Egress: 20 << 20}}}
	if err := a.report(node, legacy); err != nil {
		t.Fatal(err)
	}
	actual := Heartbeat{TrafficAccounting: "actual-v1", Traffic: []Traffic{{Date: day, RouteID: route.ID, Ingress: 100, Egress: 2 << 20}}}
	if err := a.report(node, actual); err != nil {
		t.Fatal(err)
	}
	if err := a.report(node, actual); err != nil {
		t.Fatalf("duplicate actual report: %v", err)
	}
	traffic, err := a.Store.Traffic()
	if err != nil || traffic[0].Egress != 2<<20 {
		t.Fatalf("legacy correction: %+v %v", traffic, err)
	}
	actual.Traffic[0].Egress = 0
	if err := a.report(node, actual); err == nil {
		t.Fatal("repeated large correction accepted")
	}
	if err := a.report(node, legacy); err == nil {
		t.Fatal("accounting downgrade accepted")
	}
	actual.Traffic[0].Egress = 3 << 20
	actual.Traffic[0].Ingress = 99
	if err := a.report(node, actual); err == nil {
		t.Fatal("ingress rollback accepted")
	}
}
