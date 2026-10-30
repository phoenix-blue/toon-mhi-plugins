package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Device struct {
	Driver string `json:"driver,omitempty"`
	Name   string `json:"name"`
	Host   string `json:"host"`
	ID     string `json:"id"`
}
type Config struct {
	SensorInterval int      `json:"sensorInterval"`
	Devices        []Device `json:"devices"`
	Advice         bool     `json:"advice"`
	Electricity    float64  `json:"electricity"`
	Gas            float64  `json:"gas"`
	COP            float64  `json:"cop"`
	Efficiency     float64  `json:"efficiency"`
	GasKWh         float64  `json:"gasKWh"`
}
type Unit struct {
	Choices map[string][]string `json:"choices,omitempty"`
	Device
	Online      bool               `json:"online"`
	Error       string             `json:"error"`
	Power       bool               `json:"power"`
	Mode        string             `json:"mode"`
	Target      float64            `json:"target"`
	Indoor      *float64           `json:"indoor"`
	Outdoor     *float64           `json:"outdoor"`
	Energy      *float64           `json:"energy"`
	Sensors     map[string]float64 `json:"sensors"`
	Fan         int                `json:"fan"`
	Fault       string             `json:"fault"`
	Firmware    string             `json:"firmware"`
	Updated     string             `json:"updated"`
	LouverUD    string             `json:"louverUD"`
	LouverLR    string             `json:"louverLR"`
	Telemetry   string             `json:"telemetry"`
	SensorFresh map[string]bool    `json:"sensorFresh"`
	SensorTimes map[string]string  `json:"sensorTimes"`
	Details     map[string]string  `json:"details"`
	expires     int64
	raw         []byte
}
type Thermostat struct {
	Online      bool    `json:"online"`
	Temperature float64 `json:"temperature"`
	Target      float64 `json:"target"`
	Heating     bool    `json:"heating"`
	Reason      string  `json:"reason"`
}
type State struct {
	Units      []Unit     `json:"units"`
	Thermostat Thermostat `json:"thermostat"`
	Config     Config     `json:"config"`
	Notice     string     `json:"notice"`
	Updated    string     `json:"updated"`
}

var mu sync.Mutex
var config Config
var state State
var notice string
var client = &http.Client{Timeout: 5 * time.Second}

const configPath = "/mnt/data/tsc/mhi.json"

