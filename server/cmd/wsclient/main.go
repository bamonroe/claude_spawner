// Command wsclient is a tiny client for manually testing the gateway without the
// Android app. By default it's interactive: each line you type is sent as an
// `utterance`. With -audio it instead streams a WAV file through the voice path
// (wake -> PCM16 frames -> audio_end) to exercise server-side transcription.
//
//	SPAWNER_TOKEN=secret go run ./cmd/wsclient
//	SPAWNER_TOKEN=secret go run ./cmd/wsclient -url ws://host:8080/ws
//	SPAWNER_TOKEN=secret go run ./cmd/wsclient -audio /opt/whisper.cpp/samples/jfk.wav
//
// Try (interactive): "hey buddy, spawn a new session" then a path then "yes".
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func aliasesFromFlag(s string) map[string]string {
	m := map[string]string{}
	if i := strings.IndexByte(s, '='); i > 0 {
		m[strings.TrimSpace(s[:i])] = strings.TrimSpace(s[i+1:])
	}
	return m
}

func main() {
	url := flag.String("url", "ws://localhost:8080/ws", "gateway websocket URL")
	audio := flag.String("audio", "", "WAV file to stream through the voice path instead of stdin")
	handsFree := flag.Bool("handsfree", false, "mark the audio clip as hands-free (VAD-gated) — accumulates into the buffer")
	sttMode := flag.String("sttmode", "", "whisper mode in hello: dynamic | fixed")
	sttModel := flag.String("sttmodel", "", "fixed whisper model in hello: tiny | base | small")
	calibrate := flag.Bool("calibrate", false, "send the clip as an end-token calibration sample")
	aliasFrom := flag.String("alias", "", "one alias \"from=to\" to send in hello")
	whisperURL := flag.String("whisperurl", "", "whisper_url to send in hello")
	whisperModel := flag.String("whispermodel", "", "whisper_model to send in hello")
	flag.Parse()

	token := os.Getenv("SPAWNER_TOKEN")
	if token == "" {
		log.Fatal("set SPAWNER_TOKEN")
	}

	ws, _, err := websocket.DefaultDialer.Dial(*url, nil)
	if err != nil {
		log.Fatalf("dial %s: %v", *url, err)
	}
	defer ws.Close()

	if err := ws.WriteJSON(map[string]any{
		"type": "hello", "token": token, "stt_mode": *sttMode, "stt_model": *sttModel, "aliases": aliasesFromFlag(*aliasFrom), "whisper_url": *whisperURL, "whisper_model": *whisperModel,
	}); err != nil {
		log.Fatal(err)
	}

	go func() {
		for {
			var m map[string]any
			if err := ws.ReadJSON(&m); err != nil {
				fmt.Println("\n[connection closed]")
				os.Exit(0)
			}
			printMsg(m)
		}
	}()

	if *audio != "" {
		streamWAV(ws, *audio, *handsFree, *calibrate)
		time.Sleep(20 * time.Second) // wait for transcript + any dialog response
		return
	}

	fmt.Println("connected. type an utterance and press enter (Ctrl-D to quit).")
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			// A line beginning with '{' is sent verbatim (raw protocol frame, for
			// testing e.g. history requests); otherwise it's a plain utterance.
			var err error
			if strings.HasPrefix(strings.TrimSpace(line), "{") {
				err = ws.WriteMessage(websocket.TextMessage, []byte(line))
			} else {
				err = ws.WriteJSON(map[string]any{"type": "utterance", "text": line})
			}
			if err != nil {
				log.Printf("send: %v", err)
				return
			}
		}
	}
}

// streamWAV sends wake, the file's PCM16 payload (WAV header stripped) in chunks,
// then audio_end. Assumes a canonical 44-byte header (PCM16/16kHz/mono).
func streamWAV(ws *websocket.Conn, path string, handsFree, calibrate bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read %s: %v", path, err)
	}
	if len(data) < 44 || string(data[0:4]) != "RIFF" {
		log.Fatalf("%s is not a WAV file", path)
	}
	pcm := data[44:] // skip canonical header
	fmt.Printf("streaming %s (%d bytes PCM)\n", path, len(pcm))

	if err := ws.WriteJSON(map[string]any{"type": "wake", "hands_free": handsFree, "calibrate": calibrate}); err != nil {
		log.Fatal(err)
	}
	const chunk = 3200 // 100ms @ 16kHz mono PCM16
	for i := 0; i < len(pcm); i += chunk {
		end := i + chunk
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := ws.WriteMessage(websocket.BinaryMessage, pcm[i:end]); err != nil {
			log.Fatal(err)
		}
	}
	if err := ws.WriteJSON(map[string]any{"type": "audio_end"}); err != nil {
		log.Fatal(err)
	}
}

func printMsg(m map[string]any) {
	switch m["type"] {
	case "say":
		fmt.Printf("  🔊 %v\n", m["text"])
	case "output":
		fmt.Printf("  💬 %v\n", m["text"])
	case "transcript":
		fmt.Printf("  📝 %v\n", m["text"])
	case "dialog":
		fmt.Printf("  [dialog:%v]\n", m["state"])
	case "error":
		fmt.Printf("  ⚠️  %v: %v\n", m["code"], m["message"])
	default:
		b, _ := json.Marshal(m)
		fmt.Printf("  %s\n", b)
	}
}
