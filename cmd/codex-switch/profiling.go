package main

import (
	"net/http"
	"net/http/pprof"
	"strings"

	"github.com/joshp123/codex-switch/internal/clientcapability"
)

// Profiling uses the same private capability as the deployment and menu endpoints.
func profileOrRelay(capability string, next http.Handler) http.Handler {
	prefix := clientcapability.PathPrefix(capability)
	profiles := http.NewServeMux()
	profiles.HandleFunc("/debug/pprof/", pprof.Index)
	profiles.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	profiles.HandleFunc("/debug/pprof/profile", pprof.Profile)
	profiles.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	profiles.HandleFunc("/debug/pprof/trace", pprof.Trace)
	privateProfiles := http.StripPrefix(prefix, profiles)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == prefix+"/debug/pprof" {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
		if strings.HasPrefix(r.URL.Path, prefix+"/debug/pprof/") {
			privateProfiles.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
