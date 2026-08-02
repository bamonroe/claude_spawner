package main

import (
	"fmt"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	c, _, err := websocket.DefaultDialer.Dial("ws://localhost:8098/ws", nil)
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	defer c.Close()
	start := time.Now()
	for _, a := range os.Args[1:] {
		c.WriteMessage(websocket.TextMessage, []byte(a))
		time.Sleep(300 * time.Millisecond)
	}
	c.SetReadDeadline(time.Now().Add(120 * time.Second))
	for {
		_, m, err := c.ReadMessage()
		if err != nil {
			fmt.Println("read:", err, time.Since(start))
			return
		}
		s := string(m)
		if len(s) > 130 {
			s = s[:130]
		}
		fmt.Printf("%6.1fs %s\n", time.Since(start).Seconds(), s)
	}
}
