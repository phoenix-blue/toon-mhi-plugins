// Adapted from avanha/pmaas-plugin-esphome (MIT); see LICENSE.esphome.
package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"strings"

	"github.com/flynn/noise"
)

// ESPHome's encrypted API is the Noise protocol, Noise_NNpsk0_25519_ChaChaPoly_SHA256: no static keys, with a
// 32 byte pre-shared key (the api: encryption: key in the device's YAML, base64 encoded) providing the
// authentication. The prologue is fixed. Every parameter here has to match the device's exactly, and this is
// written to match Home Assistant's client (aioesphomeapi, _frame_helper/noise.py) and checked against it: see
// the ESPHome protocol specification.
//
// The framing is a 0x01 byte, a two byte length, and that many bytes:
//
//	client -> device: an empty frame (the "hello"), then a handshake frame: 0x00 and the first Noise message
//	device -> client: a frame with the protocol it chose (0x01), the device's name and MAC as zero-terminated
//	                  strings, then a handshake frame: 0x00 and the second Noise message, or a non-zero byte
//	                  and a reason in text if it refuses
//
// After that every frame is one encrypted message: a two byte type, a two byte length, and the payload.
var noisePrologue = []byte("NoiseAPIInit\x00\x00")

// maxNoiseFrame is the most a two byte length can say, so there's nothing to bound.
const maxNoiseFrame = 0xFFFF

// decodeKey reads the pre-shared key as ESPHome's YAML gives it: base64, of 32 bytes.
func decodeKey(key string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(key))
	if err != nil {
		return nil, fmt.Errorf("the encryption key isn't base64: %w", err)
	}

	if len(decoded) != 32 {
		return nil, fmt.Errorf("the encryption key must be 32 bytes, and this one is %d", len(decoded))
	}

	return decoded, nil
}

// CheckKey reports whether key is a usable encryption key, so that a mistake in a configuration is reported when
// it's read rather than each time a connection is attempted.
func CheckKey(key string) error {
	_, err := decodeKey(key)

	return err
}

func noiseConfig(psk []byte, initiator bool) noise.Config {
	return noise.Config{
		CipherSuite:           noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256),
		Pattern:               noise.HandshakeNN,
		Initiator:             initiator,
		Prologue:              noisePrologue,
		PresharedKey:          psk,
		PresharedKeyPlacement: 0,
	}
}

// noiseCodec is the encrypted framing, once the handshake is done.
type noiseCodec struct {
	reader  *bufio.Reader
	writer  io.Writer
	encrypt *noise.CipherState
	decrypt *noise.CipherState

	// ServerName and ServerMAC are what the device said about itself in its hello. They're empty for firmware
	// too old to say.
	serverName string
	serverMAC  string
}

func writeNoiseFrame(w io.Writer, data []byte) error {
	if len(data) > maxNoiseFrame {
		return fmt.Errorf("%w: a frame of %d bytes doesn't fit", ErrProtocol, len(data))
	}

	frame := make([]byte, 0, 3+len(data))
	frame = append(frame, 0x01, byte(len(data)>>8), byte(len(data)))
	frame = append(frame, data...)

	_, err := w.Write(frame)

	return err
}

func readNoiseFrame(r *bufio.Reader) ([]byte, error) {
	header, err := r.Peek(3)
	if err != nil {
		return nil, err
	}
	switch header[0] {
	case 0x01:
	case 0x00:
		return nil, ErrDeviceNotEncrypted
	default:
		return nil, fmt.Errorf("%w: invalid frame marker", ErrProtocol)
	}
	size := int(header[1])<<8 | int(header[2])
	data, err := r.Peek(size + 3)
	if err != nil {
		return nil, err
	}
	frame := append([]byte(nil), data[3:]...)
	_, _ = r.Discard(size + 3)

	return frame, nil
}

