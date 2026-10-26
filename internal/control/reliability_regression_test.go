package control

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"embyrelay/internal/proxy"
	"embyrelay/migrations"
)

func TestUntrustedPlaybackDocumentsAreSandboxed(t *testing.T) {
	for _, ct := range []string{"text/html", "image/svg+xml"} {
		t.Run(ct, func(t *testing.T) {
			a, _, c, prefix := subscriptionFixture(t)
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", ct)
				w.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-same-origin")
				w.Write([]byte("<script>window.untrusted=1</script>"))
			}))
			defer source.Close()
			route, err := get[Route](a.Store, "route", c.RouteID)
			if err != nil {
				t.Fatal(err)
			}
			up, err := get[Upstream](a.Store, "upstream", route.UpstreamID)
			if err != nil {
				t.Fatal(err)
			}
			up.BaseURL = source.URL
			up.OwnerAccountID = c.ID
			route.OwnerAccountID = c.ID
			if err = a.Store.put("upstream", up.ID, up, "test"); err != nil {
				t.Fatal(err)
			}
			if err = a.Store.put("route", route.ID, route, "test"); err != nil {
				t.Fatal(err)
			}
			if err = a.Reload(); err != nil {
				t.Fatal(err)
			}
			for _, agent := range []bool{false, true} {
				a.AgentMode = agent
				for _, path := range []string{"/System/Info/Public", "/emby/System/Info/Public"} {
					r := call(t, a, nil, "GET", prefix+path, nil)
					if r.Code != 200 || !strings.Contains(r.Body.String(), "<script>") {
						t.Fatalf("response %d %s", r.Code, r.Body.String())
					}
					for _, csp := range r.Header().Values("Content-Security-Policy") {
						if csp != proxy.PlaybackCSP {
							t.Fatalf("upstream CSP not replaced: %q", csp)
						}
					}
					if r.Header().Get("Content-Security-Policy") != proxy.PlaybackCSP {
						t.Fatal(r.Header())
					}
				}
			}
		})
	}
}

func TestHistoricalTrafficDoesNotBlockHeartbeat(t *testing.T) {
	a, admin := testApp(t)
	route, _ := configured(t, a, admin, "http://127.0.0.1:8096")
	node := Node{ID: "history-node", Enabled: true}
	route.RelayID = node.ID
	if err := a.Store.put("node", node.ID, node, "test"); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.put("route", route.ID, route, "test"); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	report := Heartbeat{TrafficAccounting: "actual-v1", SubscriptionSupport: true, DeviceIdentitySupport: true, Version: a.state.Load().snapshot.Version}
	for _, days := range []int{0, 33, 364, 367} {
		report.Traffic = append(report.Traffic, Traffic{Date: time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Format("2006-01-02"), RouteID: route.ID, Egress: 100})
	}
	for i := 0; i < 2; i++ {
		if err := a.report(node, report); err != nil {
			t.Fatal(err)
		}
		node, _ = get[Node](a.Store, "node", node.ID)
	}
	traffic, err := a.Store.Traffic()
	if err != nil {
		t.Fatal(err)
	}
	if len(traffic) != 3 {
		t.Fatal(traffic)
	}
	for _, row := range traffic {
		if row.Egress != 100 {
			t.Fatal("duplicate heartbeat counted twice", row)
		}
	}
	if node.LastSeen == 0 || node.ConfigVersion != report.Version {
		t.Fatal(node)
	}
}

