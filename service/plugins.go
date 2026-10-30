package main

// Plugins are separate processes. Only the core decides whether cooling is safe.
import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

const pluginAPI = 1

var pluginDriver string // set by the connector build
var pluginVersion = "1.0.0"
var pluginDir = "/mnt/data/tsc/mhi-plugins"

const catalogURL = "https://raw.githubusercontent.com/phoenix-blue/toon-mhi-plugins/main/catalog.json"

var pluginHTTP = &http.Client{
	Timeout: 90 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("onveilige download-omleiding")
		}
		if len(via) >= 10 {
			return fmt.Errorf("te veel download-omleidingen")
		}
		return nil
	},
}

type pluginRequest struct {
	Op       string                 `json:"op"`
	Device   Device                 `json:"device"`
	Action   string                 `json:"action,omitempty"`
	Value    float64                `json:"value,omitempty"`
	Contents map[string]interface{} `json:"contents,omitempty"`
	Interval int                    `json:"interval,omitempty"`
}
type pluginReply struct {
	API     int                    `json:"api"`
	ID      string                 `json:"id"`
	Version string                 `json:"version"`
	Unit    Unit                   `json:"unit"`
	Data    map[string]interface{} `json:"data,omitempty"`
	Error   string                 `json:"error,omitempty"`
}
type pluginManifest struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	API      int    `json:"api"`
	Platform string `json:"platform"`
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
}
type pluginCatalog struct {
	Plugins []pluginManifest `json:"plugins"`
}

