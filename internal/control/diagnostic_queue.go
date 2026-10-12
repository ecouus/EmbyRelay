package control

import (
	"log/slog"
	"time"
)

// StartDiagnostics keeps request handlers off the database write path.
func (a *App) StartDiagnostics() {
	a.diagnosticMu.Lock()
	defer a.diagnosticMu.Unlock()
	if a.diagnosticQueue != nil {
		return
	}
	a.diagnosticQueue = make(chan Diagnostic, 512)
	a.diagnosticDone = make(chan struct{})
	go func(queue <-chan Diagnostic, done chan<- struct{}) {
		defer close(done)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		batch := make([]Diagnostic, 0, 64)
		flush := func() {
			if len(batch) == 0 {
				return
			}
			if err := a.Store.RecordDiagnostics(batch); err != nil {
				slog.Warn("diagnostic batch write failed", "error", err)
			}
			batch = batch[:0]
		}
		for {
			select {
			case d, ok := <-queue:
				if !ok {
					flush()
					return
				}
				batch = append(batch, d)
				if len(batch) >= 64 {
					flush()
				}
			case <-ticker.C:
				flush()
			}
		}
	}(a.diagnosticQueue, a.diagnosticDone)
}

func (a *App) StopDiagnostics() {
	a.diagnosticMu.Lock()
	done := a.diagnosticDone
	if a.diagnosticQueue != nil {
		close(a.diagnosticQueue)
		a.diagnosticQueue = nil
	}
	a.diagnosticMu.Unlock()
	if done != nil {
		<-done
	}
}

func (a *App) recordDiagnostic(d Diagnostic) {
	a.diagnosticMu.Lock()
	defer a.diagnosticMu.Unlock()
	if a.diagnosticQueue == nil {
		_ = a.Store.RecordDiagnostic(d)
		return
	}
	select {
	case a.diagnosticQueue <- d:
	default:
		slog.Warn("diagnostic queue full; dropping record")
	}
}
