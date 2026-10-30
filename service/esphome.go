package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"google.golang.org/protobuf/encoding/protowire"
	"math"
	"net"
	"os"
	"strings"
	"time"
)

var ErrProtocol = errors.New("ESPHome protocolfout")
var ErrDeviceNotEncrypted = errors.New("ESPHome vereist versleuteling")
var ErrHandshake = errors.New("ESPHome verbinding geweigerd")
var ErrInvalidKey = errors.New("ESPHome API-sleutel onjuist")

const espKeysPath = "/mnt/data/tsc/mhi-esphome-keys.json"

type espField struct {
	n protowire.Number
	v uint64
	s string
}

func espFields(b []byte) ([]espField, error) {
	var out []espField
	for len(b) > 0 {
		n, t, k := protowire.ConsumeTag(b)
		if k < 0 {
			return nil, ErrProtocol
		}
		b = b[k:]
		f := espField{n: n}
		var size int
		switch t {
		case protowire.VarintType:
			f.v, size = protowire.ConsumeVarint(b)
		case protowire.Fixed32Type:
			var v uint32
			v, size = protowire.ConsumeFixed32(b)
			f.v = uint64(v)
		case protowire.BytesType:
			var v []byte
			v, size = protowire.ConsumeBytes(b)
			f.s = string(v)
		default:
			size = protowire.ConsumeFieldValue(n, t, b)
		}
		if size < 0 {
			return nil, ErrProtocol
		}
		out = append(out, f)
		b = b[size:]
	}
	return out, nil
}
func ev(fs []espField, n protowire.Number) uint64 {
	for _, f := range fs {
		if f.n == n {
			return f.v
		}
	}
	return 0
}
func es(fs []espField, n protowire.Number) string {
	for _, f := range fs {
		if f.n == n {
			return f.s
		}
	}
	return ""
}
func ef(fs []espField, n protowire.Number) float64 {
	return float64(math.Float32frombits(uint32(ev(fs, n))))
}
func epv(b []byte, n protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(b, n, protowire.VarintType), v)
}
func eps(b []byte, n protowire.Number, s string) []byte {
	return protowire.AppendString(protowire.AppendTag(b, n, protowire.BytesType), s)
}
func epf(b []byte, n protowire.Number, v uint32) []byte {
	return protowire.AppendFixed32(protowire.AppendTag(b, n, protowire.Fixed32Type), v)
}

type espEntity struct {
	key                uint32
	kind               uint32
	object, name, unit string
	fields             []espField
	state              []espField
	seen               bool
}
type espSession struct {
	conn     net.Conn
	codec    *noiseCodec
	entities map[uint32]*espEntity
	climate  *espEntity
}

