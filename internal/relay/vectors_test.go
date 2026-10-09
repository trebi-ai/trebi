package relay

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/flynn/noise"
	"github.com/hashicorp/yamux"
	"github.com/trebi-ai/trebi/internal/mobile"
	"golang.org/x/crypto/curve25519"
)

var update = flag.Bool("update", false, "rewrite the relay contract vectors") //nolint:gochecknoglobals // test flag

const contractDir = "testdata/contract"

type handshakeVector struct {
	Protocol               string            `json:"protocol"`
	DeviceID               string            `json:"device_id"`
	PrologueHex            string            `json:"prologue_hex"`
	InitiatorStaticPrivate string            `json:"initiator_static_private_hex"`
	InitiatorStaticPublic  string            `json:"initiator_static_public_hex"`
	InitiatorEphemeral     string            `json:"initiator_ephemeral_private_hex"`
	ResponderStaticPrivate string            `json:"responder_static_private_hex"`
	ResponderStaticPublic  string            `json:"responder_static_public_hex"`
	ResponderEphemeral     string            `json:"responder_ephemeral_private_hex"`
	PSK                    string            `json:"psk_hex"`
	Message1Payload        string            `json:"message1_payload"`
	Message1               string            `json:"message1_hex"`
	Message2Payload        string            `json:"message2_payload"`
	Message2               string            `json:"message2_hex"`
	Transport              []transportVector `json:"transport"`
}

type transportVector struct {
	Dir        string `json:"dir"` // i2r or r2i
	Plaintext  string `json:"plaintext_hex"`
	Ciphertext string `json:"ciphertext_hex"`
}

type recordsVector struct {
	MaxRecord int            `json:"max_record"`
	Note      string         `json:"note"`
	Cases     []recordCase   `json:"cases"`
	Split     []splitVector  `json:"split"`
	Bad       []badRecordHex `json:"bad"`
}

type recordCase struct {
	Type       int    `json:"type"`
	Data       string `json:"data_hex"`
	Ciphertext string `json:"ciphertext_hex"`
}

type splitVector struct {
	InputLen   int   `json:"input_len"`
	RecordLens []int `json:"record_lens"`
}

type badRecordHex struct {
	Name      string `json:"name"`
	Plaintext string `json:"plaintext_hex"`
}

type yamuxVector struct {
	Note   string       `json:"note"`
	Frames []yamuxFrame `json:"frames"`
}

type yamuxFrame struct {
	Dir   string `json:"dir"` // c2s or s2c
	Name  string `json:"name"`
	Hex   string `json:"hex"`
	Bytes int    `json:"payload_len"`
}

// tap records each write on one side of a pipe.
type tap struct {
	net.Conn
	mu  *sync.Mutex
	dir string
	out *[]yamuxFrame
	buf []byte
}

func (t *tap) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.buf = append(t.buf, p...)
	for len(t.buf) >= 12 {
		n := int(uint32(t.buf[8])<<24 | uint32(t.buf[9])<<16 | uint32(t.buf[10])<<8 | uint32(t.buf[11]))
		if t.buf[1] != 0 { // only data frames carry a payload
			n = 0
		}
		if len(t.buf) < 12+n {
			break
		}
		*t.out = append(*t.out, yamuxFrame{Dir: t.dir, Hex: hex.EncodeToString(t.buf[:12+n]), Bytes: n})
		t.buf = t.buf[12+n:]
	}
	t.mu.Unlock()
	return t.Conn.Write(p)
}

// TestContractVectors pins the bytes of trebi-relay/1. The Rust client
// checks the same files (trebi-ui/mobile/relay/tests/vectors.rs).
func TestContractVectors(t *testing.T) {
	hv, iSend, rRecv := buildHandshake(t)
	writeOrCheck(t, "handshake.v1.json", hv)
	writeOrCheck(t, "records.v1.json", buildRecords(t, iSend, rRecv))

	// yamux framing depends on scheduling; check its shape, write it once.
	yv := buildYamux(t)
	if len(yv.Frames) != 6 {
		t.Fatalf("yamux frames = %d, want 6", len(yv.Frames))
	}
	if *update {
		writeOrCheck(t, "yamux.v1.json", yv)
	} else {
		var want yamuxVector
		raw, err := os.ReadFile(filepath.Join(contractDir, "yamux.v1.json"))
		if err != nil || json.Unmarshal(raw, &want) != nil {
			t.Fatalf("yamux.v1.json: %v", err)
		}
		for i, f := range want.Frames {
			if yv.Frames[i].Hex != f.Hex {
				t.Errorf("yamux frame %d (%s) = %s, want %s", i, f.Name, yv.Frames[i].Hex, f.Hex)
			}
		}
	}
}