func TestInterruptedCreditRecoveryIsIdempotent(t *testing.T) {
	s, err := testStore(t)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	day, err := s.reserveStreamTraffic("route", 10<<20, 0, 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.consumeStreamTraffic("route", day, 100<<10); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("INSERT INTO stream_sessions(id,route_id,started_at,status) VALUES('interrupted','route',?,'active')", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = s.recoverStreamReservations(); err != nil {
			t.Fatal(err)
		}
		traffic, err := s.Traffic()
		if err != nil || len(traffic) != 1 || traffic[0].Egress != 100<<10 {
			t.Fatal(traffic, err)
		}
	}
	if err = s.Charge("route", 10<<20, 0, (10<<20)-(100<<10)); err != nil {
		t.Fatal("unused quota was not released", err)
	}
	var status string
	s.DB.QueryRow("SELECT status FROM stream_sessions WHERE id='interrupted'").Scan(&status)
	if status != "interrupted" {
		t.Fatal(status)
	}
}

func TestCreditSurvivesProcessKill(t *testing.T) {
	if path := os.Getenv("EMBYRELAY_CRASH_TEST_DB"); path != "" {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		day, err := s.reserveStreamTraffic("route", 0, 0, 10<<20)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.consumeStreamTraffic("route", day, 100<<10); err != nil {
			t.Fatal(err)
		}
		fmt.Println("credit-ready")
		select {}
	}
	path := filepath.Join(t.TempDir(), "crash.db")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCreditSurvivesProcessKill$")
	cmd.Env = append(os.Environ(), "EMBYRELAY_CRASH_TEST_DB="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "credit-ready" {
		t.Fatalf("child: %s %v %s", line, err, stderr.String())
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	traffic, err := s.Traffic()
	if err != nil || len(traffic) != 1 || traffic[0].Egress != 100<<10 {
		t.Fatal(traffic, err)
	}
}

func TestGlobalLoginNames(t *testing.T) {
	a, admin, c, _ := subscriptionFixture(t)
	for _, name := range []string{"admin", "ADMIN"} {
		r := call(t, a, admin, "POST", "/api/v1/access-accounts", map[string]any{"route_id": c.RouteID, "plan_id": c.PlanID, "username": name, "password": "viewer-password", "start_mode": "first_login"})
		if r.Code < 400 {
			t.Fatalf("duplicate accepted: %d %s", r.Code, r.Body.String())
		}
	}
	r := call(t, a, admin, "POST", "/api/v1/users", map[string]string{"username": "VIEWER", "password": "operator-password", "role": "operator"})
	if r.Code < 400 {
		t.Fatal("subscriber collision accepted")
	}
	for _, name := range []string{"viewer", "VIEWER"} {
		r = call(t, a, nil, "POST", "/api/v1/auth/login", map[string]string{"username": name, "password": "viewer-password"})
		if r.Code != 200 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
}

func TestPacedSubscriptionDoesNotQueueUnsentBytes(t *testing.T) {
	for _, rate := range []int64{125000, 12500} {
		t.Run(fmt.Sprint(rate), func(t *testing.T) {
			a, _, c, prefix := subscriptionFixture(t)
			token := subscriptionLogin(t, a, prefix)
			c, _ = get[AccessAccount](a.Store, "access_account", c.ID)
			c.Plan.RateBPS = rate
			c.Plan.TrafficBytes = 0
			if err := a.Store.put("access_account", c.ID, c, "test"); err != nil {
				t.Fatal(err)
			}
			cmd := AccessCommand{AccountID: c.ID, KeyID: c.KeyID, Token: token, Action: "begin", Stream: true, Paced: true}
			out, err := a.accessCall(context.Background(), cmd)
			if err != nil {
				t.Fatal(err)
			}
			if out.RateBPS != rate {
				t.Fatal(out)
			}
			cmd.Lease = out.Lease
			cmd.Action = "charge"
			cmd.Bytes = streamAccessChunk
			out, err = a.accessCall(context.Background(), cmd)
			if err != nil || out.WaitNS != 0 {
				t.Fatal("quota grant delayed output", out, err)
			}
			cmd.Action = "pace"
			cmd.Bytes = int64(accessPaceChunk(rate))
			first, err := a.accessCall(context.Background(), cmd)
			if err != nil || first.WaitNS != 0 {
				t.Fatal("first small output blocked", first, err)
			}
			var before, after int64
			a.Store.DB.QueryRow("SELECT next_ns FROM access_rate WHERE account_id=?", c.ID).Scan(&before)
			// Multiple waiting requests must share the same slot rather than append debt.
			for i := 0; i < 3; i++ {
				out, err = a.accessCall(context.Background(), cmd)
				if err != nil || out.WaitNS <= 0 || out.WaitNS > int64(100*time.Millisecond) {
					t.Fatal(out, err)
				}
			}
			a.Store.DB.QueryRow("SELECT next_ns FROM access_rate WHERE account_id=?", c.ID).Scan(&after)
			if before != after {
				t.Fatal("waiting queued unsent rate debt")
			}
			cmd.Action = "finish"
			cmd.RefundBytes = streamAccessChunk
			if _, err = a.accessCall(context.Background(), cmd); err != nil {
				t.Fatal(err)
			}
			var used int64
			a.Store.DB.QueryRow("SELECT bytes FROM access_usage WHERE account_id=?", c.ID).Scan(&used)
			if used != 0 {
				t.Fatal("cancelled grant not refunded", used)
			}
		})
	}
}

func TestMediaPacingAndCancelledRetry(t *testing.T) {
	for _, rate := range []int64{125000, 12500} {
		t.Run(fmt.Sprint(rate), func(t *testing.T) {
			a, _, c, prefix := subscriptionFixture(t)
			token := subscriptionLogin(t, a, prefix)
			c, _ = get[AccessAccount](a.Store, "access_account", c.ID)
			c.Plan.RateBPS = rate
			c.Plan.TrafficBytes = 0
			c.Plan.MaxConcurrent = 2
			if err := a.Store.put("access_account", c.ID, c, "test"); err != nil {
				t.Fatal(err)
			}
			route, _ := get[Route](a.Store, "route", c.RouteID)
			up, _ := get[Upstream](a.Store, "upstream", route.UpstreamID)
			data := bytes.Repeat([]byte("m"), int(rate/2))
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "video/mp2t")
				w.Write(data)
			}))
			defer source.Close()
			up.BaseURL = source.URL
			if err := a.Store.put("upstream", up.ID, up, "test"); err != nil {
				t.Fatal(err)
			}
			if err := a.Reload(); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(a)
			defer server.Close()
			client := &http.Client{Timeout: 3 * time.Second}
			address := server.URL + prefix + "/Videos/1/hls/segment.ts?api_key=" + token
			started := time.Now()
			resp, err := client.Get(address)
			if err != nil {
				t.Fatal(err)
			}
			first := make([]byte, 1)
			if _, err = resp.Body.Read(first); err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
				t.Fatalf("short segment start delayed: %v", elapsed)
			}
			rest, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if len(rest)+1 != len(data) {
				t.Fatal("media truncated")
			}
			if elapsed := time.Since(started); elapsed < 300*time.Millisecond {
				t.Fatalf("rate allowed burst: %v", elapsed)
			}
			// Interrupt a second request while it is pacing, then retry without 10 MiB debt.
			resp, err = client.Get(address)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = resp.Body.Read(first); err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			deadline := time.Now().Add(2 * time.Second)
			for {
				var active int
				a.Store.DB.QueryRow("SELECT COUNT(*) FROM access_leases WHERE account_id=?", c.ID).Scan(&active)
				if active == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("cancelled request was not settled")
				}
				time.Sleep(10 * time.Millisecond)
			}
			started = time.Now()
			resp, err = client.Get(address)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = resp.Body.Read(first); err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
				t.Fatalf("retry inherited unused rate debt: %v", elapsed)
			}
		})
	}
}

