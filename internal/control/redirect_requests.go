package control

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

type redirectRequest struct {
	ID         string `json:"id"`
	UpstreamID string `json:"upstream_id"`
	Target     string `json:"target"`
	Applicant  string `json:"applicant"`
	Status     string `json:"status"`
	CreatedAt  int64  `json:"created_at"`
}

func (a *App) redirectRequests(w http.ResponseWriter, r *http.Request, u User, id string) {
	if r.Method == "GET" {
		all, e := list[redirectRequest](a.Store, "redirect_request")
		if e != nil {
			apiError(w, 500, "storage unavailable")
			return
		}
		out := []redirectRequest{}
		for _, v := range all {
			if u.Role == "admin" || v.Applicant == u.ID {
				out = append(out, v)
			}
		}
		writeJSON(w, 200, out)
		return
	}
	a.configMu.Lock()
	defer a.configMu.Unlock()
	if r.Method == "POST" && id == "" {
		var in struct {
			DiagnosticID string `json:"diagnostic_id"`
		}
		if !decode(w, r, &in) {
			return
		}
		var raw string
		if a.Store.DB.QueryRow("SELECT details FROM request_diagnostics WHERE id=?", in.DiagnosticID).Scan(&raw) != nil {
			apiError(w, 404, "diagnostic expired or not found")
			return
		}
		var d diagnosticDetail
		if json.Unmarshal([]byte(raw), &d) != nil || d.Code != "redirect_host_denied" {
			apiError(w, 400, "this diagnostic cannot request approval")
			return
		}
		if _, e := get[Upstream](a.Store, "upstream", d.UpstreamID); e != nil {
			apiError(w, 404, "upstream not found")
			return
		}
		all, e := list[redirectRequest](a.Store, "redirect_request")
		if e != nil {
			apiError(w, 500, "storage unavailable")
			return
		}
		for _, v := range all {
			if v.UpstreamID == d.UpstreamID && v.Target == d.Target && v.Applicant == u.ID && v.Status == "pending" {
				writeJSON(w, 200, v)
				return
			}
		}
		v := redirectRequest{ID: ID(), UpstreamID: d.UpstreamID, Target: d.Target, Applicant: u.ID, Status: "pending", CreatedAt: time.Now().Unix()}
		if a.Store.put("redirect_request", v.ID, v, u.ID) != nil {
			apiError(w, 500, "request save failed")
			return
		}
		writeJSON(w, 201, v)
		return
	}
	if r.Method != "PATCH" || u.Role != "admin" {
		apiError(w, 403, "administrator approval required")
		return
	}
	var in struct {
		Action string `json:"action"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Action != "approve" && in.Action != "reject" {
		apiError(w, 400, "invalid action")
		return
	}
	v, e := get[redirectRequest](a.Store, "redirect_request", id)
	if e != nil {
		apiError(w, 404, "request not found")
		return
	}
	if v.Status != "pending" {
		apiError(w, 409, "request already processed")
		return
	}
	if in.Action == "approve" {
		upstream, e := get[Upstream](a.Store, "upstream", v.UpstreamID)
		if e != nil {
			apiError(w, 404, "upstream not found")
			return
		}
		target, e := url.Parse(v.Target)
		base, be := url.Parse(upstream.BaseURL)
		if e != nil || be != nil || target.User != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") || (base.Scheme == "https" && target.Scheme != "https") {
			apiError(w, 400, "target no longer permitted")
			return
		}
		found := false
		for _, host := range upstream.RedirectHosts {
			if host == target.Host {
				found = true
			}
		}
		if !found {
			upstream.RedirectHosts = append(upstream.RedirectHosts, target.Host)
		}
		if validateUpstream(upstream) != nil {
			apiError(w, 400, "invalid upstream configuration")
			return
		}
		if a.Store.put("upstream", upstream.ID, upstream, u.ID) != nil {
			apiError(w, 500, "upstream save failed")
			return
		}
		if a.Reload() != nil {
			apiError(w, 500, "reload failed; retry approval")
			return
		}
		v.Status = "approved"
	} else {
		v.Status = "rejected"
	}
	if a.Store.put("redirect_request", v.ID, v, u.ID) != nil {
		apiError(w, 500, "request save failed")
		return
	}
	writeJSON(w, 200, v)
}