type pairLinkCase struct {
	Name string `json:"name"`
	Link string `json:"link"`
	// Release is the result in a release build; Debug in a debug build.
	Release pairLinkWant `json:"release"`
	Debug   pairLinkWant `json:"debug"`
}

type pairLinkWant struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	API       string `json:"api,omitempty"`
	DeviceID  string `json:"device_id,omitempty"`
	DaemonKey string `json:"daemon_key_hex,omitempty"`
	PSK       string `json:"psk_hex,omitempty"`
	Claim     string `json:"claim,omitempty"`
	OfferID   string `json:"offer_id,omitempty"`
	Machine   string `json:"machine,omitempty"`
}

// TestPairLinkFixture checks that the Go link builder makes the valid
// links in pair-link.v1.json byte for byte.
func TestPairLinkFixture(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(contractDir, "pair-link.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []pairLinkCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	built := 0
	for _, c := range doc.Cases {
		w := c.Debug
		if !w.OK || c.Name == "plus in the machine name" {
			continue
		}
		key, _ := hex.DecodeString(w.DaemonKey)
		psk, _ := hex.DecodeString(w.PSK)
		host := w.API[len("https://"):]
		if w.API[:5] == "http:" {
			host = w.API[len("http://"):]
		}
		got := mobile.PairLink(mobile.LinkParts{APIHost: host, DeviceID: w.DeviceID, DaemonKey: key, PSK: psk, Claim: w.Claim, OfferID: w.OfferID, Machine: w.Machine})
		if got != c.Link {
			t.Errorf("%s:\n got %s\nwant %s", c.Name, got, c.Link)
		}
		built++
	}
	if built < 2 {
		t.Fatalf("only %d valid cases", built)
	}
}

func seq(start byte) []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = start + byte(i)
	}
	return b
}

func fixedKey(start byte) noise.DHKey {
	priv := seq(start)
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		panic(err)
	}
	return noise.DHKey{Private: priv, Public: pub}
}

// buildHandshake makes the vector from the fixed keys with the Go code.
func buildHandshake(t *testing.T) (handshakeVector, *noise.CipherState, *noise.CipherState) {
	t.Helper()
	deviceID := "01JA2B3C4D5E6F7G8H9J0K1DEV"
	is, ie, rs, re := fixedKey(0x00), fixedKey(0x40), fixedKey(0x20), fixedKey(0x60)
	psk := seq(0x80)
	hello := `{"v":1,"client_id":"01JA2B3C4D5E6F7G8H9J0K1CLT","offer":"01JA2B3C4D5E6F7G8H9J0K1OFR","name":"Studio iPad","platform":"ios","app_version":"1.0.0"}`
	welcome := `{"v":1,"machine":"Studio Mac","daemon_version":"0.40.0","scope":"full","client_psk":"oKGio6SlpqeoqaqrrK2ur7CxsrO0tba3uLm6u7y9vr8="}`

	ini, err := noise.NewHandshakeState(HandshakeConfig(true, deviceID, is, rs.Public, psk, &ie))
	if err != nil {
		t.Fatal(err)
	}
	res, err := noise.NewHandshakeState(HandshakeConfig(false, deviceID, rs, nil, nil, &re))
	if err != nil {
		t.Fatal(err)
	}
	m1, _, _, err := ini.WriteMessage(nil, []byte(hello))
	if err != nil {
		t.Fatal(err)
	}
	got1, _, _, err := res.ReadMessage(nil, m1)
	if err != nil || string(got1) != hello || !bytes.Equal(res.PeerStatic(), is.Public) {
		t.Fatalf("message 1: %v %q", err, got1)
	}
	if err := res.SetPresharedKey(psk); err != nil {
		t.Fatal(err)
	}
	m2, rRecv, rSend, err := res.WriteMessage(nil, []byte(welcome))
	if err != nil {
		t.Fatal(err)
	}
	got2, iSend, iRecv, err := ini.ReadMessage(nil, m2)
	if err != nil || string(got2) != welcome {
		t.Fatalf("message 2: %v %q", err, got2)
	}

	v := handshakeVector{
		Protocol: "Noise_IKpsk2_25519_ChaChaPoly_BLAKE2s", DeviceID: deviceID,
		PrologueHex:            hex.EncodeToString(Prologue(deviceID)),
		InitiatorStaticPrivate: hex.EncodeToString(is.Private), InitiatorStaticPublic: hex.EncodeToString(is.Public),
		InitiatorEphemeral:     hex.EncodeToString(ie.Private),
		ResponderStaticPrivate: hex.EncodeToString(rs.Private), ResponderStaticPublic: hex.EncodeToString(rs.Public),
		ResponderEphemeral: hex.EncodeToString(re.Private), PSK: hex.EncodeToString(psk),
		Message1Payload: hello, Message1: hex.EncodeToString(m1),
		Message2Payload: welcome, Message2: hex.EncodeToString(m2),
	}
	ready := append([]byte{recordControl}, `{"type":"ready"}`...)
	steps := []struct {
		dir string
		pt  []byte
	}{
		{"i2r", ready},
		{"r2i", ready},
		{"i2r", append([]byte{recordMux}, "GET / HTTP/1.1\r\n"...)},
		{"r2i", append([]byte{recordMux}, "HTTP/1.1 200 OK\r\n"...)},
	}
	for _, st := range steps {
		send, recv := iSend, rRecv
		if st.dir == "r2i" {
			send, recv = rSend, iRecv
		}
		ct, err := send.Encrypt(nil, nil, st.pt)
		if err != nil {
			t.Fatal(err)
		}
		back, err := recv.Decrypt(nil, nil, ct)
		if err != nil || !bytes.Equal(back, st.pt) {
			t.Fatalf("transport %s: %v", st.dir, err)
		}
		v.Transport = append(v.Transport, transportVector{Dir: st.dir, Plaintext: hex.EncodeToString(st.pt), Ciphertext: hex.EncodeToString(ct)})
	}
	return v, iSend, rRecv
}