func driverID(d Device) string {
	if d.Driver == "" {
		return "wfrac"
	}
	return d.Driver
}
func validPluginID(id string) bool {
	return regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`).MatchString(id)
}
func pluginPath(id string) string { return filepath.Join(pluginDir, id, "connector") }

func runPlugin() {
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 4096), 128*1024)
	enc := json.NewEncoder(os.Stdout)
	for scan.Scan() {
		var q pluginRequest
		r := pluginReply{API: pluginAPI, ID: pluginDriver, Version: pluginVersion}
		var err error
		if json.Unmarshal(scan.Bytes(), &q) != nil {
			err = fmt.Errorf("ongeldig pluginbericht")
		} else if q.Op == "hello" {
		} else if driverID(q.Device) != pluginDriver {
			err = fmt.Errorf("verkeerde plugin")
		} else {
			config.SensorInterval = q.Interval
			switch q.Op {
			case "read":
				r.Unit, err = readUnitDirect(q.Device)
				if err == nil && pluginDriver == "wfrac" {
					r.Unit = enrichTelemetry(r.Unit)
				}
			case "control":
				var u Unit
				u, err = readUnitDirect(q.Device)
				if err == nil {
					err = controlDirect(u, q.Action, q.Value)
				}
			case "request":
				if pluginDriver != "wfrac" || (q.Action != "getDeviceInfo" && q.Action != "updateAccountInfo") {
					err = fmt.Errorf("opdracht niet ondersteund")
				} else {
					r.Data, err = request(q.Device, q.Action, q.Contents)
				}
			default:
				err = fmt.Errorf("onbekende pluginopdracht")
			}
		}
		if err != nil {
			r.Error = err.Error()
		}
		if enc.Encode(r) != nil {
			return
		}
	}
}

type pluginProcess struct {
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *bufio.Scanner
}

var pluginMu sync.Mutex
var pluginProcesses = map[string]*pluginProcess{}

func stopPlugin(id string) { // pluginMu held
	if p := pluginProcesses[id]; p != nil {
		p.input.Close()
		p.cmd.Process.Kill()
		p.cmd.Wait()
		delete(pluginProcesses, id)
	}
}
func startPlugin(path string) (*pluginProcess, error) {
	cmd := exec.Command(path)
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		input.Close()
		return nil, err
	}
	scan := bufio.NewScanner(out)
	scan.Buffer(make([]byte, 4096), 128*1024)
	return &pluginProcess{cmd, input, scan}, nil
}
func exchange(p *pluginProcess, q pluginRequest) (pluginReply, error) {
	type result struct {
		r pluginReply
		e error
	}
	ch := make(chan result, 1)
	go func() {
		var r pluginReply
		e := json.NewEncoder(p.input).Encode(q)
		if e == nil {
			if p.output.Scan() {
				e = json.Unmarshal(p.output.Bytes(), &r)
			} else {
				e = fmt.Errorf("pluginverbinding gesloten")
			}
		}
		ch <- result{r, e}
	}()
	select {
	case v := <-ch:
		return v.r, v.e
	case <-time.After(45 * time.Second):
		p.cmd.Process.Kill()
		v := <-ch
		_ = v
		return pluginReply{}, fmt.Errorf("plugin reageert niet")
	}
}
func callPlugin(q pluginRequest) (pluginReply, error) {
	id := driverID(q.Device)
	if !validPluginID(id) {
		return pluginReply{}, fmt.Errorf("ongeldige plugin")
	}
	pluginMu.Lock()
	defer pluginMu.Unlock()
	p := pluginProcesses[id]
	if p == nil {
		var e error
		p, e = startPlugin(pluginPath(id))
		if e != nil {
			return pluginReply{}, fmt.Errorf("plugin %s ontbreekt; installeer via Instellingen", id)
		}
		pluginProcesses[id] = p
	}
	r, e := exchange(p, q)
	if e != nil || r.API != pluginAPI || r.ID != id {
		stopPlugin(id)
		return pluginReply{}, fmt.Errorf("plugin %s niet beschikbaar of niet compatibel", id)
	}
	if r.Error != "" {
		return r, fmt.Errorf("%s", r.Error)
	}
	return r, nil
}
func readUnit(d Device) (Unit, error) {
	if pluginDriver != "" {
		return readUnitDirect(d)
	}
	r, e := callPlugin(pluginRequest{Op: "read", Device: d, Interval: config.SensorInterval})
	return r.Unit, e
}
func validateManifest(m pluginManifest) error {
	u, e := url.Parse(m.URL)
	if !validPluginID(m.ID) || m.Version == "" || m.API != pluginAPI || m.Platform != "linux-armv7" || e != nil || u.Scheme != "https" || u.Host != "github.com" || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(m.SHA256) {
		return fmt.Errorf("ongeldige of niet compatibele plugin")
	}
	return nil
}
func loadCatalog() (pluginCatalog, error) {
	var c pluginCatalog
	r, e := pluginHTTP.Get(catalogURL)
	if e != nil {
		return c, fmt.Errorf("plugincatalogus niet bereikbaar")
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return c, fmt.Errorf("plugincatalogus HTTP %d", r.StatusCode)
	}
	if e = json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&c); e != nil {
		return c, fmt.Errorf("ongeldige plugincatalogus")
	}
	seen := map[string]bool{}
	for _, m := range c.Plugins {
		if e = validateManifest(m); e != nil {
			return c, e
		}
		if seen[m.ID] {
			return c, fmt.Errorf("dubbele plugin")
		}
		seen[m.ID] = true
	}
	return c, nil
}
func installedManifest(id string) (pluginManifest, error) {
	var m pluginManifest
	b, e := os.ReadFile(filepath.Join(pluginDir, id, "manifest.json"))
	if e == nil {
		e = json.Unmarshal(b, &m)
	}
	return m, e
}
func installPlugin(m pluginManifest) error {
	if e := validateManifest(m); e != nil {
		return e
	}
	r, e := pluginHTTP.Get(m.URL)
	if e != nil {
		return fmt.Errorf("download mislukt")
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return fmt.Errorf("download HTTP %d", r.StatusCode)
	}
	dir := filepath.Join(pluginDir, m.ID)
	if e = os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, "download-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, h), io.LimitReader(r.Body, 32*1024*1024+1))
	closeErr := f.Close()
	if e != nil || closeErr != nil || n > 32*1024*1024 || hex.EncodeToString(h.Sum(nil)) != m.SHA256 {
		return fmt.Errorf("downloadcontrole mislukt; bestaande plugin behouden")
	}
	if e = os.Chmod(f.Name(), 0700); e != nil {
		return e
	}
	p, e := startPlugin(f.Name())
	if e != nil {
		return fmt.Errorf("plugin kan niet starten")
	}
	hello, e := exchange(p, pluginRequest{Op: "hello"})
	p.input.Close()
	p.cmd.Process.Kill()
	p.cmd.Wait()
	if e != nil || hello.API != pluginAPI || hello.ID != m.ID || hello.Version != m.Version {
		return fmt.Errorf("plugincontrole mislukt; bestaande plugin behouden")
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	mp := filepath.Join(dir, "manifest.json")
	manifestTemp := mp + ".tmp"
	if e = os.WriteFile(manifestTemp, b, 0600); e != nil {
		return e
	}
	defer os.Remove(manifestTemp)
	pluginMu.Lock()
	defer pluginMu.Unlock()
	stopPlugin(m.ID)
	binaryPath := pluginPath(m.ID)
	binaryBackup := binaryPath + ".previous"
	manifestBackup := mp + ".previous"
	_ = os.Remove(binaryBackup)
	_ = os.Remove(manifestBackup)
	hadBinary := false
	if _, statErr := os.Stat(binaryPath); statErr == nil {
		if e = os.Rename(binaryPath, binaryBackup); e != nil {
			return e
		}
		hadBinary = true
	}
	hadManifest := false
	if _, statErr := os.Stat(mp); statErr == nil {
		if e = os.Rename(mp, manifestBackup); e != nil {
			if hadBinary {
				_ = os.Rename(binaryBackup, binaryPath)
			}
			return e
		}
		hadManifest = true
	}
	rollback := func() {
		_ = os.Remove(binaryPath)
		_ = os.Remove(mp)
		if hadBinary {
			_ = os.Rename(binaryBackup, binaryPath)
		}
		if hadManifest {
			_ = os.Rename(manifestBackup, mp)
		}
	}
	if e = os.Rename(f.Name(), binaryPath); e != nil {
		rollback()
		return e
	}
	if e = os.Rename(manifestTemp, mp); e != nil {
		rollback()
		return e
	}
	_ = os.Remove(binaryBackup)
	_ = os.Remove(manifestBackup)
	return nil
}
func removePlugin(id string) error {
	if !validPluginID(id) {
		return fmt.Errorf("ongeldige plugin")
	}
	for _, d := range config.Devices {
		if driverID(d) == id {
			return fmt.Errorf("verwijder eerst de airco’s die deze plugin gebruiken")
		}
	}
	pluginMu.Lock()
	defer pluginMu.Unlock()
	stopPlugin(id)
	return os.RemoveAll(filepath.Join(pluginDir, id))
}
func pluginsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == "GET" {
		c, e := loadCatalog()
		message := ""
		if e != nil {
			message = e.Error()
			c = pluginCatalog{}
		}
		type row struct {
			pluginManifest
			Installed string `json:"installed"`
			InUse     bool   `json:"inUse"`
		}
		rows := []row{}
		seen := map[string]bool{}
		mu.Lock()
		defer mu.Unlock()
		add := func(m pluginManifest) {
			x := row{pluginManifest: m}
			if im, e := installedManifest(m.ID); e == nil {
				x.Installed = im.Version
			}
			for _, d := range config.Devices {
				if driverID(d) == m.ID {
					x.InUse = true
				}
			}
			rows = append(rows, x)
			seen[m.ID] = true
		}
		for _, m := range c.Plugins {
			add(m)
		}
		entries, _ := os.ReadDir(pluginDir)
		for _, entry := range entries {
			if !seen[entry.Name()] && validPluginID(entry.Name()) {
				if m, e := installedManifest(entry.Name()); e == nil {
					add(m)
				}
			}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"plugins": rows, "message": message})
		return
	}
	if r.Method != "POST" || r.Header.Get("X-Toon-MHI") != "1" {
		http.Error(w, "method", 405)
		return
	}
	var q struct {
		ID     string `json:"id"`
		Action string `json:"action"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&q) != nil {
		http.Error(w, "ongeldig", 400)
		return
	}
	var e error
	switch q.Action {
	case "install":
		var c pluginCatalog
		c, e = loadCatalog()
		if e == nil {
			e = fmt.Errorf("plugin niet in catalogus")
			for _, m := range c.Plugins {
				if m.ID == q.ID {
					e = installPlugin(m)
					break
				}
			}
		}
	case "remove":
		mu.Lock()
		e = removePlugin(q.ID)
		mu.Unlock()
	default:
		e = fmt.Errorf("onbekende opdracht")
	}
	if e != nil {
		http.Error(w, e.Error(), 400)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "message": "Plugin bijgewerkt"})
}