func request(d Device, command string, contents map[string]interface{}) (map[string]interface{}, error) {
	if pluginDriver == "" && (command == "getDeviceInfo" || command == "updateAccountInfo") {
		r, e := callPlugin(pluginRequest{Op: "request", Device: d, Action: command, Contents: contents})
		return r.Data, e
	}
	return requestAt(d, command, contents, time.Now().Unix())
}
func requestAt(d Device, command string, contents map[string]interface{}, stamp int64) (map[string]interface{}, error) {
	body, _ := json.Marshal(map[string]interface{}{"apiVer": "1.0", "command": command, "deviceId": "toon-mhi-1930A0FMRM", "operatorId": "toon-mhi", "timestamp": stamp, "contents": contents})
	r, e := client.Post("http://"+d.Host+"/beaver/command/"+command, "application/json", bytes.NewReader(body))
	if e != nil {
		return nil, e
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", r.StatusCode)
	}
	var v map[string]interface{}
	if e = json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&v); e != nil {
		return nil, e
	}
	if v["result"] != float64(0) {
		return nil, fmt.Errorf("WF-RAC weigert (%v); probeer later", v["result"])
	}
	c, ok := v["contents"].(map[string]interface{})
	if !ok {
		if command == "updateAccountInfo" || command == "setAirconStat" {
			return map[string]interface{}{}, nil
		}
		return nil, fmt.Errorf("ongeldig antwoord")
	}
	return c, nil
}
func decode(d Device, c map[string]interface{}) (Unit, error) {
	u := Unit{Device: d, Sensors: map[string]float64{}}
	s, _ := c["airconStat"].(string)
	b, e := base64.StdEncoding.DecodeString(s)
	if e != nil || len(b) < 21 {
		return u, fmt.Errorf("ongeldige status")
	}
	off := int(b[18])*4 + 21
	if off+21 > len(b) {
		return u, fmt.Errorf("onvolledige status")
	}
	end := off + 19 + int(b[off+18])*4
	if end+2 > len(b) {
		return u, fmt.Errorf("onvolledige sensorgegevens")
	}
	r := b[off : off+18]
	u.raw = append([]byte(nil), r...)
	if expiry, ok := c["expires"].(float64); ok {
		u.expires = int64(expiry)
	}
	u.Details = map[string]string{"compressor": map[bool]string{true: "Actief", false: "Niet actief"}[r[9]&2 != 0], "model": fmt.Sprint(r[0] & 127), "afwezig": map[bool]string{true: "Aan", false: "Uit"}[r[10]&1 != 0]}
	u.Online = true
	u.Power = r[2]&3 == 1
	u.Mode = map[byte]string{0: "Auto", 4: "Drogen", 8: "Koelen", 12: "Ventileren", 16: "Verwarmen"}[r[2]&60]
	u.Target = float64(r[4]&127) / 2
	u.Fan = int(r[3] & 15)
	u.LouverUD, u.LouverLR = louvers(r)
	u.Fault = "Geen"
	if r[6] != 0 {
		if r[6]&128 != 0 {
			u.Fault = fmt.Sprintf("M%02d", r[6]&127)
		} else {
			u.Fault = fmt.Sprintf("E%d", r[6])
		}
	}
	for i := off + 19; i < end; i += 4 {
		a := b[i : i+4]
		decodeSensor(u.Sensors, a)
		if a[0] == 128 && a[2] != 255 {
			v := 0.0
			if a[1] == 16 {
				v = outdoorTemps[int(a[2])]
				u.Outdoor = &v
			}
			if a[1] == 32 {
				v = indoorTemps[int(a[2])]
				u.Indoor = &v
			}
		}
		if a[0] == 148 && a[1] == 16 {
			v := float64(uint16(a[2])|uint16(a[3])<<8) * 0.25
			u.Energy = &v
		}
	}
	if w, ok := c["wireless"].(map[string]interface{}); ok {
		u.Firmware = fmt.Sprint(w["firmVer"])
	}
	u.Updated = time.Now().Format("15:04:05")
	return u, nil
}
func readUnitDirect(d Device) (Unit, error) {
	if d.Driver == "esphome" {
		return readESP(d)
	}
	c, e := request(d, "getAirconStat", map[string]interface{}{"airconId": d.ID})
	if e != nil {
		return Unit{Device: d}, e
	}
	return decode(d, c)
}
func thermostat() Thermostat {
	t := Thermostat{Reason: "Thermostaat niet bereikbaar: koelen geblokkeerd"}
	r, e := client.Get("http://127.0.0.1/happ_thermstat?action=getThermostatInfo")
	if e != nil {
		return t
	}
	defer r.Body.Close()
	var v map[string]interface{}
	if json.NewDecoder(io.LimitReader(r.Body, 16384)).Decode(&v) != nil || v["result"] != "ok" {
		return t
	}
	val := func(k string) (float64, error) { return strconv.ParseFloat(fmt.Sprint(v[k]), 64) }
	temp, e1 := val("currentTemp")
	set, e2 := val("currentSetpoint")
	boiler, e3 := val("currentInternalBoilerSetpoint")
	if e1 != nil || e2 != nil || e3 != nil {
		return t
	}
	t.Online = true
	t.Temperature = temp / 100
	t.Target = set / 100
	t.Heating = set > temp || boiler > 10
	t.Reason = "Geen warmtevraag: koelen beschikbaar"
	if t.Heating {
		t.Reason = "Warmtevraag Toon: koelen geblokkeerd"
	}
	return t
}
func crc(b []byte) []byte {
	c := uint16(65535)
	for _, v := range b {
		c ^= uint16(v) << 8
		for i := 0; i < 8; i++ {
			if c&32768 != 0 {
				c = c<<1 ^ 4129
			} else {
				c <<= 1
			}
		}
	}
	return append(b, byte(c), byte(c>>8))
}