func buildRecords(t *testing.T, iSend, rRecv *noise.CipherState) recordsVector {
	t.Helper()
	v := recordsVector{
		MaxRecord: MaxRecord,
		Note:      "cases continue the i2r cipher of handshake.v1.json after its transport list, in order",
	}
	for _, c := range []struct {
		typ  byte
		data []byte
	}{
		{recordMux, []byte{0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00}},
		{recordControl, []byte(`{"type":"error","code":"revoked"}`)},
	} {
		ct, err := SealRecord(iSend, c.typ, c.data)
		if err != nil {
			t.Fatal(err)
		}
		typ, data, err := OpenRecord(rRecv, ct)
		if err != nil || typ != c.typ || !bytes.Equal(data, c.data) {
			t.Fatalf("record: %v", err)
		}
		v.Cases = append(v.Cases, recordCase{Type: int(c.typ), Data: hex.EncodeToString(c.data), Ciphertext: hex.EncodeToString(ct)})
	}
	for _, n := range []int{1, MaxRecord - 1, MaxRecord, 40000} {
		sv := splitVector{InputLen: n}
		for _, chunk := range SplitMux(make([]byte, n)) {
			sv.RecordLens = append(sv.RecordLens, len(chunk)+1)
		}
		v.Split = append(v.Split, sv)
	}
	v.Bad = []badRecordHex{{"empty", ""}, {"unknown type", "02"}}
	return v
}

func buildYamux(t *testing.T) yamuxVector {
	t.Helper()
	cRaw, sRaw := net.Pipe()
	var mu sync.Mutex
	var frames []yamuxFrame
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = false
	cfg.LogOutput = io.Discard
	cli, err := yamux.Client(&tap{Conn: cRaw, mu: &mu, dir: "c2s", out: &frames}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := yamux.Server(&tap{Conn: sRaw, mu: &mu, dir: "s2c", out: &frames}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	step := func() { time.Sleep(50 * time.Millisecond) } // keep the frame order fixed
	cs, err := cli.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	step() // the server writes its ACK on Accept, so accept after the data
	ss, err := srv.Accept()
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(ss, buf); err != nil {
		t.Fatal(err)
	}
	step()
	if _, err := ss.Write([]byte("world")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(cs, buf); err != nil {
		t.Fatal(err)
	}
	step()
	_ = cs.Close()
	step()
	_ = ss.Close()
	step()
	_ = cli.Close()
	_ = srv.Close()
	mu.Lock()
	defer mu.Unlock()
	names := []string{"open (window update, SYN)", "data", "accept (window update, ACK)", "data", "close (window update, FIN)", "close (window update, FIN)"}
	var out []yamuxFrame
	for i, f := range frames {
		if i >= len(names) {
			break // a go away frame from Close is not part of the vector
		}
		f.Name = names[i]
		out = append(out, f)
	}
	return yamuxVector{Note: "header: version u8, type u8 (0 data, 1 window update, 2 ping, 3 go away), flags u16 (1 SYN, 2 ACK, 4 FIN, 8 RST), stream id u32, length u32, big endian; the phone is the client", Frames: out}
}

func writeOrCheck(t *testing.T, name string, v any) {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	raw := b.Bytes()
	path := filepath.Join(contractDir, name)
	if *update {
		if err := os.WriteFile(path, raw, 0o644); err != nil { //nolint:gosec // fixture file
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (run go test ./internal/relay -run TestContractVectors -update)", name, err)
	}
	if !bytes.Equal(raw, want) {
		t.Fatalf("%s drifts from the Go code; run with -update and re-vendor", name)
	}
}