func TestConcurrentPacingSharesAccountBudget(t *testing.T) {
	a, _, c, prefix := subscriptionFixture(t)
	token := subscriptionLogin(t, a, prefix)
	c, _ = get[AccessAccount](a.Store, "access_account", c.ID)
	c.Plan.RateBPS = 1000
	c.Plan.MaxConcurrent = 2
	if err := a.Store.put("access_account", c.ID, c, "test"); err != nil {
		t.Fatal(err)
	}
	commands := make([]AccessCommand, 2)
	for i := range commands {
		cmd := AccessCommand{AccountID: c.ID, KeyID: c.KeyID, Token: token, Action: "begin", Stream: true, Paced: true}
		out, err := a.accessCall(context.Background(), cmd)
		if err != nil {
			t.Fatal(err)
		}
		cmd.Lease = out.Lease
		cmd.Action = "pace"
		cmd.Bytes = 100
		commands[i] = cmd
	}
	start := make(chan struct{})
	results := make(chan AccessResult, 2)
	failures := make(chan error, 2)
	for _, cmd := range commands {
		go func(cmd AccessCommand) {
			<-start
			out, err := a.accessCall(context.Background(), cmd)
			results <- out
			failures <- err
		}(cmd)
	}
	close(start)
	immediate := 0
	for range commands {
		out := <-results
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
		if out.WaitNS == 0 {
			immediate++
		}
	}
	if immediate != 1 {
		t.Fatalf("independent connections bypassed shared account rate: %d permits", immediate)
	}
}

func removeLoginNamesMigration(t *testing.T, s *Store) {
	t.Helper()
	for _, table := range []string{"users", "objects"} {
		queries := []string{"DROP TRIGGER login_" + table + "_insert", "DROP TRIGGER login_" + table + "_update", "DROP TRIGGER login_" + table + "_delete"}
		if s.DB.Driver == "postgres" {
			queries = []string{"DROP TRIGGER login_" + table + "_sync ON " + table, "DROP FUNCTION login_" + table + "_sync()"}
		}
		for _, q := range queries {
			if _, err := s.DB.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.DB.Exec("DROP TABLE login_names"); err != nil {
		t.Fatal(err)
	}
}

func TestHistoricalLoginCollisionStopsMigration(t *testing.T) {
	a, _, c, _ := subscriptionFixture(t)
	removeLoginNamesMigration(t, a.Store)
	if _, err := a.Store.DB.Exec("DELETE FROM schema_migrations WHERE version>=15"); err != nil {
		t.Fatal(err)
	}
	c.Username = "ADMIN"
	if err := a.Store.put("access_account", c.ID, c, "test"); err != nil {
		t.Fatal(err)
	}
	apply := migrations.Apply
	if a.Store.DB.Driver == "postgres" {
		apply = migrations.ApplyPostgres
	}
	if err := apply(a.Store.DB.DB); err == nil || !strings.Contains(err.Error(), "conflicting login usernames") {
		t.Fatal(err)
	}
	c.Username = "viewer"
	if err := a.Store.put("access_account", c.ID, c, "test"); err != nil {
		t.Fatal(err)
	}
	if err := apply(a.Store.DB.DB); err != nil {
		t.Fatal(err)
	}
}
