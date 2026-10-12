package control

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

const (
	accessUsageRetention  = 90 * 24 * time.Hour
	accessSettlementGrace = 24 * time.Hour
)

// cleanupAccessUsage retires obsolete cycles before deleting them. A cycle must
// remain unused for a full retention period; its start date alone is not enough
// to establish that it is safe to delete (notably for total-traffic plans).
func (s *Store) cleanupAccessUsage(now time.Time) error {
	rows, err := s.DB.Query(`SELECT u.account_id,u.cycle,u.retired_at,o.payload
		FROM access_usage u LEFT JOIN objects o ON o.kind='access_account' AND o.id=u.account_id`)
	if err != nil {
		return err
	}
	candidates := map[string]bool{}
	cycles := map[string]string{}
	invalid := map[string]bool{}
	for rows.Next() {
		var id, cycle string
		var retired int64
		var raw sql.NullString
		if err = rows.Scan(&id, &cycle, &retired, &raw); err != nil {
			break
		}
		if !raw.Valid {
			candidates[id] = true
			continue
		}
		current, ok := cycles[id]
		if !ok {
			if invalid[id] {
				continue
			}
			var account AccessAccount
			if json.Unmarshal([]byte(raw.String), &account) != nil {
				invalid[id] = true
				continue
			}
			current, _ = accountCycle(account, now)
			cycles[id] = current
		}
		if cycle != current || retired > 0 {
			candidates[id] = true
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	// Release the scan before opening write transactions: SQLite has one
	// connection. Re-read each account under the same row lock used by billing,
	// rather than applying a possibly stale cycle from the initial scan.
	for id := range candidates {
		if err = s.cleanupAccountUsage(id, now); err != nil {
			return err
		}
	}
	if len(invalid) > 0 {
		return fmt.Errorf("preserved usage for %d invalid subscription accounts", len(invalid))
	}
	return nil
}

func (s *Store) cleanupAccountUsage(id string, now time.Time) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	account, err := loadAccount(tx, id)
	settlementCutoff := now.Add(-accessSettlementGrace).Unix()
	if err == sql.ErrNoRows {
		_, err = tx.Exec(`DELETE FROM access_usage WHERE account_id=?
			AND NOT EXISTS (SELECT 1 FROM objects WHERE kind='access_account' AND id=?)
			AND NOT EXISTS (SELECT 1 FROM access_leases WHERE account_id=? AND expires_at>?)`, id, id, id, settlementCutoff)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	current, _ := accountCycle(account, now)
	// Empty charged_cycle is a legacy or not-yet-charged lease. Conservatively
	// protect every cycle for that account until the lease is settled or stale.
	protected := `EXISTS (SELECT 1 FROM access_leases l WHERE l.account_id=access_usage.account_id
		AND l.expires_at>? AND (l.charged_cycle='' OR l.charged_cycle=access_usage.cycle))`
	if _, err = tx.Exec(`UPDATE access_usage SET retired_at=0 WHERE account_id=? AND retired_at>0
		AND (cycle=? OR `+protected+`)`, id, current, settlementCutoff); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE access_usage SET retired_at=? WHERE account_id=? AND cycle<>?
		AND retired_at=0 AND NOT `+protected, now.Unix(), id, current, settlementCutoff); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM access_usage WHERE account_id=? AND cycle<>?
		AND retired_at>0 AND retired_at<? AND NOT `+protected, id, current, now.Add(-accessUsageRetention).Unix(), settlementCutoff); err != nil {
		return err
	}
	return tx.Commit()
}
