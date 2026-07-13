package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bam/claude_spawner/server/internal/tts"
	"github.com/gorilla/websocket"
)

// fakeKokoro stands in for the Kokoro-FastAPI server: it records each request's
// input/voice and answers with the canned audio bytes.
func fakeKokoro(t *testing.T, audio []byte) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var reqs []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("kokoro stub: bad body: %v", err)
		}
		reqs = append(reqs, body)
		w.Header().Set("Content-Type", "audio/opus")
		_, _ = w.Write(audio)
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

// speakHello dials and authenticates, returning the socket and the hello_ok.
func speakHello(t *testing.T, ts *httptest.Server) (*websocket.Conn, map[string]any) {
	t.Helper()
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	return ws, readUntil(t, ws, "hello_ok")
}

// readSpeakStream consumes one speak stream for id: the speak_audio header,
// the binary frames, and the closing speak_end. Returns the codec and audio.
func readSpeakStream(t *testing.T, ws *websocket.Conn, id string) (string, []byte) {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	codec := ""
	var audio bytes.Buffer
	sawHeader := false
	for {
		mt, data, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("reading speak stream: %v", err)
		}
		if mt == websocket.BinaryMessage {
			if !sawHeader {
				t.Fatal("binary frame before the speak_audio header")
			}
			audio.Write(data)
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("bad json frame: %v", err)
		}
		switch m["type"] {
		case "speak_audio":
			if m["id"] != id {
				t.Fatalf("speak_audio id = %v, want %s", m["id"], id)
			}
			codec, _ = m["codec"].(string)
			sawHeader = true
		case "speak_end":
			if m["id"] != id {
				t.Fatalf("speak_end id = %v, want %s", m["id"], id)
			}
			if e, _ := m["error"].(string); e != "" {
				t.Fatalf("speak_end error = %q", e)
			}
			return codec, audio.Bytes()
		}
	}
}

func TestSpeakStreamsAudio(t *testing.T) {
	// Big enough to span several 32 KiB read chunks.
	audio := bytes.Repeat([]byte("opus!"), 20000) // 100 KB
	kokoro, reqs := fakeKokoro(t, audio)
	ts, _, gw := newTestServerGW(t, nil)
	gw.tts = tts.New(kokoro.URL, "af_heart", "opus")

	ws, hello := speakHello(t, ts)
	if hello["tts"] != true {
		t.Errorf("hello_ok tts = %v, want true", hello["tts"])
	}

	send(t, ws, map[string]any{"type": "speak", "id": "s1", "text": "hello there"})
	codec, got := readSpeakStream(t, ws, "s1")
	if codec != "opus" {
		t.Errorf("codec = %q, want opus", codec)
	}
	if !bytes.Equal(got, audio) {
		t.Errorf("audio mismatch: got %d bytes, want %d", len(got), len(audio))
	}

	// Voice override rides through to Kokoro; default fills in when omitted.
	send(t, ws, map[string]any{"type": "speak", "id": "s2", "text": "again", "voice": "bf_emma"})
	readSpeakStream(t, ws, "s2")
	if len(*reqs) != 2 {
		t.Fatalf("kokoro got %d requests, want 2", len(*reqs))
	}
	if v := (*reqs)[0]["voice"]; v != "af_heart" {
		t.Errorf("default voice = %v, want af_heart", v)
	}
	if v := (*reqs)[1]["voice"]; v != "bf_emma" {
		t.Errorf("override voice = %v, want bf_emma", v)
	}
}

func TestSpeakRefusals(t *testing.T) {
	kokoro, _ := fakeKokoro(t, []byte("x"))

	// Empty text is refused without hitting Kokoro.
	ts, _, gw := newTestServerGW(t, nil)
	gw.tts = tts.New(kokoro.URL, "af_heart", "opus")
	ws, _ := speakHello(t, ts)
	send(t, ws, map[string]any{"type": "speak", "id": "e1", "text": "  "})
	m := readUntil(t, ws, "speak_end")
	if m["id"] != "e1" || m["error"] == "" {
		t.Errorf("empty-text speak_end = %v, want id e1 with an error", m)
	}

	// No TTS configured: hello_ok says so, and speak is refused.
	ts2, _ := newTestServer(t, nil)
	ws2, hello := speakHello(t, ts2)
	if hello["tts"] != false {
		t.Errorf("hello_ok tts = %v, want false", hello["tts"])
	}
	send(t, ws2, map[string]any{"type": "speak", "id": "e2", "text": "hi"})
	m = readUntil(t, ws2, "speak_end")
	if m["id"] != "e2" || m["error"] == "" {
		t.Errorf("disabled speak_end = %v, want id e2 with an error", m)
	}
}