func (s *espSession) close() { s.conn.Close() }
func (s *espSession) receive() (uint32, []espField, error) {
	for {
		id, b, e := s.codec.readMessage()
		if e != nil {
			return 0, nil, e
		}
		if id == 7 {
			if e = s.codec.writeMessage(8, nil); e != nil {
				return 0, nil, e
			}
			continue
		}
		if id == 36 {
			if e = s.codec.writeMessage(37, epf(nil, 1, uint32(time.Now().Unix()))); e != nil {
				return 0, nil, e
			}
			continue
		}
		if id == 5 {
			return 0, nil, errors.New("ESPHome verbreekt verbinding")
		}
		f, e := espFields(b)
		return id, f, e
	}
}
func openESP(d Device) (*espSession, error) {
	keys := map[string]string{}
	b, e := os.ReadFile(espKeysPath)
	if e != nil || json.Unmarshal(b, &keys) != nil || keys[d.ID] == "" {
		return nil, errors.New("ESPHome API-sleutel ontbreekt")
	}
	if CheckKey(keys[d.ID]) != nil {
		return nil, ErrInvalidKey
	}
	c, e := net.DialTimeout("tcp", d.Host, 3*time.Second)
	if e != nil {
		return nil, errors.New("ESPHome niet bereikbaar")
	}
	good := false
	defer func() {
		if !good {
			c.Close()
		}
	}()
	c.SetDeadline(time.Now().Add(8 * time.Second))
	codec, e := newNoiseCodec(bufio.NewReaderSize(c, maxNoiseFrame+3), c, keys[d.ID])
	if e != nil {
		return nil, errors.New("ESPHome versleutelde verbinding mislukt; controleer API-sleutel")
	}
	s := &espSession{conn: c, codec: codec, entities: map[uint32]*espEntity{}}
	if e = codec.writeMessage(1, epv(epv(eps(nil, 1, "Toon MHI"), 2, 1), 3, 14)); e != nil {
		return nil, e
	}
	id, _, e := s.receive()
	if e != nil || id != 2 {
		return nil, fmt.Errorf("%w: hello antwoord %d (%v)", ErrProtocol, id, e)
	}
	if e = codec.writeMessage(9, nil); e != nil {
		return nil, e
	}
	id, f, e := s.receive()
	if e != nil || id != 10 {
		return nil, fmt.Errorf("%w: apparaatinfo antwoord %d (%v)", ErrProtocol, id, e)
	}
	if strings.ReplaceAll(strings.ToLower(es(f, 3)), ":", "") != d.ID {
		return nil, errors.New("ESPHome MAC-adres komt niet overeen")
	}
	if e = codec.writeMessage(11, nil); e != nil {
		return nil, e
	}
	for count := 0; ; count++ {
		if count > 512 {
			return nil, ErrProtocol
		}
		id, f, e = s.receive()
		if e != nil {
			return nil, e
		}
		if id == 19 {
			break
		}
		if id == 12 || id == 16 || id == 46 || id == 52 {
			en := &espEntity{key: uint32(ev(f, 2)), kind: id, object: es(f, 1), name: es(f, 3), unit: es(f, 6), fields: f}
			s.entities[en.key] = en
			if id == 46 {
				if s.climate != nil {
					return nil, errors.New("Meerdere ESPHome climates op een apparaat worden niet ondersteund")
				}
				s.climate = en
			}
		}
	}
	if s.climate == nil {
		return nil, errors.New("Geen ESPHome aircobediening gevonden")
	}
	if e = codec.writeMessage(20, nil); e != nil {
		return nil, e
	}
	if e = s.collect(); e != nil {
		return nil, e
	}
	good = true
	return s, nil
}
func (s *espSession) collect() error {
	deadline := time.Now().Add(4 * time.Second)
	for {
		s.conn.SetReadDeadline(deadline)
		id, f, e := s.receive()
		if e != nil {
			if ne, ok := e.(net.Error); ok && ne.Timeout() && s.climate.seen {
				return nil
			}
			return e
		}
		if id == 21 || id == 25 || id == 47 || id == 53 {
			if en := s.entities[uint32(ev(f, 1))]; en != nil {
				en.state = f
				en.seen = true
			}
		}
		all := true
		for _, en := range s.entities {
			if !en.seen {
				all = false
				break
			}
		}
		if all {
			return nil
		}
	}
}
func espSelect(s *espSession, axis string) *espEntity {
	for _, en := range s.entities {
		if en.kind == 52 && strings.Contains(en.object, "fan_control_"+axis) {
			return en
		}
	}
	return nil
}
func espOptions(en *espEntity) []string {
	if en == nil {
		return nil
	}
	var out []string
	for _, f := range en.fields {
		if f.n == 6 {
			out = append(out, f.s)
		}
	}
	return out
}
func espChoices(s *espSession, axis string) []string {
	opts := espOptions(espSelect(s, axis))
	var out []string
	for _, o := range opts {
		if strings.EqualFold(o, "swing") {
			out = append(out, o)
		}
	}
	for _, o := range opts {
		if !strings.EqualFold(o, "swing") {
			out = append(out, o)
		}
	}
	return out
}
func espUnit(d Device, s *espSession) Unit {
	f := s.climate.state
	u := Unit{Device: d, Online: ev(f, 17) == 0, Sensors: map[string]float64{}, SensorFresh: map[string]bool{}, SensorTimes: map[string]string{}, Details: map[string]string{}, Updated: time.Now().Format("15:04:05"), Choices: map[string][]string{}, Telemetry: "ESPHome native API"}
	mode := ev(f, 2)
	u.Power = mode != 0
	u.Mode = map[uint64]string{0: "Uit", 1: "Auto", 2: "Koelen", 3: "Verwarmen", 4: "Ventileren", 5: "Drogen", 6: "Auto"}[mode]
	u.Target = ef(f, 4)
	if math.IsNaN(u.Target) || math.IsInf(u.Target, 0) {
		u.Target = 0
	}
	in := ef(f, 3)
	if !math.IsNaN(in) && !math.IsInf(in, 0) {
		u.Indoor = &in
	}
	u.Fan = -1
	if fan, ok := map[uint64]int{2: 7, 3: 1, 4: 2, 5: 6, 9: 0}[ev(f, 9)]; ok {
		u.Fan = fan
	}
	for _, axis := range []string{"up_down", "left_right"} {
		en := espSelect(s, axis)
		key := "louverUD"
		if axis == "left_right" {
			key = "louverLR"
		}
		opts := espChoices(s, axis)
		u.Choices[key] = opts
		if en != nil && en.seen && ev(en.state, 3) == 0 {
			v := es(en.state, 2)
			if key == "louverUD" {
				u.LouverUD = v
			} else {
				u.LouverLR = v
			}
		}
	}
	names := map[string]string{"outdoor_temperature": "outdoor", "return_air_temperature": "indoor", "compressor_frequency": "hz", "indoor_unit_fan_speed": "indoorFan", "outdoor_unit_fan_speed": "outdoorFan", "indoor_u_bend_he_temp_1": "indoorCoil", "indoor_suction_header_he_temp_3": "gasCoil", "outdoor_he_temp": "outdoorCoil", "outdoor_unit_exp_valve": "eev", "outdoor_unit_discharge_pipe": "discharge", "outdoor_unit_discharge_pipe_super_heat": "superheat", "compressor_protection_code": "protection", "defrost": "defrost", "energy_used": "energy", "error_code": "fault"}
	for _, en := range s.entities {
		if !en.seen || ev(en.state, 3) != 0 || (en.kind != 16 && en.kind != 12) {
			continue
		}
		obj := en.object
		var key string
		for suffix, k := range names {
			if obj == suffix || strings.HasSuffix(obj, "_"+suffix) {
				key = k
				break
			}
		}
		if key == "" {
			continue
		}
		v := ef(en.state, 2)
		if en.kind == 12 {
			v = float64(ev(en.state, 2))
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		u.Sensors[key] = v
		u.SensorFresh[key] = true
		u.SensorTimes[key] = u.Updated
		switch key {
		case "outdoor":
			x := v
			u.Outdoor = &x
		case "energy":
			if en.unit == "kWh" {
				x := v
				u.Energy = &x
			}
		case "fault":
			u.Fault = fmt.Sprintf("%02.0f", v)
		}
	}
	return u
}

type espCacheEntry struct {
	unit Unit
	at   time.Time
}

var espCache = map[Device]espCacheEntry{}

func readESP(d Device) (Unit, error) {
	if cached, ok := espCache[d]; ok && time.Since(cached.at) < time.Duration(config.SensorInterval)*time.Second {
		return cached.unit, nil
	}
	s, e := openESP(d)
	if e != nil {
		return Unit{Device: d}, e
	}
	defer s.close()
	u := espUnit(d, s)
	espCache[d] = espCacheEntry{u, time.Now()}
	return u, nil
}
func espSupported(en *espEntity, field protowire.Number, value uint64) bool {
	for _, f := range en.fields {
		if f.n == field {
			if f.s != "" {
				b := []byte(f.s)
				for len(b) > 0 {
					v, n := protowire.ConsumeVarint(b)
					if n < 0 {
						break
					}
					if v == value {
						return true
					}
					b = b[n:]
				}
			} else if f.v == value {
				return true
			}
		}
	}
	return false
}
func controlESP(d Device, action string, value float64) error {
	delete(espCache, d)
	s, e := openESP(d)
	if e != nil {
		return e
	}
	defer s.close()
	en := s.climate
	id := uint32(48)
	b := epf(nil, 1, en.key)
	var field protowire.Number
	var want uint64
	var text string
	switch action {
	case "power", "mode":
		field = 2
		if action == "power" {
			if value != 0 && value != 1 {
				return errors.New("ongeldige aan/uitstand")
			}
			if value == 1 {
				want = 3
			}
		} else {
			m := map[int]uint64{0: 1, 4: 5, 8: 2, 12: 4, 16: 3}
			var ok bool
			want, ok = m[int(value)]
			if !ok || value != math.Trunc(value) {
				return errors.New("ongeldige modus")
			}
		}
		if want != 0 && !espSupported(en, 7, want) {
			return errors.New("Modus niet ondersteund")
		}
		b = epv(epv(b, 2, 1), 3, want)
	case "target":
		field = 4
		lo, hi := ef(en.fields, 8), ef(en.fields, 9)
		if value < lo || value > hi || math.IsNaN(value) || math.IsInf(value, 0) {
			return errors.New("Temperatuur buiten ESPHome-bereik")
		}
		want = uint64(math.Float32bits(float32(value)))
		b = epf(epv(b, 4, 1), 5, uint32(want))
	case "fan":
		field = 9
		fans := []uint64{2, 9, 3, 4, 5}
		if value != math.Trunc(value) || value < 0 || value >= float64(len(fans)) {
			return errors.New("ongeldige ventilatorstand")
		}
		want = fans[int(value)]
		if !espSupported(en, 13, want) {
			return errors.New("Ventilatorstand niet ondersteund")
		}
		b = epv(epv(b, 12, 1), 13, want)
	case "louverUD", "louverLR":
		axis := "up_down"
		if action == "louverLR" {
			axis = "left_right"
		}
		en = espSelect(s, axis)
		opts := espChoices(s, axis)
		if en == nil || value != math.Trunc(value) || value < 0 || value >= float64(len(opts)) {
			return errors.New("Lamellenstand niet ondersteund")
		}
		text = opts[int(value)]
		id = 54
		b = eps(epf(nil, 1, en.key), 2, text)
	default:
		return errors.New("Instelling niet ondersteund via ESPHome")
	}
	s.conn.SetDeadline(time.Now().Add(4 * time.Second))
	if e = s.codec.writeMessage(id, b); e != nil {
		return errors.New("ESPHome versturen mislukt")
	}
	for {
		msg, f, e := s.receive()
		if e != nil {
			return errors.New("ESPHome verstuurd, maar niet bevestigd")
		}
		if uint32(ev(f, 1)) != en.key {
			continue
		}
		if id == 54 && msg == 53 && ev(f, 3) == 0 && es(f, 2) == text {
			return nil
		}
		if id == 48 && msg == 47 && ev(f, 17) == 0 && ev(f, field) == want {
			return nil
		}
	}
}
