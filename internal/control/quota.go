package control

import (
	"database/sql"
	"fmt"
	"time"
)

type QuotaRequest struct {
	RouteID string `json:"route_id"`
	Date    string `json:"date"`
	Target  int64  `json:"target"`
}
type QuotaGrant struct {
	Date    string `json:"date"`
	Granted int64  `json:"granted"`
}

// maxQuotaGrant covers one agent batch (32 MiB top-up) and a 10 MiB stream chunk.
const maxQuotaGrant = 32 * 1024 * 1024

// grantQuota reserves bytes before any agent sends them. Grants cannot be
// reclaimed while a disconnected node might still use them.
func (a *App) grantQuota(node Node, in QuotaRequest) (QuotaGrant, error) {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	a.Store.mu.Lock()
	defer a.Store.mu.Unlock()
	out := QuotaGrant{Date: time.Now().UTC().Format("2006-01-02")}
	if in.Date != out.Date || in.Target <= 0 {
		return out, fmt.Errorf("invalid lease date or size")
	}
	var route *Route
	for _, r := range a.state.Load().snapshot.Routes {
		if r.ID == in.RouteID && r.Enabled && (r.RelayID == node.ID || r.RelayID == "auto") {
			copy := r
			route = &copy
			break
		}
	}
	if route == nil || route.DailyQuota <= 0 {
		return out, fmt.Errorf("route has no distributed quota")
	}
	tx, e := a.Store.DB.Begin()
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	var existing int64
	e = tx.QueryRow("SELECT granted FROM quota_leases WHERE date=? AND route_id=? AND node_id=?", in.Date, in.RouteID, node.ID).Scan(&existing)
	if e != nil && e != sql.ErrNoRows {
		return out, e
	}
	if existing >= in.Target {
		out.Granted = existing
		return out, nil
	}
	var used, reserved int64
	e = tx.QueryRow("SELECT egress_bytes FROM traffic_daily WHERE date=? AND route_id=?", in.Date, in.RouteID).Scan(&used)
	if e != nil && e != sql.ErrNoRows {
		return out, e
	}
	if e = tx.QueryRow("SELECT COALESCE(SUM(MAX(0,granted-reported)),0) FROM quota_leases WHERE date=? AND route_id=?", in.Date, in.RouteID).Scan(&reserved); e != nil {
		return out, e
	}
	remaining := route.DailyQuota - used - reserved
	if remaining <= 0 {
		return out, ErrQuota
	}
	grant := in.Target - existing
	if grant > maxQuotaGrant {
		grant = maxQuotaGrant
	}
	if grant > remaining {
		grant = remaining
	}
	out.Granted = existing + grant
	if _, e = tx.Exec("INSERT INTO quota_leases(date,route_id,node_id,granted) VALUES(?,?,?,?) ON CONFLICT(date,route_id,node_id) DO UPDATE SET granted=excluded.granted", in.Date, in.RouteID, node.ID, out.Granted); e != nil {
		return out, e
	}
	return out, tx.Commit()
}
func (s *Store) AgentBudget(route, date string) (grant, used int64, err error) {
	e := s.DB.QueryRow("SELECT granted FROM quota_leases WHERE date=? AND route_id=? AND node_id=?", date, route, s.AgentID).Scan(&grant)
	if e != nil && e != sql.ErrNoRows {
		return 0, 0, e
	}
	e = s.DB.QueryRow("SELECT egress_bytes FROM traffic_daily WHERE date=? AND route_id=?", date, route).Scan(&used)
	if e != nil && e != sql.ErrNoRows {
		return 0, 0, e
	}
	return grant, used, nil
}
func (s *Store) SaveGrant(route string, g QuotaGrant) error {
	_, e := s.DB.Exec("INSERT INTO quota_leases(date,route_id,node_id,granted) VALUES(?,?,?,?) ON CONFLICT(date,route_id,node_id) DO UPDATE SET granted=MAX(quota_leases.granted,excluded.granted)", g.Date, route, s.AgentID, g.Granted)
	return e
}
