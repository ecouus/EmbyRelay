// faultupstream serves synthetic media locally for transfer and recovery tests.
package main

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"time"
)

func main() {
	data := bytes.Repeat([]byte("EmbyRelay-test-"), 1<<20)
	mux := http.NewServeMux()
	mux.HandleFunc("/System/Info/Public", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ServerName":"EmbyRelay fault simulator","Version":"1.0"}`)
	})
	mux.HandleFunc("/Videos/demo/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", "\"fixture-v1\"")
		mode := r.URL.Query().Get("fault")
		if r.Header.Get("Range") == "" && (mode == "truncate" || mode == "stall") {
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			w.WriteHeader(200)
			w.Write(data[:len(data)/2])
			w.(http.Flusher).Flush()
			if mode == "stall" {
				select {
				case <-r.Context().Done():
				case <-time.After(45 * time.Second):
				}
			}
			return
		}
		if mode == "changed" && r.Header.Get("If-Range") != "" {
			w.Header().Set("ETag", "\"fixture-v2\"")
		}
		if mode == "loop" {
			http.Redirect(w, r, r.URL.String(), 302)
			return
		}
		http.ServeContent(w, r, "fixture.mp4", time.Time{}, bytes.NewReader(data))
	})
	log.Print("synthetic origin listening on 127.0.0.1:8096")
	log.Fatal(http.ListenAndServe("127.0.0.1:8096", mux))
}