// newNoiseCodec does the handshake as the initiator, over a connection nothing has been sent on yet.
func newNoiseCodec(reader *bufio.Reader, writer io.Writer, key string) (*noiseCodec, error) {
	psk, err := decodeKey(key)
	if err != nil {
		return nil, err
	}

	state, err := noise.NewHandshakeState(noiseConfig(psk, true))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrHandshake, err)
	}

	first, _, _, err := state.WriteMessage(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrHandshake, err)
	}

	// Both frames go out together, as the reference client does: the hello, then the handshake.
	var out bytes.Buffer

	_ = writeNoiseFrame(&out, nil)
	_ = writeNoiseFrame(&out, append([]byte{0x00}, first...))

	if _, err := writer.Write(out.Bytes()); err != nil {
		return nil, err
	}

	codec := &noiseCodec{reader: reader, writer: writer}

	hello, err := readNoiseFrame(reader)
	if err != nil {
		return nil, err
	}

	if err := codec.parseServerHello(hello); err != nil {
		return nil, err
	}

	response, err := readNoiseFrame(reader)
	if err != nil {
		return nil, err
	}

	if len(response) == 0 {
		return nil, fmt.Errorf("%w: the handshake response is empty", ErrHandshake)
	}

	if response[0] != 0x00 {
		// The device says why in text. The one that matters is a key that isn't its key.
		reason := sanitize(string(response[1:]))

		if reason == "Handshake MAC failure" {
			return nil, ErrInvalidKey
		}

		return nil, fmt.Errorf("%w: %s", ErrHandshake, reason)
	}

	_, encrypt, decrypt, err := state.ReadMessage(nil, response[1:])
	if err != nil {
		// A response that doesn't authenticate is the same as the device's own refusal: it isn't using our key.
		return nil, fmt.Errorf("%w: %w", ErrInvalidKey, err)
	}

	if encrypt == nil || decrypt == nil {
		return nil, fmt.Errorf("%w: the handshake didn't finish", ErrHandshake)
	}

	codec.encrypt, codec.decrypt = encrypt, decrypt

	return codec, nil
}

// parseServerHello reads which protocol the device chose, and the name and MAC address it adds after it.
func (c *noiseCodec) parseServerHello(hello []byte) error {
	if len(hello) == 0 {
		return fmt.Errorf("%w: the device's hello is empty", ErrHandshake)
	}

	if hello[0] != 0x01 {
		return fmt.Errorf("%w: the device chose protocol %d, and only 1 is known", ErrHandshake, hello[0])
	}

	// A name came in firmware from 2022.2 and a MAC in 2025.4, each zero-terminated, so older devices send less.
	fields := bytes.SplitN(hello[1:], []byte{0}, 3)

	if len(fields) >= 2 {
		c.serverName = sanitize(string(fields[0]))
	}

	if len(fields) >= 3 {
		c.serverMAC = sanitize(string(fields[1]))
	}

	return nil
}

// sanitize makes text from the network safe to put in an error or a log: printable, and not long.
func sanitize(text string) string {
	const limit = 128

	var out strings.Builder

	for _, r := range text {
		if out.Len() >= limit {
			break
		}

		if r >= 0x20 && r != 0x7f {
			out.WriteRune(r)
		}
	}

	return out.String()
}

func (c *noiseCodec) writeMessage(id uint32, payload []byte) error {
	if id > 0xFFFF || len(payload) > 0xFFFF-20 {
		return fmt.Errorf("%w: a message of type %d and %d bytes doesn't fit in an encrypted frame", ErrProtocol, id, len(payload))
	}

	plain := make([]byte, 4, 4+len(payload))
	binary.BigEndian.PutUint16(plain[0:2], uint16(id))
	binary.BigEndian.PutUint16(plain[2:4], uint16(len(payload)))
	plain = append(plain, payload...)

	encrypted, err := c.encrypt.Encrypt(nil, nil, plain)
	if err != nil {
		return err
	}

	return writeNoiseFrame(c.writer, encrypted)
}

func (c *noiseCodec) readMessage() (uint32, []byte, error) {
	frame, err := readNoiseFrame(c.reader)
	if err != nil {
		return 0, nil, err
	}

	plain, err := c.decrypt.Decrypt(nil, nil, frame)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: a frame didn't decrypt: %w", ErrProtocol, err)
	}

	if len(plain) < 4 {
		return 0, nil, fmt.Errorf("%w: a decrypted message is only %d bytes", ErrProtocol, len(plain))
	}

	id := uint32(binary.BigEndian.Uint16(plain[0:2]))
	length := int(binary.BigEndian.Uint16(plain[2:4]))

	if len(plain)-4 != length {
		return 0, nil, fmt.Errorf("%w: a message says it has %d bytes and has %d", ErrProtocol, length, len(plain)-4)
	}

	return id, plain[4:], nil
}