// Only mark the requested field as a write; echo other reported state unchanged.
func frame(u Unit, action string, value float64) (string, error) {
	if len(u.raw) != 18 {
		return "", fmt.Errorf("geen verse status")
	}
	cmd := make([]byte, 18)
	cmd[5] = 255
	// Status includes read-only flags which must not be echoed as commands.
	rcv := make([]byte, 18)
	rcv[0] = u.raw[0] & 127
	rcv[2] = u.raw[2] & 125
	rcv[3] = u.raw[3] & 63
	rcv[4] = u.raw[4] & 127
	rcv[8] = u.raw[8] & 8
	rcv[11] = u.raw[11] & 7
	rcv[12] = u.raw[12] & 5
	if rcv[0] == 1 {
		rcv[10] = u.raw[10] & 1
	}
	if rcv[0] == 1 || rcv[0] == 2 || rcv[0] == 3 {
		rcv[12] |= 128
		if u.raw[15]&1 != 0 {
			rcv[12] |= 16
		}
	}

	rcv[5] = 255
	switch action {
	case "power":
		if value != 0 && value != 1 {
			return "", fmt.Errorf("ongeldige aan/uit waarde")
		}
		cmd[2] = 2
		if value == 1 {
			cmd[2] = 3
			rcv[2] = rcv[2]&^3 | 1
		} else {
			rcv[2] &^= 3
		}
	case "fan":
		if value < 0 || value > 4 || math.Trunc(value) != value || math.IsNaN(value) {
			return "", fmt.Errorf("ongeldige ventilatorstand")
		}
		cmd[3] = []byte{15, 8, 9, 10, 14}[int(value)]
		rcv[3] = rcv[3]&^15 | []byte{7, 0, 1, 2, 6}[int(value)]
	case "mode":
		m := byte(value)
		if value != 0 && value != 4 && value != 8 && value != 16 && value != 12 {
			return "", fmt.Errorf("ongeldige modus")
		}
		cmd[2] = 32
		if m == 4 {
			cmd[2] = 36
		}
		if m == 8 {
			cmd[2] = 40
		}
		if m == 16 {
			cmd[2] = 48
		}
		if m == 12 {
			cmd[2] = 44
		}
		rcv[2] = rcv[2]&^60 | m
	case "louverUD", "louverLR":
		max := 4.0
		if action == "louverLR" {
			max = 7
		}
		if math.IsNaN(value) || value < 0 || value > max || math.Trunc(value) != value {
			return "", fmt.Errorf("ongeldige lamellenstand")
		}
		if action == "louverUD" {
			cmd[2], cmd[3] = 128, 128
			rcv[2] &^= 192
			rcv[3] &^= 240
			if value == 0 {
				cmd[2] = 192
				rcv[2] |= 64
			} else {
				cmd[3] |= byte(value-1) * 16
				rcv[3] |= byte(value-1) * 16
			}
		} else {
			cmd[12], cmd[11] = 2, 16
			rcv[12] &^= 3
			rcv[11] &^= 31
			if value == 0 {
				cmd[12] = 3
				rcv[12] |= 1
			} else {
				cmd[11] |= byte(value - 1)
				rcv[11] |= byte(value - 1)
			}
		}
	case "target":
		if value < 18 || value > 30 || math.Mod(value, 0.5) != 0 {
			return "", fmt.Errorf("kies 18 tot 30 graden")
		}
		cmd[4] = 128 + byte(value*2)
		rcv[4] = byte(value * 2)
	default:
		return "", fmt.Errorf("onbekende opdracht")
	}
	trailer := []byte{1, 255, 255, 255, 255}
	a := crc(append(cmd, trailer...))
	b := crc(append(rcv, trailer...))
	return base64.StdEncoding.EncodeToString(append(a, b...)), nil
}
func cooling(u Unit, action string, value float64) bool {
	mode := u.Mode
	if action == "mode" {
		mode = map[int]string{0: "Auto", 4: "Drogen", 8: "Koelen", 16: "Verwarmen", 12: "Ventileren"}[int(value)]
	}
	on := u.Power
	if action == "power" {
		on = value == 1
	}
	return on && (mode == "Koelen" || mode == "Drogen" || mode == "Auto" || mode == "")
}
func control(d Device, action string, value float64) error {
	u, e := readUnit(d)
	if e != nil {
		return e
	}
	t := thermostat()
	if cooling(u, action, value) && (!t.Online || t.Heating) {
		return fmt.Errorf("koelen geblokkeerd door thermostaat")
	}
	_, e = callPlugin(pluginRequest{Op: "control", Device: d, Action: action, Value: value})
	return e
}
func controlDirect(u Unit, action string, value float64) error {
	d := u.Device
	if d.Driver == "esphome" {
		return controlESP(d, action, value)
	}
	if strings.HasPrefix(action, "leave") {
		return controlLeave(u, action, value)
	}
	f, e := frame(u, action, value)
	if e != nil {
		return e
	}
	_, e = request(d, "setAirconStat", map[string]interface{}{"airconId": d.ID, "airconStat": f})
	writeErr := e
	time.Sleep(800 * time.Millisecond)
	v, e := readUnit(d)
	if e != nil {
		return fmt.Errorf("verstuurd; controle mislukt")
	}
	ok := false
	switch action {
	case "power":
		ok = v.Power == (value == 1)
	case "target":
		ok = v.Target == value
	case "fan":
		ok = v.Fan == int([]byte{7, 0, 1, 2, 6}[int(value)])
	case "louverUD", "louverLR":
		b, _ := base64.StdEncoding.DecodeString(f)
		ud, lr := louvers(b[25:43])
		if action == "louverUD" {
			ok = v.LouverUD == ud
		} else {
			ok = v.LouverLR == lr
		}
	case "mode":
		ok = v.Mode == map[int]string{0: "Auto", 4: "Drogen", 8: "Koelen", 16: "Verwarmen", 12: "Ventileren"}[int(value)]
	}
	if !ok {
		if writeErr != nil {
			return writeErr
		}
		return fmt.Errorf("opdracht nog niet bevestigd; vernieuw status")
	}
	return nil
}
func refresh() {
	s := State{Units: []Unit{}, Config: config, Thermostat: thermostat(), Notice: notice, Updated: time.Now().Format("15:04:05")}
	for _, d := range config.Devices {
		u, e := readUnit(d)
		if e != nil {
			u.Error = e.Error()
			if d.Driver == "esphome" {
				u.Error = e.Error()
			}
		}
		if e == nil && cooling(u, "", 0) && (!s.Thermostat.Online || s.Thermostat.Heating) {
			if e = control(d, "power", 0); e != nil {
				s.Notice = "Koelblokkering: " + e.Error()
			} else {
				u, _ = readUnit(d)
				s.Notice = "Koeling uitgezet wegens warmtevraag of ontbrekende Toon-status"
			}
		}
		if !u.Online {
			u.Device = d
		}
		for _, old := range state.Units {
			if old.ID != d.ID || old.Host != d.Host {
				continue
			}
			if !u.Online {
				err := u.Error
				u = old
				u.Online = false
				u.Error = err
				// Cached values must keep their original receipt timestamps.
				u.Sensors = nil
			} else {
				if u.Indoor == nil {
					u.Indoor = old.Indoor
				}
				if u.Outdoor == nil {
					u.Outdoor = old.Outdoor
				}
				if u.Energy == nil {
					u.Energy = old.Energy
				}
			}
		}
		u.Device = d

		s.Units = append(s.Units, u)
	}
	state = s
}
func main() {
	if pluginDriver != "" {
		runPlugin()
		return
	}
	b, e := os.ReadFile(configPath)
	if e != nil || json.Unmarshal(b, &config) != nil {
		fmt.Fprintln(os.Stderr, "mhi configuratie ontbreekt")
		os.Exit(1)
	}
	if config.SensorInterval == 0 {
		config.SensorInterval = 10
	}
	if err := writeUnitTiles(config.Devices); err != nil {
		fmt.Fprintln(os.Stderr, "MHI tegels:", err)
	}
	refresh()
	go func() {
		for {
			time.Sleep(5 * time.Second)
			mu.Lock()
			refresh()
			mu.Unlock()
		}
	}()
	http.HandleFunc("/plugins", pluginsHandler)
	http.HandleFunc("/discover", discoverHandler)
	http.HandleFunc("/esphome-key", keyHandler)
	http.HandleFunc("/resolve", resolveHandler)
	http.HandleFunc("/state", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(state)
	})
	http.HandleFunc("/command", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("X-Toon-MHI") != "1" {
			http.Error(w, "method", 405)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		var v struct {
			ID     string  `json:"id"`
			Index  int     `json:"index"`
			Action string  `json:"action"`
			Value  float64 `json:"value"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 2048)).Decode(&v) != nil {
			http.Error(w, "ongeldig", 400)
			return
		}
		if v.ID != "" {
			v.Index = -1
			for i, d := range config.Devices {
				if d.ID == v.ID {
					v.Index = i
					break
				}
			}
			if v.Index < 0 {
				http.Error(w, "airco verwijderd", 404)
				return
			}
		}
		if v.Index < 0 || v.Index >= len(config.Devices) {
			http.Error(w, "ongeldig apparaat", 400)
			return
		}
		e := control(config.Devices[v.Index], v.Action, v.Value)
		notice = "Opdracht bevestigd"
		if e != nil {
			notice = e.Error()
		}
		refresh()
		json.NewEncoder(w).Encode(map[string]interface{}{"message": notice, "ok": e == nil})
	})
	http.HandleFunc("/settings", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("X-Toon-MHI") != "1" {
			http.Error(w, "method", 405)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		next := config
		if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&next) != nil || len(next.Devices) > 12 || next.Electricity <= 0 || next.Gas <= 0 || next.COP < 1 || next.COP > 8 || next.Efficiency < 0.5 || next.Efficiency > 1 || next.GasKWh < 8 || next.GasKWh > 12 || next.SensorInterval < 10 || next.SensorInterval > 300 {
			http.Error(w, "controleer instellingen", 400)
			return
		}
		seen := map[string]bool{}
		for _, d := range next.Devices {
			if seen[d.ID] {
				http.Error(w, "airco bestaat al", 400)
				return
			}
			seen[d.ID] = true
			host, port, hostErr := net.SplitHostPort(d.Host)
			if len(d.Name) == 0 || len(d.Name) > 40 || !regexp.MustCompile(`^[a-f0-9]{12}$`).MatchString(d.ID) || hostErr != nil || net.ParseIP(host) == nil || ((d.Driver == "esphome" && port != "6053") || (d.Driver != "esphome" && port != "51443")) || (d.Driver != "" && d.Driver != "wfrac" && d.Driver != "esphome") {
				http.Error(w, "apparaat onvolledig", 400)
				return
			}
		}
		for _, d := range next.Devices {
			exists := false
			for _, old := range config.Devices {
				if old.ID == d.ID {
					exists = true
				}
			}
			if !exists && d.Driver != "esphome" {
				_, err := request(d, "updateAccountInfo", map[string]interface{}{"accountId": "toon-mhi", "airconId": d.ID, "remote": 0, "timezone": "Europe/Amsterdam"})
				if err != nil {
					http.Error(w, "Registratie WF-RAC mislukt: "+err.Error(), 502)
					return
				}
			}
		}
		if err := writeUnitTiles(next.Devices); err != nil {
			http.Error(w, "tegel aanmaken mislukt", 500)
			return
		}
		b, _ := json.MarshalIndent(next, "", "  ")
		if os.WriteFile(configPath+".tmp", b, 0600) != nil || os.Rename(configPath+".tmp", configPath) != nil {
			http.Error(w, "opslaan mislukt", 500)
			return
		}
		config = next
		refresh()
		w.Write([]byte("opgeslagen"))
	})
	if e := http.ListenAndServe("127.0.0.1:18781", nil); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}

// Files contain only validated IDs, never names, network addresses or telemetry.
// A stable URL lets Toon restore the same unit tile after reboot or reordering.
func writeUnitTiles(devices []Device) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return writeUnitTilesAt(filepath.Dir(exe), devices)
}
func writeUnitTilesAt(dir string, devices []Device) error {
	for _, d := range devices {
		if !regexp.MustCompile(`^[a-f0-9]{12}$`).MatchString(d.ID) {
			return fmt.Errorf("ongeldige tegel-ID")
		}
		data := []byte(fmt.Sprintf("import QtQuick 2.1\nMhiUnitTile { unitId: %q }\n", d.ID))
		path := filepath.Join(dir, "MhiUnit_"+d.ID+".qml")
		if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
			continue
		}
		if err := os.WriteFile(path+".tmp", data, 0644); err != nil {
			return err
		}
		if err := os.Rename(path+".tmp", path); err != nil {
			return err
		}
	}
	return nil
}

// Decode only operation-data records in the receive block, never command echoes.
func decodeSensor(s map[string]float64, a []byte) {
	if len(a) != 4 {
		return
	}
	if a[0] == 0x0c && (a[1] == 0x10 || a[1] == 0x11) {
		s["defrost"] = float64(a[1] - 0x10)
		return
	}
	if a[0] == 0xf8 && a[1] == 0x10 && a[2] >= 27 && a[2] <= 32 && a[3] != 255 {
		v := float64(a[3]) / 2
		if a[2] >= 31 {
			n, ok := map[byte]float64{0: 0, 3: 1, 5: 2, 7: 3, 14: 4}[a[3]]
			if !ok {
				return
			}
			v = n
		}
		s[fmt.Sprintf("leave%d", a[2])] = v
		return
	}
	if a[0] == 0x0d && (a[1] == 0 || a[1] == 2) {
		s["hotStart"] = float64(a[1] / 2)
		return
	}
	if a[2] == 255 {
		return
	}
	v := float64(a[2])
	switch {
	case a[0] == 0x7c && a[1] == 0x10:
		s["protection"] = v
	case a[0] == 0xdd && a[1] == 0x80:
		s["silent"] = float64((a[2] & 0x20) >> 5)
	case a[0] == 0x11 && a[1] >= 0x10 && a[1] <= 0x1f:
		s["hz"] = float64(a[1]-0x10)*25.6 + v/10
	case a[0] == 0x90 && a[1] == 0x10:
		s["current"] = v * 14 / 51
	case a[0] == 0x13 && a[1] == 0x10:
		s["eev"] = v
		s["eevPercent"] = math.Round(v * 100 / 255)
	case a[0] == 0x85 && a[1] == 0x10 && a[2] >= 0x12:
		s["discharge"] = v/2 + 32
	case a[0] == 0x81 && a[1] == 0x20 && a[2] > 0:
		s["indoorCoil"] = coilTemperature(v)
	case a[0] == 0xad && a[1] == 0x10 && a[2] > 0 && a[2] <= 100 && a[2] != 30:
		s["humidity"] = v
	case a[0] == 0x87 && a[1] == 0x10 && a[2] > 0:
		s["gasCoil"] = coilTemperature(v)
	}
}

// coilTemperature follows pywfrac's NTC divider conversion (MIT).
// Valid for indoor channels 0x81/0x87 only; above byte 170 is extrapolated.
func coilTemperature(raw float64) float64 {
	resistance := 1912.0 * (367.0/raw - 1.0)
	inverseKelvin := math.Log(resistance/5200.0)/3900.0 + 1.0/298.15
	return math.Round((1.0/inverseKelvin-273.15)*10) / 10
}
