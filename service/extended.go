package main

import (
	"encoding/base64"
	"fmt"
	"math"
	"time"
)

// Query all six settings afresh before writing: never replace another
// controller's changes using a cached partial HomeLeave configuration.
func controlLeave(u Unit, action string, value float64) error {
	sub := 0
	if _, err := fmt.Sscanf(action, "leave%d", &sub); err != nil || action != fmt.Sprintf("leave%d", sub) || sub < 27 || sub > 32 {
		return fmt.Errorf("onbekende instelling")
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 50 || math.Mod(value, .5) != 0 {
		return fmt.Errorf("kies 0 tot 50 °C in stappen van 0,5")
	}
	if sub >= 31 && (value > 4 || math.Trunc(value) != value) {
		return fmt.Errorf("ongeldige ventilatorstand")
	}
	f, err := serviceFrame(u, 0xf8)
	if err != nil {
		return err
	}
	time.Sleep(time.Second)
	c, err := request(u.Device, "setAirconStat", map[string]interface{}{"airconId": u.ID, "airconStat": f})
	if err != nil {
		return err
	}
	fresh, err := decode(u.Device, c)
	if err != nil {
		return err
	}
	if !sameSettings(u, fresh) {
		return fmt.Errorf("bediening veranderde tijdens uitlezen; probeer opnieuw")
	}
	expected := map[string]float64{}
	trailer := []byte{6}
	for n := 27; n <= 32; n++ {
		v, ok := fresh.Sensors[fmt.Sprintf("leave%d", n)]
		if !ok {
			return fmt.Errorf("afwezigheidsinstellingen niet volledig ontvangen; niets gewijzigd")
		}
		if n == sub {
			v = value
		}
		expected[fmt.Sprintf("leave%d", n)] = v
		b := byte(v * 2)
		if n >= 31 {
			if v < 0 || v > 4 || math.Trunc(v) != v {
				return fmt.Errorf("onbekende ventilatorstand")
			}
			b = []byte{0, 3, 5, 7, 14}[int(v)]
		}
		trailer = append(trailer, 248, 0, byte(n), b)
	}
	// Build from the fresh response, preserving current operating settings.
	f, err = serviceFrame(fresh, 0xf8)
	if err != nil {
		return err
	}
	b, _ := base64.StdEncoding.DecodeString(f)
	off := 21 + int(b[18])*4
	encoded := base64.StdEncoding.EncodeToString(append(crc(append(append([]byte{}, b[:18]...), trailer...)), b[off:]...))
	time.Sleep(time.Second)
	_, writeErr := request(u.Device, "setAirconStat", map[string]interface{}{"airconId": u.ID, "airconStat": encoded})
	time.Sleep(time.Second)
	after, err := readUnit(u.Device)
	if err != nil {
		return fmt.Errorf("verstuurd; controle mislukt")
	}
	if !sameSettings(u, after) {
		return fmt.Errorf("bedrijfsinstellingen veranderden; controleer de bediening")
	}
	f, err = serviceFrame(after, 0xf8)
	if err != nil {
		return err
	}
	time.Sleep(time.Second)
	c, err = request(u.Device, "setAirconStat", map[string]interface{}{"airconId": u.ID, "airconStat": f})
	if err != nil {
		return fmt.Errorf("verstuurd; teruglezen mislukt")
	}
	verified, err := decode(u.Device, c)
	if err != nil {
		return err
	}
	for key, want := range expected {
		v, ok := verified.Sensors[key]
		if !ok || v != want {
			if writeErr != nil {
				return writeErr
			}
			return fmt.Errorf("instelling niet bevestigd")
		}
	}
	// Refresh only this controller's RAM cache; no measurements are persisted.
	if cache := telemetryUnits[u.ID+"@"+u.Host]; cache != nil {
		for k, v := range verified.Sensors {
			cache.samples[k] = sample{v, time.Now()}
		}
	}
	return nil
}
