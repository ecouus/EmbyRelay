package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type embyCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (a *App) updateCredentials(u *Upstream, username, password *string, clear bool) error {
	if clear {
		u.EncryptedCredentials = ""
		return nil
	}
	if username == nil && password == nil {
		return nil
	}
	var c embyCredentials
	if u.EncryptedCredentials != "" {
		raw, err := a.unseal(u.EncryptedCredentials)
		if err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(raw), &c); err != nil {
			return err
		}
	}
	if username != nil {
		c.Username = *username
	}
	if password != nil {
		c.Password = *password
	}
	if len(c.Username) > 1024 || len(c.Password) > 4096 {
		return fmt.Errorf("credentials too long")
	}
	if c.Username == "" && c.Password == "" {
		u.EncryptedCredentials = ""
		return nil
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	u.EncryptedCredentials, err = a.seal(string(raw))
	return err
}

func (a *App) revealCredentials(w http.ResponseWriter, r *http.Request, user User, id string) {
	w.Header().Set("Cache-Control", "no-store")
	if user.Role != "admin" {
		apiError(w, 403, "administrator role required")
		return
	}
	if r.Method != "POST" {
		apiError(w, 405, "method not allowed")
		return
	}
	u, err := get[Upstream](a.Store, "upstream", id)
	if err != nil {
		apiError(w, 404, "upstream not found")
		return
	}
	var c embyCredentials
	if u.EncryptedCredentials != "" {
		raw, err := a.unseal(u.EncryptedCredentials)
		if err != nil || json.Unmarshal([]byte(raw), &c) != nil {
			apiError(w, 500, "credentials unavailable")
			return
		}
	}
	if _, err := a.Store.DB.Exec("INSERT INTO audit_logs(actor_id,action,target,created_at) VALUES(?,?,?,?)", user.ID, "reveal_emby_credentials", id, time.Now().Unix()); err != nil {
		apiError(w, 500, "audit unavailable")
		return
	}
	writeJSON(w, 200, map[string]string{"source_url": u.BaseURL, "username": c.Username, "password": c.Password})
}
