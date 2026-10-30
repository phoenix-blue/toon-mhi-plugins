package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
)

func keyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || r.Header.Get("X-Toon-MHI") != "1" {
		http.Error(w, "method", 405)
		return
	}
	var v struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&v) != nil || CheckKey(v.Key) != nil {
		http.Error(w, "Ongeldige API-sleutel: verwacht een base64-sleutel van 32 bytes", 400)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, d := range config.Devices {
		if d.ID == v.ID && d.Driver == "esphome" {
			found = true
		}
	}
	if !found {
		http.Error(w, "ESPHome airco niet gevonden", 404)
		return
	}
	keys := map[string]string{}
	if b, e := os.ReadFile(espKeysPath); e == nil {
		if json.Unmarshal(b, &keys) != nil {
			http.Error(w, "Sleutelbestand onleesbaar", 500)
			return
		}
	} else if !os.IsNotExist(e) {
		http.Error(w, "Sleutelbestand onleesbaar", 500)
		return
	}
	keys[v.ID] = strings.TrimSpace(v.Key)
	b, _ := json.Marshal(keys)
	if os.WriteFile(espKeysPath+".tmp", b, 0600) != nil || os.Rename(espKeysPath+".tmp", espKeysPath) != nil {
		http.Error(w, "Opslaan sleutel mislukt", 500)
		return
	}
	refresh()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "message": "API-sleutel opgeslagen"})
}

func resolveHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || r.Header.Get("X-Toon-MHI") != "1" {
		http.Error(w, "method", 405)
		return
	}
	var d Device
	if json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&d) != nil {
		http.Error(w, "Ongeldig apparaat", 400)
		return
	}
	host, port, e := net.SplitHostPort(d.Host)
	if e != nil || net.ParseIP(host) == nil || port != "51443" || len(d.Name) == 0 || len(d.Name) > 40 {
		http.Error(w, "Controleer naam en IP-adres", 400)
		return
	}
	d.Driver = "wfrac"
	info, e := request(d, "getDeviceInfo", map[string]interface{}{})
	if e != nil {
		http.Error(w, "WF-RAC niet bereikbaar op dit IP-adres", 502)
		return
	}
	d.ID = strings.ToLower(fmtString(info["airconId"]))
	if !regexp.MustCompile(`^[a-f0-9]{12}$`).MatchString(d.ID) {
		http.Error(w, "Geen geldig WF-RAC apparaat-ID ontvangen", 502)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(d)
}
