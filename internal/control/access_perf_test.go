package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAccessPacerBatchesRPCs(t *testing.T) {
	const rate = 4 << 20
	var calls, largest int64
	pacer := newAccessPacer(context.Background(), rate, func(ctx context.Context, n int64) (time.Duration, error) {
		atomic.AddInt64(&calls, 1)
		largest = max(largest, n)
		return 0, nil
	})
	started := time.Now()
	for sent := 0; sent < 300<<10; sent += 16 << 10 {
		if err := pacer.take(16 << 10); err != nil {
			t.Fatal(err)
		}
	}
	// 19 chunks used to be 19 RPCs; permits double from the minimum instead.
	if n := atomic.LoadInt64(&calls); n > 6 || largest > accessPaceBatch(rate) {
		t.Fatalf("pace calls = %d largest = %d", n, largest)
	}
	if elapsed := time.Since(started); elapsed < 50*time.Millisecond {
		t.Fatalf("permit bytes were not spread at the plan rate: %v", elapsed)
	}
}

func TestAccessPacerFallsBackForOldControl(t *testing.T) {
	const rate = 1 << 20
	small := int64(accessPaceChunk(rate))
	var calls atomic.Int64
	pacer := newAccessPacer(context.Background(), rate, func(ctx context.Context, n int64) (time.Duration, error) {
		calls.Add(1)
		if n > small {
			return 0, errors.New("invalid pace request")
		}
		return 0, nil
	})
	for i := 0; i < 8; i++ {
		if err := pacer.take(int(small)); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() < 8 {
		t.Fatalf("old controls must be asked for minimum permits, got %d calls", calls.Load())
	}
}

func TestAccessPacerWaitsForSharedSlot(t *testing.T) {
	var calls atomic.Int64
	pacer := newAccessPacer(context.Background(), 1<<20, func(ctx context.Context, n int64) (time.Duration, error) {
		if calls.Add(1) == 1 {
			return 30 * time.Millisecond, nil
		}
		return 0, nil
	})
	started := time.Now()
	if err := pacer.take(1024); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) < 25*time.Millisecond || calls.Load() != 2 {
		t.Fatal("wait from the shared limiter ignored", calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	blocked := newAccessPacer(ctx, 1<<20, func(ctx context.Context, n int64) (time.Duration, error) { return time.Hour, nil })
	if err := blocked.take(1); err == nil {
		t.Fatal("cancelled request kept waiting")
	}
}

func TestOrdinaryRequestsBatchSubscriptionCharges(t *testing.T) {
	a, admin := testApp(t)
	route, _ := configured(t, a, admin, "http://127.0.0.1:8096")
	var charges []int
	w := &meterWriter{
		ResponseWriter: httptest.NewRecorder(), app: a, route: route,
		usage: &routeRuntime{}, ctx: context.Background(),
		accessCharge: func(n int) error { charges = append(charges, n); return nil },
	}
	for sent := 0; sent < 600<<10; sent += 32 << 10 {
		if n, err := w.Write(bytes.Repeat([]byte("x"), 32<<10)); err != nil || n != 32<<10 {
			t.Fatalf("wrote %d: %v", n, err)
		}
	}
	// Used to be one charge per 32 KiB (19 here); credit ramps 64 KiB, 256 KiB, 1 MiB.
	if len(charges) != 3 || charges[0] != accessBatchStart || charges[2] != accessBatchChunk {
		t.Fatalf("charges = %v", charges)
	}
	if w.accessRefund != 0 || w.accessCredit != int64(charges[0]+charges[1]+charges[2])-19*32<<10 {
		t.Fatalf("unused credit must be refunded by finish: credit=%d refund=%d", w.accessCredit, w.accessRefund)
	}
}

func TestStreamConsumptionIsCoalesced(t *testing.T) {
	a, admin := testApp(t)
	route, _ := configured(t, a, admin, "http://127.0.0.1:8096")
	w := &meterWriter{ResponseWriter: httptest.NewRecorder(), app: a, route: route, usage: &routeRuntime{}, ctx: context.Background(), stream: true}
	for i := 0; i < 8; i++ {
		if _, err := w.Write(bytes.Repeat([]byte("x"), 64<<10)); err != nil {
			t.Fatal(err)
		}
	}
	var pending int64
	a.Store.DB.QueryRow("SELECT pending_bytes FROM stream_reservations WHERE route_id=?", route.ID).Scan(&pending)
	// The first write is persisted immediately; the rest wait for 1 MiB or 1 second.
	if pending != 10<<20-64<<10 {
		t.Fatalf("pending bytes = %d", pending)
	}
	w.finishRouteCredit()
	traffic, err := a.Store.Traffic()
	if err != nil || len(traffic) != 1 || traffic[0].Egress != 512<<10 {
		t.Fatalf("traffic after finish: %+v %v", traffic, err)
	}
}

func TestAccountLocksAreIndependent(t *testing.T) {
	a, _ := testApp(t)
	release := a.lockAccount("a")
	other := make(chan struct{})
	go func() { a.lockAccount("b")(); close(other) }()
	select {
	case <-other:
	case <-time.After(time.Second):
		t.Fatal("a second account waited for the first")
	}
	same := make(chan struct{})
	go func() { a.lockAccount("a")(); close(same) }()
	select {
	case <-same:
		t.Fatal("one account ran twice at once")
	case <-time.After(50 * time.Millisecond):
	}
	admin := make(chan struct{})
	go func() { a.accessMu.Lock(); a.accessMu.Unlock(); close(admin) }()
	select {
	case <-admin:
		t.Fatal("administrative change ran beside an account operation")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	for _, done := range []chan struct{}{same, admin} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("waiter never resumed")
		}
	}
	if len(a.accountLocks.m) != 0 {
		t.Fatal("account lock entries leaked")
	}
}

func TestWrapBatchAndLegacyWrap(t *testing.T) {
	a, _, c, prefix := subscriptionFixture(t)
	token := subscriptionLogin(t, a, prefix)
	cmd := AccessCommand{AccountID: c.ID, KeyID: c.KeyID, Token: token, Action: "wrap_batch", Targets: []string{"https://cdn.example/a.ts", "https://cdn.example/b.ts"}}
	out, err := a.accessCall(context.Background(), cmd)
	if err != nil || len(out.Targets) != 2 || out.Targets[0] == out.Targets[1] {
		t.Fatal(out, err)
	}
	cmd.Action, cmd.Target = "resolve", out.Targets[1]
	if resolved, err := a.accessCall(context.Background(), cmd); err != nil || resolved.Target != "https://cdn.example/b.ts" {
		t.Fatal(resolved, err)
	}
	cmd.Action, cmd.Targets = "wrap_batch", []string{"https://cdn.example/a.ts", "ftp://cdn.example/b.ts"}
	if _, err = a.accessCall(context.Background(), cmd); err == nil {
		t.Fatal("invalid target accepted")
	}
	cmd.Targets = make([]string, maxWrapBatch+1)
	if _, err = a.accessCall(context.Background(), cmd); err == nil {
		t.Fatal("oversized batch accepted")
	}
	cmd.Action, cmd.Target, cmd.Targets = "wrap", "https://cdn.example/c.ts", nil
	single, err := a.accessCall(context.Background(), cmd)
	if err != nil || single.Target == "" {
		t.Fatal(single, err)
	}
}

func TestPlaylistIsWrappedWithOneAgentCall(t *testing.T) {
	a, _, c, prefix := subscriptionFixture(t)
	token := subscriptionLogin(t, a, prefix)
	routes, _ := list[Route](a.Store, "route")
	route := routes[0]
	route.RelayID = "auto"
	up, _ := get[Upstream](a.Store, "upstream", route.UpstreamID)
	const segments = 60
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n")
		for i := 0; i < segments; i++ {
			fmt.Fprintf(&b, "#EXTINF:2.0,\nseg%d.ts\n", i)
		}
		io.WriteString(w, b.String())
	}))
	defer source.Close()
	up.BaseURL = source.URL
	if err := a.Store.put("upstream", up.ID, up, "test"); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.put("route", route.ID, route, "test"); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	counts := map[string]int{}
	rejectBatch := false
	a.AgentMode = true
	a.AccessRPC = func(ctx context.Context, command AccessCommand) (AccessResult, error) {
		mu.Lock()
		counts[command.Action]++
		reject := rejectBatch && command.Action == "wrap_batch"
		mu.Unlock()
		if reject {
			return AccessResult{}, errors.New("invalid subscription action")
		}
		return a.accessCommand(ctx, "test-node", command)
	}
	fetch := func() string {
		w := call(t, a, nil, "GET", prefix+"/Videos/1/master.m3u8?api_key="+token, nil)
		if w.Code != 200 || strings.Contains(w.Body.String(), "seg0.ts") || strings.Count(w.Body.String(), "/__access_media/") != segments+1 {
			t.Fatal(w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	fetch()
	mu.Lock()
	if counts["wrap_batch"] != 1 || counts["wrap"] != 0 {
		t.Fatalf("rpc counts = %v", counts)
	}
	rejectBatch = true
	counts = map[string]int{}
	a.wrapBatchRetry.Store(0)
	mu.Unlock()
	// An older control rejects the new action: fall back to single wraps once, then stop asking.
	fetch()
	fetch()
	mu.Lock()
	defer mu.Unlock()
	if counts["wrap_batch"] != 1 || counts["wrap"] != 2*(segments+1) {
		t.Fatalf("fallback rpc counts = %v", counts)
	}
	_ = c
}

func TestFinishReleasesUnusedPaceSlot(t *testing.T) {
	a, _, c, prefix := subscriptionFixture(t)
	token := subscriptionLogin(t, a, prefix)
	c, _ = get[AccessAccount](a.Store, "access_account", c.ID)
	c.Plan.RateBPS, c.Plan.TrafficBytes, c.Plan.MaxConcurrent = 10000, 0, 3
	if err := a.Store.put("access_account", c.ID, c, "test"); err != nil {
		t.Fatal(err)
	}
	begin := func() AccessCommand {
		cmd := AccessCommand{AccountID: c.ID, KeyID: c.KeyID, Token: token, Action: "begin", Stream: true, Paced: true}
		out, err := a.accessCall(context.Background(), cmd)
		if err != nil {
			t.Fatal(err)
		}
		cmd.Lease, cmd.Action, cmd.Bytes = out.Lease, "pace", accessPaceBatch(c.Plan.RateBPS)
		return cmd
	}
	first := begin()
	if out, err := a.accessCall(context.Background(), first); err != nil || out.WaitNS != 0 {
		t.Fatal(out, err)
	}
	// A report shorter than the booking means another stream booked after it.
	first.Action, first.PaceUnusedNS = "finish", int64(50*time.Millisecond)
	if _, err := a.accessCall(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := begin()
	if out, err := a.accessCall(context.Background(), second); err != nil || out.WaitNS <= 0 {
		t.Fatal("slot of another stream released", out, err)
	}
	// The booking is exactly this request's own unused tail: return it.
	second.Action, second.PaceUnusedNS = "finish", int64(time.Second)
	if _, err := a.accessCall(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	third := begin()
	if out, err := a.accessCall(context.Background(), third); err != nil || out.WaitNS != 0 {
		t.Fatal("unused tail kept the account waiting", out, err)
	}
}
