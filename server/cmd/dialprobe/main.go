package main

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func main() {
	key, err := os.ReadFile("/data/claude_spawner/deploy/state/ssh_keys/id_bazzite")
	if err != nil {
		fmt.Println("key:", err)
		return
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		fmt.Println("parse:", err)
		return
	}
	cb, err := knownhosts.New("/data/claude_spawner/deploy/state/known_hosts")
	if err != nil {
		fmt.Println("kh:", err)
		return
	}
	cfg := &ssh.ClientConfig{
		User:            "bam",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: cb,
		Timeout:         15 * time.Second,
	}
	for i := 0; i < 3; i++ {
		t := time.Now()
		c, err := ssh.Dial("tcp", "localhost:22", cfg)
		dialed := time.Since(t)
		if err != nil {
			fmt.Printf("dial %d failed after %v: %v\n", i, dialed, err)
			continue
		}
		t2 := time.Now()
		s, err := c.NewSession()
		if err != nil {
			fmt.Println("session:", err)
			c.Close()
			continue
		}
		out, err := s.Output("echo hi")
		fmt.Printf("dial=%v cmd=%v out=%q err=%v\n", dialed, time.Since(t2), out, err)
		s.Close()
		c.Close()
	}
}
