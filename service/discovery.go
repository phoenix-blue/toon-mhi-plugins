package main

import (
	"context"
	"encoding/json"
	"github.com/grandcat/zeroconf"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

func discoveryDevice(e *zeroconf.ServiceEntry, driver string) (Device, bool) {
	if len(e.AddrIPv4) == 0 {
		return Device{}, false
	}
	d := Device{Driver: driver, Name: e.Instance, Host: net.JoinHostPort(e.AddrIPv4[0].String(), "51443")}
	if driver == "esphome" {
		if !strings.Contains(strings.ToLower(e.Instance+" "+e.HostName), "mhi") {
			return Device{}, false
		}
		d.Host = net.JoinHostPort(e.AddrIPv4[0].String(), "6053")
		for _, t := range e.Text {
			if strings.HasPrefix(t, "mac=") {
				d.ID = strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(t, "mac="), ":", ""))
			}
		}
	} else {
		info, err := request(d, "getDeviceInfo", map[string]interface{}{})
		if err != nil {
			return Device{}, false
		}
		d.ID = strings.ToLower(fmtString(info["airconId"]))
	}
	if len(d.Name) > 40 {
		d.Name = d.Name[:40]
	}
	return d, regexp.MustCompile(`^[a-f0-9]{12}$`).MatchString(d.ID)
}
func fmtString(v interface{}) string { s, _ := v.(string); return s }
func discoverHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || r.Header.Get("X-Toon-MHI") != "1" {
		http.Error(w, "method", 405)
		return
	}
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		http.Error(w, "netwerk zoeken niet beschikbaar", 503)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	esp := make(chan *zeroconf.ServiceEntry, 32)
	wf := make(chan *zeroconf.ServiceEntry, 32)
	if err = resolver.Browse(ctx, "_esphomelib._tcp", "local.", esp); err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	if err = resolver.Browse(ctx, "_beaver._tcp", "local.", wf); err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	found := []Device{}
	seen := map[string]bool{}
	add := func(e *zeroconf.ServiceEntry, driver string) {
		if e == nil {
			return
		}
		if d, ok := discoveryDevice(e, driver); ok && !seen[d.ID] {
			found = append(found, d)
			seen[d.ID] = true
		}
	}
	done := false
	for !done {
		select {
		case e, ok := <-esp:
			if !ok {
				esp = nil
			} else {
				add(e, "esphome")
			}
		case e, ok := <-wf:
			if !ok {
				wf = nil
			} else {
				add(e, "wfrac")
			}
		case <-ctx.Done():
			done = true
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"devices": found})
}
