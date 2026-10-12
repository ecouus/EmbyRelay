package control

import "net/http"

// Legacy clients may read the retired setting, but cannot restore restrictions.
func (a *App) securitySettings(w http.ResponseWriter, r *http.Request, user User) {
	if r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]bool{"redirect_safe_mode": false})
		return
	}
	apiError(w, 410, "安全模式已移除，媒体自动中转")
}
