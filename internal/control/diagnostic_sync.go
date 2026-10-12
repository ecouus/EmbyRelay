package control

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Diagnostic carries metadata only, never request bodies or credential values.
type Diagnostic struct {
	ID        string `json:"id"`
	Status    int    `json:"status"`
	Reason    string `json:"reason"`
	CreatedAt int64  `json:"created_at"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Source    string `json:"source"`
	Details   string `json:"details"`
}
type DiagnosticBatch struct {
	Records []Diagnostic `json:"records"`
}
type DiagnosticAck struct {
	IDs []string `json:"ids"`
}

func (s *Store) RecordDiagnostic(d Diagnostic) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO request_diagnostics(id,status,reason,created_at,method,path,source,details) VALUES(?,?,?,?,?,?,?,?)", d.ID, d.Status, d.Reason, d.CreatedAt, d.Method, d.Path, d.Source, d.Details); err != nil {
		return err
	}
	// Bound each node independently so a noisy node cannot evict other nodes.
	if _, err = tx.Exec("DELETE FROM request_diagnostics WHERE node_id='' AND id NOT IN (SELECT id FROM request_diagnostics WHERE node_id='' ORDER BY created_at DESC,id DESC LIMIT 1000)"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PendingDiagnostics() ([]Diagnostic, error) {
	rows, err := s.DB.Query("SELECT id,status,reason,created_at,method,path,source,details FROM request_diagnostics WHERE node_id='' AND uploaded=0 ORDER BY created_at,id LIMIT 100")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Diagnostic{}
	for rows.Next() {
		var d Diagnostic
		if err = rows.Scan(&d.ID, &d.Status, &d.Reason, &d.CreatedAt, &d.Method, &d.Path, &d.Source, &d.Details); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
func (s *Store) AcknowledgeDiagnostics(ids []string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err = tx.Exec("UPDATE request_diagnostics SET uploaded=1 WHERE id=? AND node_id=''", id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func cleanDiagnosticPath(path string) string {
	path = strings.SplitN(path, "?", 2)[0]
	path = strings.SplitN(path, "#", 2)[0]
	return accessPath.ReplaceAllString(path, `${1}***`)
}
func diagnosticOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
func cleanRemoteDiagnostic(d Diagnostic) (Diagnostic, error) {
	if len(d.ID) == 0 || len(d.ID) > 64 || strings.ContainsAny(d.ID, "/:\r\n") || d.Status < 400 || d.Status > 599 || d.CreatedAt <= 0 || d.CreatedAt > time.Now().Add(5*time.Minute).Unix() || len(d.Method) > 16 || len(d.Path) > 4096 || len(d.Reason) > 1024 || len(d.Details) > 16384 {
		return d, fmt.Errorf("invalid diagnostic record")
	}
	if d.Source != "local" && d.Source != "upstream" && d.Source != "agent" {
		return d, fmt.Errorf("invalid diagnostic source")
	}
	var detail diagnosticDetail
	if err := json.Unmarshal([]byte(d.Details), &detail); err != nil {
		return d, fmt.Errorf("invalid diagnostic details")
	}
	d.Path = cleanDiagnosticPath(d.Path)
	detail.UpstreamPath = cleanDiagnosticPath(detail.UpstreamPath)
	detail.Target = diagnosticOrigin(detail.Target)
	if detail.Auth != nil {
		cleanFields := func(values []string) []string {
			result := []string{}
			for _, v := range values {
				switch v {
				case "Authorization", "X-Emby-Authorization", "X-MediaBrowser-Authorization", "X-Emby-Token", "X-MediaBrowser-Token", "Cookie", "query:api_key", "query:X-Emby-Token", "query:access_token", "api_key", "access_token":
					result = append(result, v)
				}
			}
			return result
		}
		detail.Auth.Inbound = cleanFields(detail.Auth.Inbound)
		detail.Auth.Outbound = cleanFields(detail.Auth.Outbound)
		for i, v := range detail.Auth.Redirects {
			detail.Auth.Redirects[i] = diagnosticOrigin(v)
		}
	}
	// Typed serialization discards unknown fields rather than forwarding raw JSON.
	d.Details = string(Encode(detail))
	return d, nil
}
func (a *App) receiveDiagnostics(node Node, batch DiagnosticBatch) (DiagnosticAck, error) {
	if len(batch.Records) > 100 {
		return DiagnosticAck{}, fmt.Errorf("too many diagnostic records")
	}
	cleaned := make([]Diagnostic, 0, len(batch.Records))
	for _, d := range batch.Records {
		v, err := cleanRemoteDiagnostic(d)
		if err != nil {
			return DiagnosticAck{}, err
		}
		cleaned = append(cleaned, v)
	}
	tx, err := a.Store.DB.Begin()
	if err != nil {
		return DiagnosticAck{}, err
	}
	defer tx.Rollback()
	ack := DiagnosticAck{IDs: []string{}}
	for _, d := range cleaned {
		// Bind provenance to the authenticated signer; retries are idempotent.
		if _, err = tx.Exec("INSERT INTO request_diagnostics(id,status,reason,created_at,method,path,source,details,node_id,node_name,uploaded) VALUES(?,?,?,?,?,?,?,?,?,?,1) ON CONFLICT(id) DO NOTHING", node.ID+":"+d.ID, d.Status, d.Reason, d.CreatedAt, d.Method, d.Path, d.Source, d.Details, node.ID, node.Name); err != nil {
			return DiagnosticAck{}, err
		}
		ack.IDs = append(ack.IDs, d.ID)
	}
	if _, err = tx.Exec("DELETE FROM request_diagnostics WHERE node_id=? AND id NOT IN (SELECT id FROM request_diagnostics WHERE node_id=? ORDER BY created_at DESC,id DESC LIMIT 1000)", node.ID, node.ID); err != nil {
		return DiagnosticAck{}, err
	}
	return ack, tx.Commit()
}
