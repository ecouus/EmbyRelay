package control

import (
	"embyrelay/internal/database"
	"encoding/json"
	"fmt"
	"time"
)

type AccessPlan struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	DurationDays   int    `json:"duration_days"`
	DurationMonths int    `json:"duration_months"`
	TrafficBytes   int64  `json:"traffic_bytes"`
	TrafficPeriod  string `json:"traffic_period"`
	PeriodDays     int    `json:"period_days"`
	RateBPS        int64  `json:"rate_bps"`
	MaxConcurrent  int    `json:"max_concurrent"`
}
type AccessAccount struct {
	ID           string     `json:"id"`
	RouteID      string     `json:"route_id"`
	KeyID        string     `json:"key_id"`
	Username     string     `json:"username"`
	PasswordHash string     `json:"password_hash,omitempty"`
	Credentials  string     `json:"credentials,omitempty"`
	PlanID       string     `json:"plan_id"`
	Plan         AccessPlan `json:"plan"`
	StartMode    string     `json:"start_mode"`
	StartedAt    int64      `json:"started_at"`
	ExpiresAt    int64      `json:"expires_at"`
	ResetAt      int64      `json:"reset_at"`
	Enabled      bool       `json:"enabled"`
}

func validPlan(p AccessPlan) error {
	if !safeName(p.Name) || p.DurationDays < 0 || p.DurationDays > 36500 || p.DurationMonths < 0 || p.DurationMonths > 1200 || p.DurationDays > 0 && p.DurationMonths > 0 || p.TrafficBytes < 0 || p.TrafficBytes > 1<<60 || p.RateBPS < 0 || p.RateBPS > 1<<40 || p.MaxConcurrent < 1 || p.MaxConcurrent > 1000 {
		return fmt.Errorf("invalid plan values")
	}
	if p.TrafficPeriod != "total" && p.TrafficPeriod != "days" && p.TrafficPeriod != "month" {
		return fmt.Errorf("invalid traffic period")
	}
	if p.TrafficPeriod == "days" && (p.PeriodDays < 1 || p.PeriodDays > 36500) {
		return fmt.Errorf("period_days must be 1–36500")
	}
	return nil
}

// Month addition clamps to the final day rather than overflowing into another month.
func addMonths(t time.Time, n int) time.Time {
	first := time.Date(t.Year(), t.Month()+time.Month(n), 1, t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
	last := first.AddDate(0, 1, -1).Day()
	day := t.Day()
	if day > last {
		day = last
	}
	return first.AddDate(0, 0, day-1)
}
func planExpiry(start int64, p AccessPlan) int64 {
	t := time.Unix(start, 0).UTC()
	if p.DurationMonths > 0 {
		return addMonths(t, p.DurationMonths).Unix()
	}
	if p.DurationDays > 0 {
		return t.AddDate(0, 0, p.DurationDays).Unix()
	}
	return 0
}
func accountCycle(c AccessAccount, now time.Time) (string, int64) {
	anchor := c.StartedAt
	if anchor == 0 {
		return "pending", 0
	}
	periodStart := anchor
	next := int64(0)
	switch c.Plan.TrafficPeriod {
	case "days":
		interval := int64(c.Plan.PeriodDays) * 86400
		if interval > 0 {
			periodStart = anchor + max(int64(0), (now.Unix()-anchor)/interval)*interval
			next = periodStart + interval
		}
	case "month":
		month := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
		periodStart = max(anchor, month.Unix())
		next = month.AddDate(0, 1, 0).Unix()
	}
	return fmt.Sprintf("%d:%d", periodStart, c.ResetAt), next
}
func accountActive(c AccessAccount, now time.Time) error {
	if !c.Enabled {
		return fmt.Errorf("访问账号已停用")
	}
	if c.ExpiresAt > 0 && now.Unix() >= c.ExpiresAt {
		return fmt.Errorf("订阅已到期")
	}
	return nil
}
func loadAccount(tx *database.Tx, id string) (AccessAccount, error) {
	var c AccessAccount
	var raw string
	err := tx.QueryRow("SELECT payload FROM objects WHERE kind='access_account' AND id=?"+tx.ForUpdate(), id).Scan(&raw)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &c)
	}
	return c, err
}
func saveAccount(tx *database.Tx, c AccessAccount) error {
	_, err := tx.Exec("INSERT INTO objects VALUES('access_account',?,?,?) ON CONFLICT(kind,id) DO UPDATE SET payload=excluded.payload,updated_at=excluded.updated_at", c.ID, string(Encode(c)), time.Now().Unix())
	return err
}
