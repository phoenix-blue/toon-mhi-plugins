package main

import (
	"encoding/base64"
	"fmt"
	"time"
)

// One optional code per request: unsupported codes must not suppress other data.
var serviceCodes = []byte{0x11, 0x13, 0x90, 0x85, 0xad, 0x81, 0x87, 0x0c, 0x0d, 0x7c, 0xdd, 0xf8}

type sample struct {
	value float64
	at    time.Time
}
type telemetryCache struct {
	next    time.Time
	index   int
	lease   int64
	stopped bool
	status  string
	samples map[string]sample
}

var telemetryUnits = map[string]*telemetryCache{}

func sensorInterval() time.Duration {
	if config.SensorInterval < 10 || config.SensorInterval > 300 {
		return 10 * time.Second
	}
	return time.Duration(config.SensorInterval) * time.Second
}

func louvers(r []byte) (string, string) {
	ud, lr := "Niet ontvangen", "Niet ontvangen"
	if r[2]&192 == 64 {
		ud = "Swing"
	} else if p := r[3] & 240; p <= 48 && p%16 == 0 {
		ud = fmt.Sprintf("Stand %d / 4", p/16+1)
	}
	// Legacy modules can report a constant swing flag. Do not call it a
	// measured position; without extended-protocol detection it stays qualified.
	if r[12]&3 == 1 {
		lr = "Swing gemeld*"
	} else if p := r[11] & 31; p <= 6 {
		lr = fmt.Sprintf("Stand %d / 7*", p+1)
	}
	return ud, lr
}

func serviceFrame(u Unit, code byte) (string, error) {
	// Use the established sanitized receive-block builder, removing every write
	// bit from the command. No power, mode, setpoint, fan or vane command.
	f, err := frame(u, "power", 0)
	if err != nil {
		return "", err
	}
	b, _ := base64.StdEncoding.DecodeString(f)
	cmd := make([]byte, 18)
	cmd[5] = 255
	cmd[8] = u.raw[8] & 8
	// Preserve external room-sensor selection on regulating units.
	if u.Power && u.Mode != "Ventileren" {
		cmd[5] = u.raw[5]
	}
	rcv := append([]byte(nil), b[25:43]...)
	rcv[2] = u.raw[2] & 125
	trailer := []byte{1, code, 255, 255, 255}
	if code == 0xf8 {
		trailer = []byte{6}
		for sub := byte(27); sub <= 32; sub++ {
			trailer = append(trailer, 248, 255, sub, 0)
		}
	}
	return base64.StdEncoding.EncodeToString(append(crc(append(cmd, trailer...)), crc(append(rcv, []byte{1, 255, 255, 255, 255}...))...)), nil
}

func sameSettings(a, b Unit) bool {
	return a.Power == b.Power && a.Mode == b.Mode && a.Target == b.Target && a.Fan == b.Fan && a.LouverUD == b.LouverUD && a.LouverLR == b.LouverLR
}

func mergeSamples(u *Unit, c *telemetryCache, now time.Time) {
	if u.Sensors == nil {
		u.Sensors = map[string]float64{}
	}
	u.SensorTimes = map[string]string{}
	u.SensorFresh = map[string]bool{}
	for k, s := range c.samples {
		u.Sensors[k] = s.value
		u.SensorTimes[k] = s.at.Format("15:04:05")
		u.SensorFresh[k] = u.Online && now.Sub(s.at) <= time.Duration(len(serviceCodes))*sensorInterval()+30*time.Second
	}
	u.Telemetry = c.status
}

func enrichTelemetry(u Unit) Unit {
	key := u.ID + "@" + u.Host
	c := telemetryUnits[key]
	if c == nil {
		c = &telemetryCache{samples: map[string]sample{}, status: "Servicedata opvragen"}
		telemetryUnits[key] = c
	}
	now := time.Now()
	for k, v := range u.Sensors {
		c.samples[k] = sample{v, now}
	}
	if u.Online && !c.stopped && !now.Before(c.next) {
		c.next = now.Add(sensorInterval())
		// The module requires a gap between connections. Refresh before building
		// the query, then check the real settings again after the response.
		time.Sleep(time.Second)
		fresh, err := readUnit(u.Device)
		if err == nil {
			u = fresh
			stamp, ready := serviceStamp(time.Now().Unix(), u.expires, c.lease)
			if !ready {
				c.next = time.Unix(u.expires+1, 0)
				c.status = "Servicedata wacht: module door andere bediening bezet"
				mergeSamples(&u, c, time.Now())
				return u
			}
			code := serviceCodes[c.index%len(serviceCodes)]
			c.index++
			f, e := serviceFrame(u, code)
			if e == nil {
				time.Sleep(time.Second)
				contents, e := requestAt(u.Device, "setAirconStat", map[string]interface{}{"airconId": u.ID, "airconStat": f}, stamp)
				if e == nil {
					c.lease = stamp + 60
					reply, e := decode(u.Device, contents)
					if e == nil {
						for k, v := range reply.Sensors {
							c.samples[k] = sample{v, time.Now()}
						}
						c.status = "Servicedata · laatst ontvangen waarden"
					} else {
						c.status = "Serviceantwoord zonder meetwaarden"
					}
				} else {
					c.status = fmt.Sprintf("Servicevraag: %v", e)
				}
				time.Sleep(time.Second)
				after, e := readUnit(u.Device)
				if e == nil {
					if !sameSettings(u, after) {
						c.stopped = true
						c.status = "Servicevragen gestopt: instellingen veranderden"
					}
					u = after
				} else {
					c.next = time.Now().Add(time.Minute)
					c.status = "Servicevragen gepauzeerd: controle niet bereikbaar"
					u.Online = false
					u.Error = "Statuscontrole niet bereikbaar"
				}
			}
		} else {
			c.status = "Servicedata tijdelijk niet bereikbaar"
		}
	}
	mergeSamples(&u, c, time.Now())
	return u
}

// A foreign lease is checked against the request timestamp, not wall time.
// Wait for it to expire, then use a timestamp it accepts. Once we own the
// lease, backdating releases it again after about five seconds.
func serviceStamp(now, expires, ownLease int64) (int64, bool) {
	stamp := now - 55
	if expires != ownLease || ownLease == 0 {
		if expires > now {
			return 0, false
		}
		if stamp <= expires {
			stamp = expires + 1
		}
	}
	return stamp, true
}
